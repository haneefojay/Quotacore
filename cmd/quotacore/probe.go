package main

import (
	"context"
	"database/sql"
	"log/slog"
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
}

// probe watches the two dependencies until ctx ends: the store's clock every
// second, which is readiness's store term, datastore_up and the clock skew;
// its memory and Postgres every ten. A change of the store's state is one log
// line, so an outage is two lines and not one a second.
func probe(ctx context.Context, o probeOptions) {
	every, slow := o.every, o.slow
	if every <= 0 {
		every = storeProbeEvery
	}
	if slow <= 0 {
		slow = memoryProbeEvery
	}
	warnedUnbounded := false
	up := -1
	var lastSlow time.Time
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
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
		if now != up && ctx.Err() == nil {
			switch {
			case now == 1 && up != -1:
				o.log.Info("the data store answers again")
			case now == 0:
				o.log.Warn("the data store did not answer the probe within the timeout; not ready", "error", err)
			}
			up = now
		}

		if time.Since(lastSlow) >= slow {
			lastSlow = time.Now()
			mctx, cancel := context.WithTimeout(ctx, o.pgWait)
			if used, limit, err := o.data.Memory(mctx); err == nil {
				switch {
				case limit > 0:
					o.deps.MemoryUsedRatio.Set(float64(used) / float64(limit))
				default:
					o.deps.MemoryUsedRatio.Set(0)
					if !warnedUnbounded {
						warnedUnbounded = true
						o.log.Warn("the data store has no maxmemory, so its memory ratio is reported as 0 and nothing bounds it (DR-048)")
					}
				}
			}
			pgUp := 0.0
			if o.db.PingContext(mctx) == nil {
				pgUp = 1
			}
			o.deps.ControlPlaneUp.Set(pgUp)
			cancel()
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
