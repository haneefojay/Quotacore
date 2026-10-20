package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/db"
	"github.com/quotacore/quotacore/internal/ledger"
	"github.com/quotacore/quotacore/internal/observability"
)

// The integration tests of IP-12: the ledger the real writer produces, read
// back from usage_events after a flush (S11), never by polling with a request.

func rowsFor(rows []ledgerRow, feature string) []ledgerRow {
	var out []ledgerRow
	for _, r := range rows {
		if r.Feature == feature {
			out = append(out, r)
		}
	}
	return out
}

// expect is one row as a test states it.
type expect struct {
	typ, source    string
	delta, balance int64
}

func (e expect) String() string {
	return fmt.Sprintf("%s/%s %+d to %d", e.typ, e.source, e.delta, e.balance)
}

func shapes(rows []ledgerRow) []expect {
	out := make([]expect, len(rows))
	for i, r := range rows {
		out[i] = expect{r.Type, r.Source, r.Delta, r.Balance}
	}
	return out
}

func sameShapes(got []ledgerRow, want ...expect) bool {
	g := shapes(got)
	if len(g) != len(want) {
		return false
	}
	for i := range g {
		if g[i] != want[i] {
			return false
		}
	}
	return true
}

// hashWindow is the window the balance hash is in now.
func (s *stack) hashWindow(tn tenant, feature string) (int64, time.Time) {
	s.t.Helper()
	h := s.state(tn, feature).Hash
	index, err := strconv.ParseInt(h["cycle_index"], 10, 64)
	if err != nil {
		s.t.Fatalf("cycle_index %q: %v", h["cycle_index"], err)
	}
	start, err := strconv.ParseInt(h["window_start"], 10, 64)
	if err != nil {
		s.t.Fatalf("window_start %q: %v", h["window_start"], err)
	}
	return index, time.UnixMilli(start).UTC()
}

// seedPrevious puts a feature's hash in the window before the current one,
// holding balance, so the next request crosses the boundary.
func (s *stack) seedPrevious(tn tenant, feature string, balance int64) {
	s.t.Helper()
	cur := s.currentWindow(tn, feature, time.Now())
	prev := s.currentWindow(tn, feature, cur.Start.Add(-time.Millisecond))
	s.seed(tn, feature, stored{Balance: balance, Limit: tn.ents[feature].limit, Index: prev.Index,
		Start: prev.Start, End: prev.End})
}

func (s *stack) refund(feature string, amount int64, external, key string) response {
	return s.do(http.MethodPost, "/v1/refund", s.runtime, metered(feature, amount, external), "Idempotency-Key", key)
}

