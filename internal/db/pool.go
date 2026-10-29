package db

import (
	"context"
	"database/sql"
	"time"

	"github.com/quotacore/quotacore/internal/observability"
)

// StartPoolObserver is the instrumented-pool half of IP-04's ownership of the
// control-plane connection. It samples the pool's wait statistics into
// quotacore_dbpool_wait_seconds at a fixed interval. The metric's raison d'être
// (observability.md section 1.4) is that a rising wait is the mechanism by
// which a control-plane problem would reach the data plane — the thing to watch
// precisely because that coupling is supposed to be impossible. Sampling a
// counter delta, rather than timing individual acquires, costs nothing during
// the requests themselves.
//
// The observer never changes the pool's behaviour: it reads, never modifies.
// A nil Metrics or pool starts nothing.
func StartPoolObserver(ctx context.Context, pool *sql.DB, m *observability.Metrics, every time.Duration) {
	if m == nil || pool == nil {
		return
	}
	if every <= 0 {
		every = 10 * time.Second
	}
	go func() {
		last := pool.Stats().WaitDuration
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s := pool.Stats()
				if delta := s.WaitDuration - last; delta > 0 {
					m.DBPoolWait.Observe(delta.Seconds())
				}
				last = s.WaitDuration
			}
		}
	}()
}
