package main

import (
	"context"
	"database/sql"
	"log/slog"
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
	mctx, cancel := context.WithTimeout(ctx, o.timeout)
	used, limit, err := o.data.Memory(mctx)
	cancel()
	if err == nil {
		ratio := 0.0
		if limit > 0 {
			ratio = float64(used) / float64(limit)
		} else if !o.warned.Swap(true) {
			o.log.Warn("the data store has no maxmemory, so its memory ratio is reported as 0 and nothing bounds it (DR-048)")
		}
		o.deps.MemoryUsedRatio.Set(ratio)
	}
	pctx, cancel := context.WithTimeout(ctx, o.pgWait)
	defer cancel()
	up := 0.0
	if o.db.PingContext(pctx) == nil {
		up = 1
	}
	o.deps.ControlPlaneUp.Set(up)
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