// TestEveryAppliedMutationAppendsOneEvent is DoD 1: each of U-1 to U-7
// through its route writes exactly one row with the operation's type, source,
// delta, balance, window, request id and idempotency key; U-6 is written by
// whichever request crosses the boundary, consume, refund, check, grant or
// set; and a change of zero writes nothing (Q-22).
func TestEveryAppliedMutationAppendsOneEvent(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(1000)
	tp := "/v1/admin/tenants/" + tn.ID
	index, start := s.hashWindow(tn, f)
	before := time.Now().Add(-time.Second)

	consume := s.consume(s.runtime, f, 30, tn.External, "key-ledger-consume")
	mustStatus(t, consume, http.StatusOK)
	// A zero-amount consume is valid and changes nothing (DR-023).
	mustStatus(t, s.consume(s.runtime, f, 0, tn.External, "key-ledger-zero"), http.StatusOK)
	refund := s.refund(f, 10, tn.External, "key-ledger-refund")
	mustStatus(t, refund, http.StatusOK)
	grant := s.adminDo(http.MethodPost, tp+"/grant", `{"feature_key":"`+f+`","amount":25,"reason":"goodwill"}`)
	mustStatus(t, grant, http.StatusOK)
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/set", `{"feature_key":"`+f+`","value":1005,"reason":"same"}`), http.StatusOK)
	set := s.adminDo(http.MethodPost, tp+"/set", `{"feature_key":"`+f+`","value":900,"reason":"correction"}`)
	mustStatus(t, set, http.StatusOK)
	force := s.adminDo(http.MethodPost, tp+"/force-rollover", `{"feature_key":"`+f+`","reason":"support ticket 7"}`)
	mustStatus(t, force, http.StatusOK)
	smaller := uniq("small")
	s.createPlan(smaller, 400, "monthly", f)
	change := s.adminDo(http.MethodPatch, tp, fmt.Sprintf(`{"version":%d,"plan_key":%q}`, s.version(tp), smaller))
	mustStatus(t, change, http.StatusOK)
	newIndex, newStart := s.hashWindow(tn, f)
	after := time.Now().Add(time.Second)

	rows := s.ledger(tn)
	if !sameShapes(rows,
		expect{"consumed", "api", -30, 970},
		expect{"refunded", "api", 10, 980},
		expect{"granted", "admin", 25, 1005},
		expect{"set", "admin", -105, 900},
		expect{"force_rolled_over", "admin", 100, 1000},
		expect{"plan_changed", "admin", -600, 400},
	) {
		t.Fatalf("the ledger is %v", shapes(rows))
	}
	ids := []string{consume.Header.Get("X-Request-Id"), refund.Header.Get("X-Request-Id"), grant.Header.Get("X-Request-Id"),
		set.Header.Get("X-Request-Id"), force.Header.Get("X-Request-Id"), change.Header.Get("X-Request-Id")}
	clients := []string{"key-ledger-consume", "key-ledger-refund", "", "", "", ""}
	for i, r := range rows {
		if r.RequestID == "" || r.RequestID != ids[i] {
			t.Errorf("row %d (%s) has request id %q, want %q", i, r.Type, r.RequestID, ids[i])
		}
		if r.ClientKey != clients[i] {
			t.Errorf("row %d (%s) has idempotency key %q, want %q", i, r.Type, r.ClientKey, clients[i])
		}
		if r.OccurredAt.Before(before) || r.OccurredAt.After(after) {
			t.Errorf("row %d (%s) is dated %s, outside the test", i, r.Type, r.OccurredAt)
		}
		wantIndex, wantStart := index, start
		if r.Type == "plan_changed" {
			wantIndex, wantStart = newIndex, newStart
		}
		if r.Index != wantIndex || !r.Start.Equal(wantStart) {
			t.Errorf("row %d (%s) is in cycle %d from %s, want %d from %s", i, r.Type, r.Index, r.Start, wantIndex, wantStart)
		}
	}
	if !strings.Contains(rows[4].Metadata, `"reason": "support ticket 7"`) && !strings.Contains(rows[4].Metadata, `"reason":"support ticket 7"`) {
		t.Errorf("the force-rollover row's metadata is %q, want its reason (U-7)", rows[4].Metadata)
	}
	for i, r := range rows {
		if i != 4 && r.Metadata != "{}" && r.Type != "consumed" && r.Type != "refunded" {
			t.Errorf("row %d (%s) carries metadata %q", i, r.Type, r.Metadata)
		}
	}

	// U-6, written by whichever request crosses the boundary. Each feature's
	// hash is in the previous window with 3 left.
	feats := map[string]string{}
	var ents []ent
	for _, op := range []string{"consume", "refund", "check", "grant", "set", "idle"} {
		feats[op] = uniq("u6" + op)
		ents = append(ents, ent{feats[op], 1000, "monthly", false})
	}
	rt := s.tenant(externalID("u6"), "UTC", monthlyAnchor, ents...)
	rp := "/v1/admin/tenants/" + rt.ID
	for op, feat := range feats {
		left := int64(3)
		if op == "idle" {
			left = 1000 // nothing consumed, so the rollover changes nothing
		}
		s.seedPrevious(rt, feat, left)
	}
	reqs := map[string]response{
		"consume": s.consume(s.runtime, feats["consume"], 1, rt.External, newKey()),
		"refund":  s.refund(feats["refund"], 1, rt.External, newKey()),
		"check":   s.check(feats["check"], rt.External),
		"grant":   s.adminDo(http.MethodPost, rp+"/grant", `{"feature_key":"`+feats["grant"]+`","amount":5,"reason":"x"}`),
		"set":     s.adminDo(http.MethodPost, rp+"/set", `{"feature_key":"`+feats["set"]+`","value":400,"reason":"x"}`),
		"idle":    s.check(feats["idle"], rt.External),
	}
	if reqs["refund"].code(t) != "refund_exceeds_grant" {
		t.Errorf("a refund on a freshly rolled balance = %d %s, want refund_exceeds_grant", reqs["refund"].Status, reqs["refund"].Body)
	}
	rolled := expect{"cycle_rolled_over", "system", 997, 1000}
	want := map[string][]expect{
		"consume": {rolled, {"consumed", "api", -1, 999}},
		"refund":  {rolled},
		"check":   {rolled},
		"grant":   {rolled, {"granted", "admin", 5, 1005}},
		"set":     {rolled, {"set", "admin", -600, 400}},
		"idle":    nil,
	}
	all := s.ledger(rt)
	for op, feat := range feats {
		got := rowsFor(all, feat)
		if !sameShapes(got, want[op]...) {
			t.Errorf("%s across a boundary wrote %v, want %v", op, shapes(got), want[op])
			continue
		}
		if len(got) == 0 {
			continue
		}
		index, start := s.hashWindow(rt, feat)
		rid := reqs[op].Header.Get("X-Request-Id")
		for _, r := range got {
			if r.Index != index || !r.Start.Equal(start) || r.RequestID != rid {
				t.Errorf("%s: row %+v is not in the new cycle %d from %s under request %s", op, r, index, start, rid)
			}
		}
	}
}

// TestTheSinkRefusesAZeroDelta is DoD 1's sink half: an event with no change
// is never queued, is counted as a drop with reason error, and writes nothing.
func TestTheSinkRefusesAZeroDelta(t *testing.T) {
	s := newStack(t)
	_, _, tn := s.provisioned(10)
	errorsBefore := s.counter("quotacore_event_sink_dropped_total", observability.DropError)
	s.app.Ledger.Enqueue(dataplane.Event{RequestID: "r", TenantID: tn.ID, FeatureKey: "f", Type: "consumed",
		Source: "api", Delta: 0, OccurredAt: time.Now()})
	if got := s.counter("quotacore_event_sink_dropped_total", observability.DropError); got != errorsBefore+1 {
		t.Errorf("error drops rose by %v, want 1", got-errorsBefore)
	}
	if rows := s.ledger(tn); len(rows) != 0 {
		t.Errorf("a zero delta wrote %v", shapes(rows))
	}
}

