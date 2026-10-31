// Package worker is the reset worker (ADR-0003 rule 5, state-machines.md
// section 11, Q-13): a loop that, every scan interval, applies the cycle
// transition to each tenant-feature whose boundary has passed since its last
// scan, so a tenant's window and its ledger's cycle opening are current at the
// boundary rather than at the tenant's next request.
//
// It is an optimisation and nothing depends on it (INV-WK1): the request path
// repairs any tenant it touches, the transition is monotonic, and a scan that
// races a request on a boundary loses harmlessly with a stale verdict. It has
// no database access and no credential beyond the data plane's own path
// (INV-WK2): it reads tenants from the snapshot the data plane already holds
// and calls the data plane's Advance, which runs the one transition. Its source
// check holds it to that.
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/snapshot"
)

// Tenants is where the worker finds the tenants to scan: the snapshot's
// cached tenants, which are the ones this instance serves.
type Tenants interface {
	Cached() []*snapshot.Tenant
}

// Plane is the data plane's transition.
type Plane interface {
	Advance(ctx context.Context, t *snapshot.Tenant, featureKey string, at time.Time) (bool, error)
}

// Options configure a worker.
type Options struct {
	Tenants Tenants
	Plane   Plane
	// Interval is QUOTACORE_RESET_SCAN_INTERVAL, and Batch is
	// QUOTACORE_RESET_BATCH_SIZE: how many tenants a scan visits before it
	// checks whether it should stop.
	Interval time.Duration
	Batch    int
	Logger   *slog.Logger
	// Paused reports read-only mode, in which the worker writes nothing.
	Paused func() bool
	Now    func() time.Time
	// Zone loads a tenant's time zone.
	Zone func(name string) (*time.Location, error)
}

// Worker is the reset worker. Run it in its own goroutine.
type Worker struct {
	o Options
}

// New builds a worker.
func New(o Options) *Worker {
	if o.Interval <= 0 {
		o.Interval = time.Minute
	}
	if o.Batch <= 0 {
		o.Batch = 500
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Paused == nil {
		o.Paused = func() bool { return false }
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Zone == nil {
		o.Zone = time.LoadLocation
	}
	return &Worker{o: o}
}

// Tally is one sweep's count.
type Tally struct {
	Tenants int // tenants visited
	Due     int // tenant-features whose boundary passed in the scan's span
	Rolled  int // of those, the ones this scan rolled
	Failed  int // the ones the data plane could not answer
}

// Run scans every interval until ctx ends. The first scan looks back one
// interval, so a boundary that passed while the process started is covered
// once; one that passed while the process was down is the request path's.
func (w *Worker) Run(ctx context.Context) {
	prev := w.o.Now().Add(-w.o.Interval)
	t := time.NewTicker(w.o.Interval)
	defer t.Stop()
	for {
		now := w.o.Now()
		// Read once, so a pause that begins mid-scan cannot advance the span
		// past boundaries the scan never ran for, or skip one it did.
		paused := w.o.Paused()
		if !paused {
			began := time.Now()
			sc := w.Sweep(ctx, prev, now)
			level := slog.LevelDebug
			if sc.Failed > 0 {
				level = slog.LevelWarn
			}
			w.o.Logger.Log(ctx, level, "reset scan", "job", "worker", "tenants", sc.Tenants, "due", sc.Due,
				"rolled", sc.Rolled, "failed", sc.Failed, "duration_ms", time.Since(began).Milliseconds())
		}
		// A paused scan keeps its span, so the boundaries it skipped are
		// covered when it resumes.
		if !paused {
			prev = now
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep applies the transition to every cached tenant-feature whose cycle
// started in (prev, now]: the boundaries that passed since the last scan. The
// window is computed once per tenant and cadence, because every feature a
// tenant holds at one cadence shares its anchor and zone. A failure is
// counted and the scan goes on (state-machines.md section 11); it never
// retries in a loop, because the next request or scan will.
func (w *Worker) Sweep(ctx context.Context, prev, now time.Time) Tally {
	var sc Tally
	tenants := w.o.Tenants.Cached()
	for i, t := range tenants {
		if i > 0 && i%w.o.Batch == 0 && ctx.Err() != nil {
			return sc
		}
		sc.Tenants++
		if t.State == "suspended" || t.State == "deleted" {
			continue
		}
		loc, err := w.o.Zone(t.Timezone)
		if err != nil {
			sc.Failed++
			continue
		}
		starts := map[cycle.Interval]time.Time{}
		for feature, e := range t.Features(now) {
			// A boolean or rolling entitlement has no cycle to roll (DR-050,
			// DR-052).
			if !e.Anchored() || e.Interval == cycle.Never || e.Archived {
				continue
			}
			start, seen := starts[e.Interval]
			if !seen {
				start = cycle.CurrentWindow(t.Anchor, e.Interval, loc, now).Start
				starts[e.Interval] = start
			}
			if !start.After(prev) || start.After(now) {
				continue
			}
			sc.Due++
			rolled, err := w.o.Plane.Advance(ctx, t, feature, now)
			switch {
			case err != nil:
				sc.Failed++
			case rolled:
				sc.Rolled++
			}
		}
	}
	return sc
}
