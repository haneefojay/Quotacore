package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/db"
	"github.com/quotacore/quotacore/internal/ledger"
	"github.com/quotacore/quotacore/internal/reconcile"
	"github.com/quotacore/quotacore/internal/store"
)

// pass runs one reconciliation pass the way the service's own loop does,
// after the ledger has written what is queued and the rows have settled.
func (s *stack) pass() reconcile.Ran {
	s.t.Helper()
	s.flush()
	time.Sleep(s.settle + 50*time.Millisecond)
	r, err := s.app.Pass.Once(context.Background())
	if err != nil {
		s.t.Fatalf("the reconciliation pass: %v", err)
	}
	if !r.Ran {
		s.t.Fatal("the reconciliation pass did not run")
	}
	return r
}

// findings is every reconcile_findings row.
func (s *stack) findings() []db.StoredFinding {
	s.t.Helper()
	f, err := db.NewReconciler(s.seedDB).Findings(context.Background(), time.Time{}, 1000)
	if err != nil {
		s.t.Fatal(err)
	}
	return f
}

func findingKinds(fs []db.StoredFinding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Kind)
	}
	return out
}

// dropping loses every event whose idempotency key starts with drop-, as a
// writer that crashed holding it would: written nowhere and counted nowhere.
type dropping struct{ ledger.Appender }

func (d dropping) Append(ctx context.Context, events []dataplane.Event) (ledger.Appended, error) {
	kept := events[:0:0]
	for _, e := range events {
		if !strings.HasPrefix(e.ClientKey, "drop-") {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		return ledger.Appended{}, nil
	}
	return d.Appender.Append(ctx, kept)
}

// duplicate makes the shape a store loss inside the window leaves (FS-22):
// a consume applied, its idempotency record lost, and the identical request
// retried, so the store applies it twice and the ledger refuses the second.
func (s *stack) duplicate(tn tenant, feature string, amount int64, key string) {
	s.t.Helper()
	mustStatus(s.t, s.consume(s.runtime, feature, amount, tn.External, key), http.StatusOK)
	idem, err := store.IdempotencyKey(tn.ID, key)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := s.rdb.Del(context.Background(), idem).Err(); err != nil {
		s.t.Fatal(err)
	}
	mustStatus(s.t, s.consume(s.runtime, feature, amount, tn.External, key), http.StatusOK)
	s.flush()
}

// TestAMutationWithNoLedgerEventIsDetected is IP-11 DoD 2: a consume the store
// applied and whose event never reached the ledger leaves the hash and the
// ledger disagreeing, which the pass confirms, records once, and counts once.
func TestAMutationWithNoLedgerEventIsDetected(t *testing.T) {
	s := newStack(t, func(o *stackOptions) {
		o.appender = func(l *db.Ledger) ledger.Appender { return dropping{l} }
	})
	f, _, tn := s.provisioned(100)
	mustStatus(t, s.consume(s.runtime, f, 10, tn.External, newKey()), http.StatusOK)
	mustStatus(t, s.consume(s.runtime, f, 5, tn.External, "drop-"+newKey()), http.StatusOK)
	r := s.pass()
	if len(r.Confirmed) != 1 || r.Confirmed[0].Kind != reconcile.KindHashBalance {
		t.Fatalf("the pass confirmed %+v, want one hash_balance", r.Confirmed)
	}
	if got := s.counter("quotacore_event_reconcile_mismatch_total"); got != 1 {
		t.Errorf("reconcile_mismatch_total = %v, want 1", got)
	}
	fs := s.findings()
	if len(fs) != 1 || fs[0].Kind != reconcile.KindHashBalance || *fs[0].TenantID != tn.ID || *fs[0].FeatureKey != f ||
		*fs[0].Expected != 90 || *fs[0].Actual != 85 {
		t.Fatalf("findings %s", mustJSON(fs))
	}
	if s.balance(tn, f) != 85 {
		t.Errorf("the pass moved the balance to %d", s.balance(tn, f))
	}
	// The next consume makes the tenant-feature change again; the pass reads
	// it, sees the same disagreement in a new place, and records that one
	// alone.
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
	s.pass()
	if got := s.counter("quotacore_event_reconcile_mismatch_total"); got != 2 {
		t.Errorf("reconcile_mismatch_total after a second pass = %v, want 2: %v", got, findingKinds(s.findings()))
	}
	// A pass with nothing new reads nothing and records nothing.
	if r := s.pass(); r.Verified != 0 || len(r.Confirmed) != 0 {
		t.Errorf("an idle pass verified %d and confirmed %+v", r.Verified, r.Confirmed)
	}
	if got := s.counter("quotacore_event_reconcile_mismatch_total"); got != 2 {
		t.Errorf("reconcile_mismatch_total after an idle pass = %v, want 2", got)
	}
}

// TestDetectionNeverPrevents is the rest of DoD 2 and IP-06 item 7: an event
// still in a stalled writer's queue is never reported, and the pass running
// alongside T-02's fifty deliveries neither stops the store charging once nor
// touches a balance.
func TestDetectionNeverPrevents(t *testing.T) {
	st := &gate{}
	s := newStack(t, func(o *stackOptions) {
		o.appender = func(l *db.Ledger) ledger.Appender { st.Appender = l; return st }
		o.settle = 600 * time.Millisecond
	})
	f, _, tn := s.provisioned(1000)
	mustStatus(t, s.consume(s.runtime, f, 10, tn.External, newKey()), http.StatusOK)
	s.flush()
	time.Sleep(s.settle + 50*time.Millisecond)
	// The writer stalls with the next event in hand, the pass reads a hash
	// ahead of the ledger, and the writer is released inside the settle
	// period: the second read finds them agreeing.
	st.close()
	mustStatus(t, s.consume(s.runtime, f, 7, tn.External, newKey()), http.StatusOK)
	time.AfterFunc(s.settle/3, st.open)
	r, err := s.app.Pass.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Suspects != 1 || len(r.Confirmed) != 0 {
		t.Errorf("a slow ledger: %d suspects and %+v confirmed, want one suspect and nothing confirmed", r.Suspects, r.Confirmed)
	}
	if fs := s.findings(); len(fs) != 0 {
		t.Errorf("a slow ledger was reported: %s", mustJSON(fs))
	}

	// T-02 while the pass runs.
	key := newKey()
	before := s.balance(tn, f)
	stop := make(chan struct{})
	var passes sync.WaitGroup
	passes.Add(1)
	go func() {
		defer passes.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.app.Pass.Once(context.Background()); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := s.consume(s.runtime, f, 25, tn.External, key)
			if r.Status != http.StatusOK {
				t.Errorf("a delivery answered %d: %s", r.Status, r.Body)
			}
		}()
	}
	wg.Wait()
	close(stop)
	passes.Wait()
	if got := s.balance(tn, f); got != before-25 {
		t.Errorf("fifty deliveries with the pass running left %d, want %d", got, before-25)
	}
	s.pass()
	consumed := 0
	for _, row := range s.ledger(tn) {
		if row.ClientKey == key {
			consumed++
		}
	}
	if consumed != 1 {
		t.Errorf("%d ledger rows under the key, want 1", consumed)
	}
	if fs := s.findings(); len(fs) != 0 {
		t.Errorf("findings %s", mustJSON(fs))
	}
	if got := s.counter("quotacore_duplicate_reversal_total"); got != 0 {
		t.Errorf("duplicate_reversal_total = %v", got)
	}
}