// redeliver hands the inner appender every batch twice, once doubled in one
// call and once again in a second call, and loses the acknowledgement of the
// first lost batches after they commit, so the writer retries a batch that is
// already in the table.
type redeliver struct {
	ledger.Appender
	mu   sync.Mutex
	lost int
}

func (r *redeliver) Append(ctx context.Context, events []dataplane.Event) (ledger.Appended, error) {
	doubled := append(append([]dataplane.Event(nil), events...), events...)
	out, err := r.Appender.Append(ctx, doubled)
	if err != nil {
		return out, err
	}
	again, err := r.Appender.Append(ctx, events)
	if err != nil {
		return again, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lost > 0 {
		r.lost--
		return ledger.Appended{}, errors.New("the connection closed before the commit was acknowledged")
	}
	out.Present = len(events) - out.Written
	return out, nil
}

// TestEveryEventIsWrittenOnceWhenDeliveredTwice is DoD 4: an event delivered
// twice in one batch, again in a second batch, and again because the commit's
// acknowledgement was lost, is one row; nothing is dead-lettered and nothing
// is counted as a duplicate deduction.
func TestEveryEventIsWrittenOnceWhenDeliveredTwice(t *testing.T) {
	s := newStack(t, func(o *stackOptions) {
		o.appender = func(l *db.Ledger) ledger.Appender { return &redeliver{Appender: l, lost: 3} }
	})
	f, _, tn := s.provisioned(1000)
	for i := range 20 {
		mustStatus(t, s.consume(s.runtime, f, 1, tn.External, fmt.Sprintf("key-ledger-%d", i)), http.StatusOK)
	}
	rows := s.ledger(tn)
	if len(rows) != 20 {
		t.Errorf("%d rows for 20 consumes delivered twice and retried", len(rows))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.ID] {
			t.Errorf("event %s is in the ledger twice", r.ID)
		}
		seen[r.ID] = true
	}
	if n := s.rows(`SELECT count(*) FROM usage_event_dead_letters`); n != 0 {
		t.Errorf("%d dead letters", n)
	}
	if got := s.counter("quotacore_event_duplicate_detected_total"); got != 0 {
		t.Errorf("duplicate_detected_total = %v, want 0: a redelivery is not a second deduction", got)
	}
}

// TestADeadLetterIsVisibleAndReplayed is DoD 5: an event whose feature
// resolves to nothing and a second deduction under one key are each kept as
// a dead letter with its reason, counted, gauged and logged with its event_id;
// after the feature is created the next tick replays the rejected one exactly
// once, and the duplicate is never replayed.
func TestADeadLetterIsVisibleAndReplayed(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(1000)
	mustStatus(t, s.consume(s.runtime, f, 5, tn.External, "key-ledger-first"), http.StatusOK)
	ghost := uniq("ghost")
	orphanID, dupID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := time.Now()
	s.app.Ledger.Enqueue(dataplane.Event{ID: orphanID, RequestID: "r-orphan", TenantID: tn.ID, FeatureKey: ghost,
		Type: "consumed", Source: "api", Amount: 1, Delta: -1, Balance: 9, ClientKey: "key-ledger-orphan", OccurredAt: now})
	s.app.Ledger.Enqueue(dataplane.Event{ID: dupID, RequestID: "r-dup", TenantID: tn.ID, FeatureKey: f,
		Type: "consumed", Source: "api", Amount: 5, Delta: -5, Balance: 990, ClientKey: "key-ledger-first", OccurredAt: now})
	rows := s.ledger(tn)
	if len(rows) != 1 {
		t.Fatalf("the ledger is %v, want the first deduction alone", shapes(rows))
	}
	reasons := map[string]string{}
	q, err := s.seedDB.Query(`SELECT event_id::text, reason FROM usage_event_dead_letters`)
	if err != nil {
		t.Fatal(err)
	}
	for q.Next() {
		var id, reason string
		if err := q.Scan(&id, &reason); err != nil {
			t.Fatal(err)
		}
		reasons[id] = reason
	}
	q.Close()
	if reasons[orphanID.String()] != observability.DeadRejected || reasons[dupID.String()] != observability.DeadDuplicate || len(reasons) != 2 {
		t.Fatalf("dead letters = %v", reasons)
	}
	for reason, want := range map[string]float64{observability.DeadRejected: 1, observability.DeadDuplicate: 1} {
		if got := s.counter("quotacore_event_dead_lettered_total", reason); got != want {
			t.Errorf("dead_lettered_total{%s} = %v, want %v", reason, got, want)
		}
	}
	if got := s.counter("quotacore_event_duplicate_detected_total"); got != 1 {
		t.Errorf("duplicate_detected_total = %v, want 1", got)
	}
	for _, id := range []uuid.UUID{orphanID, dupID} {
		if !strings.Contains(s.logs.String(), id.String()) {
			t.Errorf("no log line names dead letter %s", id)
		}
	}

	opt := db.RetentionOptions{Metrics: s.app.Ledger.Metrics()}
	p, err := db.PruneOnce(context.Background(), s.sql, time.Now(), opt)
	if err != nil || p.Replayed != 0 {
		t.Fatalf("a tick while the cause stands = %+v, %v", p, err)
	}
	if got := s.counter("quotacore_event_dead_letters", observability.DeadRejected); got != 1 {
		t.Errorf("the rejected gauge = %v, want 1", got)
	}
	s.createFeature(ghost)
	if p, err = db.PruneOnce(context.Background(), s.sql, time.Now(), opt); err != nil || p.Replayed != 1 {
		t.Fatalf("the tick after the fix = %+v, %v; want one replayed", p, err)
	}
	if p, err = db.PruneOnce(context.Background(), s.sql, time.Now(), opt); err != nil || p.Replayed != 0 {
		t.Fatalf("a second tick = %+v, %v; want nothing", p, err)
	}
	if n := s.rows(`SELECT count(*) FROM usage_events WHERE id = $1`, orphanID.String()); n != 1 {
		t.Errorf("the replayed event is in the ledger %d times", n)
	}
	if n := s.rows(`SELECT count(*) FROM usage_events WHERE id = $1`, dupID.String()); n != 0 {
		t.Errorf("the duplicate deduction was replayed into the ledger")
	}
	if got := s.counter("quotacore_event_dead_letters", observability.DeadRejected); got != 0 {
		t.Errorf("the rejected gauge = %v after the replay, want 0", got)
	}
	if got := s.counter("quotacore_event_dead_letters", observability.DeadDuplicate); got != 1 {
		t.Errorf("the duplicate gauge = %v, want 1: IP-11 closes it", got)
	}
}

