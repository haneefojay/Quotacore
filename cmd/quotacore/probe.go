package main

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quotacore/quotacore/internal/observability"
	"github.com/quotacore/quotacore/internal/store"
)

// The prober's intervals (observability.md section 1.3, IP-13 S1). They are
// constants rather than configuration: readiness must notice a lost store
// within two probes, and the memory and Postgres reads are cheap enough at
// ten seconds that no operator has a reason to change them.
const (
	storeProbeEvery  = time.Second
	memoryProbeEvery = 10 * time.Second
)

type probeOptions struct {
	data    *store.Store
	db      *sql.DB
	deps    *observability.Dependencies
	ready   *readinessState
	log     *slog.Logger
	timeout time.Duration // QUOTACORE_DATASCRIPT_TIMEOUT
	pgWait  time.Duration // QUOTACORE_CONTROLPLANE_TIMEOUT
	every   time.Duration // the store probe; zero is storeProbeEvery
	slow    time.Duration // memory and Postgres; zero is memoryProbeEvery
	warned  *atomic.Bool  // the unbounded-memory warning, given once
	policy  *policyState  // the eviction-policy term (IP-15 D6)
}

// policyState remembers the store's last-read maxmemory-policy, so a change
// is one log line and the store probe knows whether it has been read at all.
type policyState struct {
	mu    sync.Mutex
	known bool
	last  string
}

// probe watches the two dependencies until ctx ends: the store's clock every
// second, which is readiness's store term, datastore_up and the clock skew;
// its memory and Postgres every ten, on their own loop, so a slow Postgres
// never delays the store's verdict. A change of the store's state is one log
// line, so an outage is two lines and not one a second.
func probe(ctx context.Context, o probeOptions) {
	every, slow := o.every, o.slow
	if every <= 0 {
		every = storeProbeEvery
	}
	if slow <= 0 {
		slow = memoryProbeEvery
	}
	go tick(ctx, slow, func() { o.slowProbe(ctx) })
	up := -1
	tick(ctx, every, func() {
		pctx, cancel := context.WithTimeout(ctx, o.timeout)
		skew, err := o.data.Clock(pctx)
		cancel()
		now := 0
		if err == nil {
			now = 1
			o.deps.ClockSkew.Set(skew.Seconds())
		}
		o.deps.DataStoreUp.Set(float64(now))
		o.ready.Set(termDataStore, now == 1)
		// A store that was down at start has never had its policy read; read
		// it as soon as it answers rather than on the next slow tick, so
		// readiness is not held back ten seconds by a term nobody checked.
		if now == 1 && !o.policy.isKnown() {
			o.memory(ctx)
		}
		if now == up || ctx.Err() != nil {
			return
		}
		switch {
		case now == 1 && up != -1:
			o.log.Info("the data store answers again")
		case now == 0:
			o.log.Warn("the data store did not answer the probe within the timeout; not ready", "error", err)
		}
		up = now
	})
}

// slowProbe reads the store's memory and pings Postgres.
func (o probeOptions) slowProbe(ctx context.Context) {
	o.memory(ctx)
	pctx, cancel := context.WithTimeout(ctx, o.pgWait)
	defer cancel()
	up := 0.0
	if o.db.PingContext(pctx) == nil {
		up = 1
	}
	o.deps.ControlPlaneUp.Set(up)
}

// memory reads INFO memory once: the used-memory ratio, and the eviction
// policy, which is a readiness term (DR-048). A store that may evict can
// discard an idempotency record inside its window, so an instance against one
// is not ready; it is not stopped, because a process that exits on a store
// setting turns one CONFIG SET into a restart loop (IP-15 D6).
func (o probeOptions) memory(ctx context.Context) {
	mctx, cancel := context.WithTimeout(ctx, o.timeout)
	m, err := o.data.Memory(mctx)
	cancel()
	if err != nil {
		return
	}
	ratio := 0.0
	if m.Limit > 0 {
		ratio = float64(m.Used) / float64(m.Limit)
	} else if !o.warned.Swap(true) {
		o.log.Warn("the data store has no maxmemory, so its memory ratio is reported as 0 and nothing bounds it (DR-048)")
	}
	o.deps.MemoryUsedRatio.Set(ratio)
	ok := m.Policy == store.EvictionPolicy
	o.ready.Set(termNoEviction, ok)
	if o.policy.changed(m.Policy) {
		if ok {
			o.log.Info("the data store refuses to evict again", "policy", m.Policy)
		} else {
			o.log.Error("the data store's maxmemory-policy is not noeviction, so it may discard an idempotency record inside its window; not ready (DR-048)", "policy", m.Policy)
		}
	}
}

func (p *policyState) isKnown() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.known
}

// changed records policy and reports whether it differs from the last one
// read; the first read is a change only when it is not noeviction, so a
// healthy start logs nothing.
func (p *policyState) changed(policy string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	was, known := p.last, p.known
	p.last, p.known = policy, true
	if !known {
		return policy != store.EvictionPolicy
	}
	return was != policy
}

// tick runs f now and every interval until ctx ends.
func tick(ctx context.Context, every time.Duration, f func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		f()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
