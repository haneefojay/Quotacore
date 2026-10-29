package main

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/dataplane"
)

// rolloversOf is the cycle_rolled_over events handed to the sink for one
// tenant.
func (s *stack) rolloversOf(tn tenant) []dataplane.Event {
	var out []dataplane.Event
	for _, e := range s.events.since(0) {
		if e.TenantID == tn.ID && e.Type == "cycle_rolled_over" {
			out = append(out, e)
		}
	}
	return out
}

// TestTheWorkerAndTheRequestPathRollABoundaryOnce is T-05's race half and
// mvp-scope section 3's "the request path and the worker, both racing on the
// same boundary, produce the same result regardless of the interleaving",
// with the reset worker of Q-13. Each iteration seeds a tenant whose hourly
// cycle closed at the top of this hour with 60 of 100 spent, caches it with a
// balance read (which rolls nothing), and lets the worker's scan and a consume
// of 10 meet the boundary in one of three orders: the worker first, the
// request first, or both at once. Every order ends identically: one
// transition, one cycle_rolled_over event of +60, and a balance of 90 in the
// current cycle. The worker's late scan, and an out-of-order one aimed at the
// previous hour, change nothing.
func TestTheWorkerAndTheRequestPathRollABoundaryOnce(t *testing.T) {
	s := newStack(t)
	const feature = "wk-calls"
	ctx := context.Background()
	for _, order := range []string{"worker first", "request first", "together"} {
		for i := range 15 {
			at := time.Now()
			tn := s.tenant(externalID("wk"), "UTC", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				ent{feature: feature, limit: 100, interval: "hourly"})
			w := s.currentWindow(tn, feature, at)
			prevStart, prevEnd := w.Start.Add(-time.Hour), w.Start
			s.seed(tn, feature, stored{Balance: 40, Limit: 100, Index: w.Index - 1, Start: prevStart, End: &prevEnd})
			// The read caches the tenant. Forty-five new tenants in a few
			// seconds outrun the snapshot's miss limiter, which answers 503
			// with a retry_after_ms (DR-039), so the read is retried.
			until(t, "", func() response {
				return s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External, s.runtime, "")
			})

			scan := func() { s.app.Worker.Sweep(ctx, w.Start.Add(-time.Millisecond), time.Now()) }
			consume := func() response { return s.consume(s.runtime, feature, 10, tn.External, newKey()) }
			var r response
			switch order {
			case "worker first":
				scan()
				if got := s.balance(tn, feature); got != 100 {
					t.Fatalf("%s %d: the scan left the balance at %d, want the cycle rolled to 100", order, i, got)
				}
				r = consume()
			case "request first":
				r = consume()
				scan()
			default:
				var wg sync.WaitGroup
				wg.Add(1)
				go func() { defer wg.Done(); scan() }()
				r = consume()
				wg.Wait()
			}
			if s.currentWindow(tn, feature, time.Now()).Index != w.Index {
				t.Skip("the hour turned during the iteration; the assertions are about one boundary")
			}
			mustStatus(t, r, http.StatusOK)
			if got := s.balance(tn, feature); got != 90 {
				t.Fatalf("%s %d: balance %d, want 90", order, i, got)
			}
			rolls := s.rolloversOf(tn)
			if len(rolls) != 1 || rolls[0].Delta != 60 || rolls[0].CycleIndex != w.Index || rolls[0].Source != "system" {
				t.Fatalf("%s %d: rollovers %+v, want one of +60 into index %d", order, i, rolls, w.Index)
			}
			// A scan behind the store, as a delayed or duplicated worker
			// would run it, is stale and moves nothing.
			before := s.state(tn, feature)
			snap := s.app.Snapshot.Cached()
			for _, c := range snap {
				if c.ID != tn.ID {
					continue
				}
				rolled, err := s.app.Service.Advance(ctx, c, feature, prevStart.Add(time.Minute))
				if err != nil || rolled {
					t.Fatalf("%s %d: an out-of-order advance: rolled %v, %v", order, i, rolled, err)
				}
			}
			after := s.state(tn, feature)
			if after.Hash["balance"] != before.Hash["balance"] || after.Hash["cycle_index"] != before.Hash["cycle_index"] || len(s.rolloversOf(tn)) != 1 {
				t.Fatalf("%s %d: an out-of-order advance changed %v into %v", order, i, before.Hash, after.Hash)
			}
		}
	}
}

// TestTheWorkerRollsAnIdleTenantAtItsBoundary is Q-13's purpose: with the
// worker on, a cached tenant that sends nothing is rolled at its boundary,
// so the window an admin reads is the current one, and the ledger's opening
// row is written without a request. The event belongs to no request.
func TestTheWorkerRollsAnIdleTenantAtItsBoundary(t *testing.T) {
	s := newStack(t, func(o *stackOptions) {
		o.env["QUOTACORE_WORKER_ENABLED"] = "true"
		o.env["QUOTACORE_RESET_SCAN_INTERVAL"] = "1s"
	})
	const feature = "wk-idle"
	// Anchored so that its hourly boundary is two seconds away, a moment the
	// worker's one-second scans will cross.
	anchor := time.Now().Add(2 * time.Second).Add(-5 * time.Hour).Truncate(time.Second)
	tn := s.tenant(externalID("wk"), "UTC", anchor, ent{feature: feature, limit: 100, interval: "hourly"})
	s.seedCurrent(tn, feature, 25)
	mustStatus(t, s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External, s.runtime, ""), http.StatusOK)
	deadline := time.Now().Add(5 * time.Second)
	for s.balance(tn, feature) != 100 {
		if time.Now().After(deadline) {
			t.Fatalf("the worker did not roll an idle cached tenant within five scans; balance %d", s.balance(tn, feature))
		}
		time.Sleep(50 * time.Millisecond)
	}
	rolls := s.rolloversOf(tn)
	if len(rolls) != 1 || rolls[0].RequestID != "" || rolls[0].Delta != 75 {
		t.Fatalf("rollovers %+v, want one of +75 belonging to no request", rolls)
	}
}