// stall holds every Append until release is closed.
type stall struct {
	ledger.Appender
	release chan struct{}
}

func (s *stall) Append(ctx context.Context, events []dataplane.Event) (ledger.Appended, error) {
	select {
	case <-s.release:
	case <-ctx.Done():
		return ledger.Appended{}, ctx.Err()
	}
	return s.Appender.Append(ctx, events)
}

// TestStatusReportsTheLedger is IP-09's hand-on (D8): /v1/admin/status reports
// the writer running, a queue depth and a drop count that move while the
// writer is stalled, and draining while a shutdown writes what is queued.
func TestStatusReportsTheLedger(t *testing.T) {
	st := &stall{release: make(chan struct{})}
	s := newStack(t, func(o *stackOptions) {
		o.env["QUOTACORE_EVENT_QUEUE_SIZE"] = "8"
		o.env["QUOTACORE_EVENT_BATCH_SIZE"] = "4"
		o.appender = func(l *db.Ledger) ledger.Appender { st.Appender = l; return st }
	})
	status := func() map[string]any {
		return mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/status", ""), http.StatusOK)
	}
	if m := status(); str(m, "worker") != "running" || num(t, m, "queue_depth") != 0 || num(t, m, "ledger_dropped_total") != 0 {
		t.Fatalf("an idle status = %s", mustJSON(m))
	}
	f, _, tn := s.provisioned(1000)
	for i := range 30 {
		mustStatus(t, s.consume(s.runtime, f, 1, tn.External, fmt.Sprintf("key-ledger-%d", i)), http.StatusOK)
	}
	m := status()
	if num(t, m, "queue_depth") != 8 || num(t, m, "ledger_dropped_total") < 30-8-4 {
		t.Errorf("a stalled writer's status = %s, want a full queue of 8 and the overflow dropped", mustJSON(m))
	}
	drained := make(chan struct{})
	go func() {
		s.app.Drain(context.Background())
		close(drained)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for str(status(), "worker") != "draining" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if w := str(status(), "worker"); w != "draining" {
		t.Errorf("worker during a shutdown = %q, want draining", w)
	}
	close(st.release)
	<-drained
	if m := status(); str(m, "worker") != "stopped" || num(t, m, "queue_depth") != 0 {
		t.Errorf("status after the drain = %s", mustJSON(m))
	}
	accepted := int(s.counter("quotacore_event_enqueued_total"))
	if rows := s.ledger(tn); len(rows) != accepted {
		t.Errorf("the drain wrote %d of the %d accepted", len(rows), accepted)
	}
}

// p99 is the 99th percentile of ds.
func p99(ds []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)*99/100]
}