// gate holds every Append while closed.
type gate struct {
	ledger.Appender
	mu   sync.Mutex
	wait chan struct{}
}

func (g *gate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.wait = make(chan struct{})
}

func (g *gate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.wait != nil {
		close(g.wait)
		g.wait = nil
	}
}

func (g *gate) Append(ctx context.Context, events []dataplane.Event) (ledger.Appended, error) {
	g.mu.Lock()
	w := g.wait
	g.mu.Unlock()
	if w != nil {
		select {
		case <-w:
		case <-ctx.Done():
			return ledger.Appended{}, ctx.Err()
		}
	}
	return g.Appender.Append(ctx, events)
}

// TestADuplicateDeductionIsReversedOnce is DR-049 through the pass (IP-12's
// hand-on): the second deduction under one key is refunded once, under the
// derived key, the ledger then holds the first deduction, the second and the
// reversal under the customer's key, the audit trail names both events, and a
// second pass issues nothing.
func TestADuplicateDeductionIsReversedOnce(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	key := newKey()
	s.duplicate(tn, f, 30, key)
	if got := s.balance(tn, f); got != 40 {
		t.Fatalf("the duplicate left %d, want 40", got)
	}
	r := s.pass()
	if r.Reversed != 1 {
		t.Fatalf("the pass reversed %d", r.Reversed)
	}
	if got := s.balance(tn, f); got != 70 {
		t.Errorf("the reversal left %d, want 70: one charge of 30", got)
	}
	if got := s.counter("quotacore_duplicate_reversal_total"); got != 1 {
		t.Errorf("duplicate_reversal_total = %v", got)
	}
	page := mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/tenants/"+tn.ID+"/events?idempotency_key="+key, ""),
		http.StatusOK)
	data := page["events"].([]any)
	if len(data) != 3 {
		t.Fatalf("the key filter returned %d events, want the deduction, the duplicate and the reversal: %s",
			len(data), mustJSON(page))
	}
	var sum int64
	sources := map[string]int{}
	for _, e := range data {
		m := e.(map[string]any)
		sum += int64(m["delta"].(float64))
		sources[m["event_type"].(string)+"/"+m["source"].(string)]++
		if m["idempotency_key"] != key {
			t.Errorf("an event under the key carries %v", m["idempotency_key"])
		}
	}
	if sum != -30 || sources["consumed/api"] != 2 || sources["refunded/system"] != 1 {
		t.Errorf("the key's events sum to %d with %v", sum, sources)
	}
	if n := s.rows(`SELECT count(*) FROM audit_log WHERE action = 'tenant.reverse_duplicate' AND subject_id = $1`, tn.ID); n != 1 {
		t.Errorf("%d reversal audit rows", n)
	}
	if n := s.rows(`SELECT count(*) FROM usage_event_dead_letters WHERE resolution = 'reversed'`); n != 1 {
		t.Errorf("%d dead letters resolved reversed", n)
	}
	if fs := s.findings(); len(fs) != 0 {
		t.Errorf("the reversed cycle does not reconcile: %s", mustJSON(fs))
	}
	if r := s.pass(); r.Reversed != 0 {
		t.Errorf("a second pass reversed %d", r.Reversed)
	}
	if got := s.counter("quotacore_duplicate_reversal_total"); got != 1 || s.balance(tn, f) != 70 {
		t.Errorf("after a second pass: reversals %v, balance %d", got, s.balance(tn, f))
	}
}

