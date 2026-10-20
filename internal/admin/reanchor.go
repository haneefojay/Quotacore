package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/snapshot"
	"github.com/quotacore/quotacore/internal/store"
)

// The re-anchor drain (IP-10, D2). A plan change or an apply-now commits the
// new anchor, the new pins and a pending re-anchor in one Postgres
// transaction; the balance hashes are rewritten after, by the request inline
// and by every instance on start and on each refresh tick, and the pending row
// is deleted in the transaction that finishes the rewrite. Until a tenant is
// drained the data plane serves its old state, never a fresh allowance, and a
// rewrite that is retried or run twice at once changes a tenant once.

// drainChunk is how many tenants one background pass finishes before it logs
// and looks again (ADR-0006's chunked job with progress).
const drainChunk = 100

// maxRewriteAttempts bounds the compare-and-set loop on one hash. A request
// that rolls the hash between the read and the write moves it once; a hash
// that moves on every attempt is a fault worth reporting rather than chasing.
const maxRewriteAttempts = 5

// rewrite is the DrainFunc: every feature the tenant now holds, rewritten to
// the window its new anchor puts it in at the new plan's limit, and the
// discarded balance handed to the ledger as a plan_changed event (DR-013).
// An archived feature is skipped, as provisioning skips it.
func (s *Service) rewrite(ctx context.Context, p Reanchor, lt *snapshot.LoadedTenant) error {
	t := snapshot.NewTenant(lt)
	loc, err := time.LoadLocation(t.Timezone)
	if err != nil {
		return fmt.Errorf("load zone: %w", err)
	}
	now := s.now()
	features := make([]string, 0, len(t.Pins))
	for f := range t.Pins {
		features = append(features, f)
	}
	sort.Strings(features)
	var events []dataplane.Event
	for _, f := range features {
		e, held := t.Entitlement(f, now)
		if !held || e.Archived {
			continue
		}
		out, err := s.rewriteOne(ctx, t.ID, f, t.Anchor, e, loc, now)
		if err != nil {
			return err
		}
		if out == nil {
			continue
		}
		if delta, known := out.Delta(); known && delta != 0 {
			events = append(events, dataplane.Event{
				RequestID: p.RequestID, TenantID: t.ID, FeatureKey: f, Type: "plan_changed", Source: "admin",
				Amount: abs(delta), Delta: delta, Balance: out.Balance, CycleIndex: out.Index, CycleStart: out.Start,
				OccurredAt: now,
			})
		}
	}
	// The events describe hashes already rewritten; the caller's commit only
	// deletes the pending row, and a rollback after this point leaves a row
	// whose next drain finds every hash already re-anchored and hands over
	// nothing twice.
	for _, ev := range events {
		s.events.Enqueue(ev)
	}
	return nil
}

// rewriteOne re-anchors one hash, unless it is already one of the new
// anchor's windows. It returns nil when there was nothing to do.
func (s *Service) rewriteOne(ctx context.Context, tenantID, feature string, anchor time.Time, e snapshot.Entitlement,
	loc *time.Location, now time.Time) (*script.AdminOutcome, error) {
	win := cycle.CurrentWindow(anchor, e.Interval, loc, now)
	for range maxRewriteAttempts {
		var expect script.Expectation
		stored, err := s.data.ReadBalances(ctx, tenantID, []string{feature})
		switch {
		case errors.Is(err, store.ErrCorruptBalance):
			expect.Any = true
		case err != nil:
			return nil, errDataStore{err: err}
		case !stored[0].Present:
			expect.Absent = true
		case underAnchor(stored[0], anchor, e.Interval, loc):
			return nil, nil
		default:
			expect.Index, expect.Start = stored[0].Index, stored[0].Start
		}
		out, err := s.scripts.Reanchor(ctx, script.Rewrite{
			TenantID: tenantID, FeatureKey: feature, Window: win, Limit: e.Limit, Expect: expect,
		})
		if errors.Is(err, script.ErrMoved) {
			continue
		}
		if err != nil {
			return nil, errDataStore{err: err}
		}
		return &out, nil
	}
	return nil, errDataStore{err: fmt.Errorf("the balance of %s kept moving during its re-anchor", feature)}
}

// underAnchor reports whether a stored window is one of the anchor's: the
// boundary the cycle engine puts at the stored index is the stored start. A
// window written under the previous anchor is not, which is how a rewrite
// that already happened, or a roll the data plane made under the new anchor
// since, is told apart from one still to do. Windows are stored in
// milliseconds, and the anchor is compared at that precision.
func underAnchor(st store.Stored, anchor time.Time, iv cycle.Interval, loc *time.Location) bool {
	if iv == cycle.Never {
		return st.Index == 0 && st.End == nil && st.Start.UnixMilli() == anchor.UnixMilli()
	}
	if st.End == nil {
		return false
	}
	return st.Start.UnixMilli() == cycle.Add(anchor, iv, loc, st.Index).UnixMilli() &&
		st.End.UnixMilli() == cycle.Add(anchor, iv, loc, st.Index+1).UnixMilli()
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// drainInline finishes the listed tenants' re-anchors within the request's
// time, leaving any it does not reach to the background drain. The request
// has committed, so a failure here is logged and never answered (D2).
func (s *Service) drainInline(r *http.Request, tenantIDs []string) {
	if len(tenantIDs) == 0 {
		return
	}
	deadline, ok := r.Context().Deadline()
	if !ok {
		deadline = time.Now().Add(time.Second)
	}
	ctx, cancel := context.WithDeadline(context.WithoutCancel(r.Context()), deadline.Add(-100*time.Millisecond))
	defer cancel()
	n, err := s.store.DrainReanchors(ctx, tenantIDs, len(tenantIDs), s.rewrite)
	if err != nil {
		s.log.Warn("could not finish a re-anchor inline; the background drain will",
			"drained", n, "pending", len(tenantIDs)-n, "error", err)
	}
}

// RunDrain is the background drain: once at start and then on every tick, it
// finishes pending re-anchors a chunk at a time until none is left, logging
// each chunk's count and duration. It returns when ctx is done.
func (s *Service) RunDrain(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.DrainOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// DrainOnce finishes every pending re-anchor it can reach now, a chunk at a
// time, and reports how many it finished.
func (s *Service) DrainOnce(ctx context.Context) int {
	if s.readOnly() {
		// A re-anchor writes balances, which read-only mode forbids; the
		// drain resumes when the instance is back in normal mode (IP-11 D5).
		return 0
	}
	total := 0
	for ctx.Err() == nil {
		began := time.Now()
		n, err := s.store.DrainReanchors(ctx, nil, drainChunk, s.rewrite)
		total += n
		if n > 0 || err != nil {
			s.log.Info("drained pending re-anchors", "drained", n, "duration_ms", time.Since(began).Milliseconds())
		}
		if err != nil {
			s.log.Warn("a re-anchor could not be finished; it stays pending", "error", err)
			return total
		}
		if n < drainChunk {
			return total
		}
	}
	return total
}