// TestLedgerSaturationLeavesEnforcementAlone is DoD 3: with the writer stalled
// behind an ACCESS EXCLUSIVE lock on usage_events and a queue of 64, 2 000
// consumes are answered exactly as they would be with no ledger at all, the
// overflow is counted as queue_full drops, the queue sits at its cap, and the
// latency is the latency of the same run with the lock released.
func TestLedgerSaturationLeavesEnforcementAlone(t *testing.T) {
	s := newStack(t, func(o *stackOptions) {
		o.env["QUOTACORE_EVENT_QUEUE_SIZE"] = "64"
		o.env["QUOTACORE_EVENT_BATCH_SIZE"] = "16"
	})
	const n = 2000
	run := func(label string) []time.Duration {
		f, _, tn := s.provisioned(n)
		lat := make([]time.Duration, n)
		for i := range n {
			start := time.Now()
			r := s.consume(s.runtime, f, 1, tn.External, fmt.Sprintf("saturation-%s-%05d", label, i))
			lat[i] = time.Since(start)
			if r.Status != http.StatusOK || r.json(t)["balance"] != float64(n-i-1) {
				t.Fatalf("%s: consume %d = %d %s, want 200 with %d left", label, i, r.Status, r.Body, n-i-1)
			}
		}
		if b := s.balance(tn, f); b != 0 {
			t.Errorf("%s: balance = %d, want 0", label, b)
		}
		return lat
	}
	baseline := run("before")
	s.flush()

	lock, err := s.seedDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback() }()
	if _, err := lock.Exec(`LOCK TABLE usage_events IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	enqueued := s.counter("quotacore_event_enqueued_total")
	dropped := s.counter("quotacore_event_sink_dropped_total", observability.DropQueueFull)
	locked := run("locked")
	accepted := s.counter("quotacore_event_enqueued_total") - enqueued
	if got := s.counter("quotacore_event_sink_dropped_total", observability.DropQueueFull) - dropped; got != n-accepted || got == 0 {
		t.Errorf("queue_full drops rose by %v; %v were accepted of %d", got, accepted, n)
	}
	if depth := s.counter("quotacore_event_queue_depth"); depth != 64 {
		t.Errorf("queue depth = %v, want the cap of 64", depth)
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	s.flush()
	after := run("after")
	// The baseline is the slower of the two unlocked runs, so a noisy
	// neighbour slowing one of them does not fail the comparison.
	base := max(p99(baseline), p99(after))
	if got := p99(locked); got > base+base/2+2*time.Millisecond {
		t.Errorf("p99 with the writer stalled = %s, against %s with it free", got, base)
	}
}

// TestAnIdleTenantRollsOnceAndEmitsOneEvent is the v0.1 exit criterion: a
// tenant idle across three boundaries is granted one allowance and the ledger
// gains one cycle_rolled_over, with the index jumping by three
// (consistency-and-recovery section 4.3).
func TestAnIdleTenantRollsOnceAndEmitsOneEvent(t *testing.T) {
	s := newStack(t)
	f := uniq("idle")
	tn := s.tenant(externalID("idle"), "UTC", monthlyAnchor, ent{f, 1000, "monthly", false})
	cur := s.currentWindow(tn, f, time.Now())
	old := s.currentWindow(tn, f, cur.Start.AddDate(0, -3, 0))
	if cur.Index-old.Index != 3 {
		t.Fatalf("the fixture is %d windows back, want 3", cur.Index-old.Index)
	}
	s.seed(tn, f, stored{Balance: 3, Limit: 1000, Index: old.Index, Start: old.Start, End: old.End})
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
	rows := s.ledger(tn)
	if !sameShapes(rows, expect{"cycle_rolled_over", "system", 997, 1000}, expect{"consumed", "api", -1, 999}) {
		t.Fatalf("the ledger after three idle boundaries is %v", shapes(rows))
	}
	if rows[0].Index != old.Index+3 || !rows[0].Start.Equal(cur.Start) {
		t.Errorf("the rollover is in cycle %d from %s, want %d from %s", rows[0].Index, rows[0].Start, old.Index+3, cur.Start)
	}
}

// TestListUsageEventsReadsWhatTheWriterWrote is IP-09's hand-on: the events
// route over rows the writer produced, with the window the tenant's anchor
// computes; a null window for a row whose stored cycle_start is not that
// window (S5); and, after a plan change re-anchors the tenant, a null window
// for the rows the old anchor defined.
func TestListUsageEventsReadsWhatTheWriterWrote(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(1000)
	tp := "/v1/admin/tenants/" + tn.ID
	mustStatus(t, s.consume(s.runtime, f, 30, tn.External, "key-ledger-a"), http.StatusOK)
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/grant", `{"feature_key":"`+f+`","amount":5,"reason":"x"}`), http.StatusOK)
	s.flush()
	list := func() []map[string]any {
		m := mustStatus(t, s.adminDo(http.MethodGet, tp+"/events", ""), http.StatusOK)
		var out []map[string]any
		for _, e := range asSlice(m["events"]) {
			out = append(out, asMap(e))
		}
		return out
	}
	index, start := s.hashWindow(tn, f)
	evs := list()
	if len(evs) != 2 || str(evs[0], "event_type") != "granted" || str(evs[1], "event_type") != "consumed" ||
		num(t, evs[1], "delta") != -30 || num(t, evs[1], "balance_after") != 970 || num(t, evs[1], "cycle_index") != index {
		t.Fatalf("events = %s", mustJSON(evs))
	}
	for _, e := range evs {
		got, err := time.Parse(time.RFC3339Nano, str(e, "cycle_start"))
		// The route renders whole seconds.
		if err != nil || !got.Equal(start.Truncate(time.Second)) || e["cycle_end"] == nil {
			t.Errorf("event %s has window %v to %v, want from %s", str(e, "event_type"), e["cycle_start"], e["cycle_end"], start)
		}
	}

	// A hash whose stored window starts a minute late: the row records that
	// start, which the anchor does not compute, so the route says null.
	h := s.state(tn, f).Hash
	shifted, _ := strconv.ParseInt(h["window_start"], 10, 64)
	end, _ := strconv.ParseInt(h["window_end"], 10, 64)
	endAt := time.UnixMilli(end)
	s.seed(tn, f, stored{Balance: 975, Limit: 1000, Bonus: 5, Index: index, Start: time.UnixMilli(shifted + 60_000), End: &endAt})
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, "key-ledger-b"), http.StatusOK)
	s.flush()
	evs = list()
	if str(evs[0], "event_type") != "consumed" || evs[0]["cycle_start"] != nil || evs[0]["cycle_end"] != nil {
		t.Errorf("the row recorded against a window the anchor does not compute = %s, want a null window", mustJSON(evs[0]))
	}

	// A plan change re-anchors: its row is in the new window, the older rows
	// have none.
	other := uniq("other")
	s.createPlan(other, 200, "monthly", f)
	mustStatus(t, s.adminDo(http.MethodPatch, tp, fmt.Sprintf(`{"version":%d,"plan_key":%q}`, s.version(tp), other)), http.StatusOK)
	s.flush()
	_, newStart := s.hashWindow(tn, f)
	evs = list()
	if len(evs) != 4 || str(evs[0], "event_type") != "plan_changed" {
		t.Fatalf("events after the plan change = %s", mustJSON(evs))
	}
	if got, err := time.Parse(time.RFC3339Nano, str(evs[0], "cycle_start")); err != nil || !got.Equal(newStart.Truncate(time.Second)) {
		t.Errorf("the plan change's window starts %v, want %s", evs[0]["cycle_start"], newStart)
	}
	for _, e := range evs[1:] {
		if e["cycle_start"] != nil || e["cycle_end"] != nil {
			t.Errorf("a row the old anchor defined = %s, want a null window", mustJSON(e))
		}
	}
}

// TestTheLedgerReconcilesEveryCycle is DoD 2: over four tenants on hourly,
// monthly and never features, a seeded random sequence of consumes, refunds,
// checks, grants, sets, force-rollovers, plan changes, an apply-now and
// boundary crossings leaves a ledger in which every cycle's last balance_after
// is its last opening plus the deltas after it, and the current cycle's
// result is the hash's balance (D1, DR-042, data-model section 2.7).
func TestTheLedgerReconcilesEveryCycle(t *testing.T) {
	s := newStack(t)
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d", seed)
	hourly, monthly, never := uniq("rh"), uniq("rm"), uniq("rn")
	for _, k := range []string{hourly, monthly, never} {
		s.createFeature(k)
	}
	plans := []string{uniq("ra"), uniq("rb")}
	planIDs := make([]string, len(plans))
	for i, p := range plans {
		limit := int64(500 + 250*i)
		planIDs[i] = str(mustStatus(t, s.adminDo(http.MethodPost, "/v1/admin/plans", `{"plan_key":"`+p+`","name":"P","entitlements":[`+
			`{"feature_key":"`+hourly+`","limit":`+strconv.FormatInt(limit, 10)+`,"reset_interval":"hourly"},`+
			`{"feature_key":"`+monthly+`","limit":`+strconv.FormatInt(limit*2, 10)+`,"reset_interval":"monthly"},`+
			`{"feature_key":"`+never+`","limit":`+strconv.FormatInt(limit*3, 10)+`,"reset_interval":"never"}]}`),
			http.StatusCreated), "id")
	}
	features := []string{hourly, monthly, never}
	// The tenants are anchored in the past, so a window can be moved back
	// one, and their hashes are seeded full: a cycle with no opening row
	// opens at the limit.
	var tenants []tenant
	for i := range 4 {
		limit := int64(500 + 250*(i%2))
		tn := s.tenantOn(planIDs[i%2], "UTC", monthlyAnchor, ent{hourly, limit, "hourly", false},
			ent{monthly, limit * 2, "monthly", false}, ent{never, limit * 3, "never", false})
		for _, f := range features {
			s.seedCurrent(tn, f, tn.ents[f].limit)
		}
		tenants = append(tenants, tn)
	}
	limitOf := func(tn tenant, f string) int64 {
		h := s.state(tn, f).Hash
		n, _ := strconv.ParseInt(h["limit"], 10, 64)
		return n
	}
	cross := func(tn tenant, f string) {
		// Move the hash into the window before the one it is in, keeping its
		// balance, so the next request rolls it back into the current window
		// and its rollover row reopens that cycle, as a force-rollover does.
		h := s.state(tn, f).Hash
		if h["window_end"] == "" {
			return
		}
		start, _ := strconv.ParseInt(h["window_start"], 10, 64)
		end, _ := strconv.ParseInt(h["window_end"], 10, 64)
		bal, _ := strconv.ParseInt(h["balance"], 10, 64)
		lim, _ := strconv.ParseInt(h["limit"], 10, 64)
		bonus, _ := strconv.ParseInt(h["bonus"], 10, 64)
		idx, _ := strconv.ParseInt(h["cycle_index"], 10, 64)
		if idx == 0 {
			return
		}
		length := time.Duration(end-start) * time.Millisecond
		prevStart := time.UnixMilli(start).Add(-length)
		prevEnd := time.UnixMilli(start)
		s.seed(tn, f, stored{Balance: bal, Limit: lim, Bonus: bonus, Index: idx - 1, Start: prevStart, End: &prevEnd})
	}
	applied := false
	for range 300 {
		tn := tenants[rng.Intn(len(tenants))]
		f := features[rng.Intn(len(features))]
		tp := "/v1/admin/tenants/" + tn.ID
		amount := int64(rng.Intn(40))
		switch op := rng.Intn(100); {
		case op < 40:
			s.consume(s.runtime, f, amount, tn.External, newKey())
		case op < 50:
			s.refund(f, amount, tn.External, newKey())
		case op < 58:
			s.check(f, tn.External)
		case op < 68:
			s.adminDo(http.MethodPost, tp+"/grant", fmt.Sprintf(`{"feature_key":%q,"amount":%d,"reason":"r"}`, f, amount))
		case op < 76:
			value := rng.Int63n(limitOf(tn, f) + 1)
			s.adminDo(http.MethodPost, tp+"/set", fmt.Sprintf(`{"feature_key":%q,"value":%d,"reason":"r"}`, f, value))
		case op < 82:
			s.adminDo(http.MethodPost, tp+"/force-rollover", fmt.Sprintf(`{"feature_key":%q,"reason":"r"}`, f))
		case op < 86:
			plan := plans[rng.Intn(len(plans))]
			s.adminDo(http.MethodPatch, tp, fmt.Sprintf(`{"version":%d,"plan_key":%q}`, s.version(tp), plan))
		case op < 88 && !applied:
			// One apply-now of an edited plan, which re-anchors every tenant
			// on it (IP-10 D2).
			applied = true
			mustStatus(t, s.adminDo(http.MethodPost, "/v1/admin/plans/"+plans[0]+"/apply", `{"reason":"reconcile"}`), http.StatusOK)
		default:
			cross(tn, f)
		}
	}

	type cycleKey struct {
		feature string
		start   time.Time
		index   int64
	}
	opening := map[string]bool{"cycle_rolled_over": true, "plan_changed": true, "force_rolled_over": true}
	for _, tn := range tenants {
		rows := s.ledger(tn)
		cycles := map[cycleKey][]ledgerRow{}
		var order []cycleKey
		for _, r := range rows {
			k := cycleKey{r.Feature, r.Start, r.Index}
			if _, ok := cycles[k]; !ok {
				order = append(order, k)
			}
			cycles[k] = append(cycles[k], r)
		}
		for _, k := range order {
			rs := cycles[k]
			var base int64
			from := 0
			found := false
			for i, r := range rs {
				if opening[r.Type] {
					base, from, found = r.Balance, i+1, true
				}
			}
			if !found {
				base = rs[0].Balance - rs[0].Delta
				// No opening row: the cycle opened at the effective limit,
				// which is what the first row's balance before it was.
			}
			sum := base
			for _, r := range rs[from:] {
				sum += r.Delta
			}
			if last := rs[len(rs)-1].Balance; sum != last {
				t.Errorf("tenant %s cycle %+v: opening %d plus the deltas after it is %d, the last balance_after is %d (seed %d): %v",
					tn.External, k, base, sum, last, seed, shapes(rs))
			}
		}
		// The current cycle's result is the hash's balance.
		for _, f := range features {
			index, start := s.hashWindow(tn, f)
			rs := cycles[cycleKey{f, start, index}]
			if len(rs) == 0 {
				continue
			}
			if last := rs[len(rs)-1].Balance; last != s.balance(tn, f) {
				t.Errorf("tenant %s %s: the ledger ends at %d, the hash holds %d (seed %d)", tn.External, f, last, s.balance(tn, f), seed)
			}
		}
	}
	if n := s.rows(`SELECT count(*) FROM usage_event_dead_letters`); n != 0 {
		t.Errorf("%d dead letters", n)
	}
	if got := s.counter("quotacore_event_sink_dropped_total", observability.DropQueueFull); got != 0 {
		t.Errorf("%v events dropped", got)
	}
}

// writerScript is TestTheWriterIsAnOptimisation's fixed sequence, spread
// over s and its peers in turn, with s's keys (a peer shares s's database, not
// the database its own keys went into): what each step answered and the
// balances it left.
func writerScript(t *testing.T, s *stack, peers ...*stack) []string {
	t.Helper()
	all := append([]*stack{s}, peers...)
	f, g := "writer-f", "writer-g"
	tn := s.tenant("writer-script", "UTC", monthlyAnchor, ent{f, 100, "monthly", false}, ent{g, 50, "monthly", false})
	s.seedCurrent(tn, f, 100)
	s.seedPrevious(tn, g, 7)
	tp := "/v1/admin/tenants/" + tn.ID
	var out []string
	for i := range 12 {
		on := all[i%len(all)]
		key := fmt.Sprintf("writer-%04d", i)
		var r response
		switch i % 6 {
		case 0, 1:
			r = on.do(http.MethodPost, "/v1/consume", s.runtime, metered(f, 7, tn.External), "Idempotency-Key", key)
		case 2:
			r = on.do(http.MethodPost, "/v1/refund", s.runtime, metered(f, 3, tn.External), "Idempotency-Key", key)
		case 3:
			r = on.do(http.MethodPost, "/v1/check", s.runtime, metered(g, 1, tn.External))
		case 4:
			r = on.do(http.MethodPost, tp+"/grant", s.admin, `{"feature_key":"`+f+`","amount":4,"reason":"x"}`)
		case 5:
			r = on.do(http.MethodPost, "/v1/consume", s.runtime, metered(g, 2, tn.External), "Idempotency-Key", key)
		}
		out = append(out, fmt.Sprintf("%d %s", r.Status, r.Body))
	}
	out = append(out, fmt.Sprintf("balances %d %d", s.balance(tn, f), s.balance(tn, g)))
	return out
}

// stripVolatile removes what differs between any two runs: request ids,
// timestamps and the tenant id.
func stripVolatile(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		for _, field := range []string{"request_id", "cycle_start", "cycle_end", "server_time"} {
			for {
				at := strings.Index(l, `"`+field+`":"`)
				if at < 0 {
					break
				}
				end := strings.Index(l[at+len(field)+4:], `"`)
				l = l[:at] + `"` + field + `":~` + l[at+len(field)+4+end+1:]
			}
		}
		out[i] = l
	}
	return out
}