// failOnce refuses the first Reverse, as a pass that crashed after its refund
// and before its transaction committed would leave things.
type failOnce struct {
	reconcile.Ledger
	once sync.Once
}

func (f *failOnce) Reverse(ctx context.Context, r reconcile.Reversal) (bool, error) {
	failed := false
	f.once.Do(func() { failed = true })
	if failed {
		return false, errors.New("the process stopped between the refund and the commit")
	}
	return f.Ledger.Reverse(ctx, r)
}

// TestAReversalSurvivesACrashBetweenItsHalves is IP-11 D4's crash case: the
// refund was applied and the transaction recording it was not; the next pass
// replays the refund under the same derived key rather than issuing a second
// one, and records the reversal once.
func TestAReversalSurvivesACrashBetweenItsHalves(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	s.duplicate(tn, f, 30, newKey())
	crashing := reconcile.NewPass(reconcile.PassOptions{
		Ledger:   &failOnce{Ledger: db.NewReconciler(s.sql)},
		Balances: balances{s.app.Data},
		Refunder: refunder{plane: s.app.Service, now: time.Now},
		Settle:   s.settle,
	})
	if _, err := crashing.Once(context.Background()); err == nil {
		t.Fatal("the crashing pass did not fail")
	}
	if got := s.balance(tn, f); got != 70 {
		t.Fatalf("after the refund half: %d, want 70", got)
	}
	r := s.pass()
	if r.Reversed != 1 {
		t.Fatalf("the next pass reversed %d", r.Reversed)
	}
	if got := s.balance(tn, f); got != 70 {
		t.Errorf("the next pass refunded again: %d, want 70", got)
	}
	var reversals int
	for _, row := range s.ledger(tn) {
		if row.Type == "refunded" && row.Source == "system" {
			reversals++
			if row.Balance != 70 {
				t.Errorf("the replayed reversal row records %d, want the 70 the refund left", row.Balance)
			}
		}
	}
	if reversals != 1 {
		t.Errorf("%d reversal rows", reversals)
	}
	if fs := s.findings(); len(fs) != 0 {
		t.Errorf("findings %s", mustJSON(fs))
	}
	if got := s.counter("quotacore_duplicate_reversal_total"); got != 1 {
		t.Errorf("duplicate_reversal_total = %v", got)
	}
}

// TestADuplicateRefundIsReferredNotReversed: DR-049 reverses a deduction; a
// second refund under one key is a finding for a person, and no refund or
// charge is issued for it.
func TestADuplicateRefundIsReferredNotReversed(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	mustStatus(t, s.consume(s.runtime, f, 50, tn.External, newKey()), http.StatusOK)
	key := newKey()
	mustStatus(t, s.refund(f, 10, tn.External, key), http.StatusOK)
	idem, _ := store.IdempotencyKey(tn.ID, key)
	s.rdb.Del(context.Background(), idem)
	mustStatus(t, s.refund(f, 10, tn.External, key), http.StatusOK)
	r := s.pass()
	if r.Reversed != 0 || r.Referred != 1 || s.balance(tn, f) != 70 {
		t.Errorf("reversed %d, referred %d, balance %d", r.Reversed, r.Referred, s.balance(tn, f))
	}
	fs := s.findings()
	if len(fs) != 1 || fs[0].Kind != reconcile.KindDuplicateRefund {
		t.Errorf("findings %v", findingKinds(fs))
	}
	if got := s.counter("quotacore_event_reconcile_mismatch_total"); got != 0 {
		t.Errorf("a referred duplicate counted as a mismatch: %v", got)
	}
	// The referred refund is still part of the ledger the verifier reads,
	// because the store applied it.
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
	s.pass()
	if fs := s.findings(); len(fs) != 1 {
		t.Errorf("findings after the next change %v", findingKinds(fs))
	}
}