// TestTheWriterIsAnOptimisation is DoD 8: one fixed sequence of consumes,
// refunds, checks, grants and a boundary crossing is answered the same and
// leaves the same balances whether the writer is healthy, never started,
// stalled, or running in two instances against one database; and two
// instances' writers write no row twice.
func TestTheWriterIsAnOptimisation(t *testing.T) {
	healthy := newStack(t)
	want := stripVolatile(writerScript(t, healthy))

	stopped := newStack(t, func(o *stackOptions) { o.noWriter = true })
	if got := stripVolatile(writerScript(t, stopped)); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("with no writer:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	st := &stall{release: make(chan struct{})}
	stalled := newStack(t, func(o *stackOptions) {
		o.appender = func(l *db.Ledger) ledger.Appender { st.Appender = l; return st }
	})
	got := stripVolatile(writerScript(t, stalled))
	close(st.release)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("with a stalled writer:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	one := newStack(t)
	two := newStack(t, func(o *stackOptions) { o.dbURL = one.dbURL })
	if got := stripVolatile(writerScript(t, one, two)); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("with two instances:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	one.flush()
	two.flush()
	healthy.flush()
	count := func(s *stack) int { return s.rows(`SELECT count(*) FROM usage_events`) }
	if a, b := count(one), count(healthy); a != b {
		t.Errorf("two instances wrote %d rows, one wrote %d", a, b)
	}
	if n := one.rows(`SELECT count(*) FROM (SELECT id FROM usage_events GROUP BY id HAVING count(*) > 1) d`); n != 0 {
		t.Errorf("%d events written twice by two instances", n)
	}
}

// TestTheWriterKeepsUpWithTheFloor is S9: 20 000 events through the real
// writer and its two-connection pool are appended at no less than a generous
// floor under NFR-T8's 2 000 a second; the measured figure is IP-16's.
func TestTheWriterKeepsUpWithTheFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput is not a short test")
	}
	s := newStack(t, func(o *stackOptions) { o.env["QUOTACORE_EVENT_QUEUE_SIZE"] = "20000" })
	f, _, tn := s.provisioned(1000)
	const n = 20000
	start := time.Now()
	for i := range n {
		s.app.Ledger.Enqueue(dataplane.Event{RequestID: "r", TenantID: tn.ID, FeatureKey: f, Type: "granted",
			Source: "admin", Amount: 1, Delta: 1, Balance: int64(1001 + i), OccurredAt: time.Now()})
	}
	s.flush()
	rate := float64(n) / time.Since(start).Seconds()
	if got := s.rows(`SELECT count(*) FROM usage_events WHERE tenant_id = $1`, tn.ID); got != n {
		t.Fatalf("%d rows of %d", got, n)
	}
	t.Logf("%.0f appends a second", rate)
	if rate < 1000 {
		t.Errorf("%.0f appends a second, below the floor of 1 000", rate)
	}
}

// TestAnImmediateOverrideIsWrittenAsAPlanChange closes a gap IP-10 left: an
// override set or cleared with apply_at=immediate moves the open cycle's
// balance by the change in limit (D6), and that move is a row, written as
// U-5's plan_changed because the contract has no other type for it (Q-24);
// across a boundary its transition's rollover is written first.
func TestAnImmediateOverrideIsWrittenAsAPlanChange(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(1000)
	tp := "/v1/admin/tenants/" + tn.ID
	mustStatus(t, s.consume(s.runtime, f, 100, tn.External, newKey()), http.StatusOK)
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/overrides",
		`{"feature_key":"`+f+`","limit_value":600,"interval":null,"apply_at":"immediate"}`), http.StatusOK)
	if b := s.balance(tn, f); b != 500 {
		t.Fatalf("balance after an immediate override to 600 = %d, want 500", b)
	}
	if r := s.adminDo(http.MethodDelete, tp+"/overrides/"+f, `{"apply_at":"immediate"}`); r.Status != http.StatusNoContent {
		t.Fatalf("clearing the override = %d %s", r.Status, r.Body)
	}
	rows := s.ledger(tn)
	if !sameShapes(rows, expect{"consumed", "api", -100, 900}, expect{"plan_changed", "admin", -400, 500},
		expect{"plan_changed", "admin", 400, 900}) {
		t.Errorf("the ledger is %v", shapes(rows))
	}
}
