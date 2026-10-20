package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// normalPeer starts a second instance on s's database in normal mode, with
// s's keys.
func (s *stack) normalPeer() *stack {
	s.t.Helper()
	n := newStack(s.t, func(o *stackOptions) { o.dbURL = s.dbURL })
	n.runtime, n.admin = s.runtime, s.admin
	return n
}

// loseStore deletes every key of the tenants, as a fast store lost and
// restarted empty would.
func (s *stack) loseStore(ids ...string) {
	s.t.Helper()
	ctx := context.Background()
	for _, id := range ids {
		for _, k := range s.rdb.Keys(ctx, "qc:{t:"+id+"}:*").Val() {
			s.rdb.Del(ctx, k)
		}
	}
}

// TestT14AStoreLossInsideTheWindowIsReversedOnce is T-14's eight lines in
// process: a consume of 30, the fast store flushed and rebuilt from the
// ledger, the identical request retried inside the window and applied again,
// and reconciliation reversing it exactly once, with a second pass issuing
// nothing (DR-048, DR-049, IP-11 D4, D7).
func TestT14AStoreLossInsideTheWindowIsReversedOnce(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	key := newKey()
	mustStatus(t, s.consume(s.runtime, f, 30, tn.External, key), http.StatusOK)
	s.flush()
	s.app.Close()
	s.loseStore(tn.ID)
	c, _ := s.commands()
	rep, code, err := c.rebuild(context.Background(), rebuildOptions{apply: true, reason: "T-14"})
	if err != nil || code != exitClean {
		t.Fatalf("the rebuild: exit %d, %v: %s", code, err, mustJSON(rep))
	}
	if got := s.balance(tn, f); got != 70 {
		t.Fatalf("the rebuild wrote %d, want 70", got)
	}
	n := s.normalPeer()
	until(t, "", func() response { return n.consume(n.runtime, f, 30, tn.External, key) })
	n.flush()
	if got := n.balance(tn, f); got != 40 {
		t.Fatalf("the retry inside the window left %d, want 40: the record was lost, so it applies", got)
	}
	r := n.pass()
	if r.Reversed != 1 {
		t.Fatalf("the pass reversed %d, want 1", r.Reversed)
	}
	if got := n.balance(tn, f); got != 70 {
		t.Errorf("after the reversal the balance is %d, want 70: a single charge of 30", got)
	}
	if r := n.pass(); r.Reversed != 0 {
		t.Errorf("a second pass reversed %d", r.Reversed)
	}
	if got := n.counter("quotacore_duplicate_reversal_total"); got != 1 || n.balance(tn, f) != 70 {
		t.Errorf("after a second pass: reversals %v, balance %d", got, n.balance(tn, f))
	}
	if refunds := s.rows(`SELECT count(*) FROM usage_events WHERE event_type = 'refunded' AND reconciles_key = $1`,
		key); refunds != 1 {
		t.Errorf("%d refunds against the key, want 1", refunds)
	}
}

// TestADisputedDeductionIsAnsweredFromTheLedgerAndAudit is IP-11 DoD 4 and
// UC-16: a tenant's history of consumes, a refund, a grant, a set, a
// force-rollover, a plan change and a reversed duplicate is answered, after
// the fast store is gone, from the events and audit routes alone.
func TestADisputedDeductionIsAnsweredFromTheLedgerAndAudit(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	other := uniq("po")
	s.createPlan(other, 300, "monthly", f)
	tp := "/v1/admin/tenants/" + tn.ID
	mustStatus(t, s.consume(s.runtime, f, 10, tn.External, newKey()), http.StatusOK)
	mustStatus(t, s.consume(s.runtime, f, 15, tn.External, newKey()), http.StatusOK)
	mustStatus(t, s.refund(f, 5, tn.External, newKey()), http.StatusOK)
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/grant", `{"feature_key":"`+f+`","amount":20,"reason":"goodwill"}`), http.StatusOK)
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/set", `{"feature_key":"`+f+`","value":60,"reason":"correction"}`), http.StatusOK)
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/force-rollover", `{"feature_key":"`+f+`","reason":"incident"}`), http.StatusOK)
	mustStatus(t, s.consume(s.runtime, f, 7, tn.External, newKey()), http.StatusOK)
	mustStatus(t, s.adminDo(http.MethodPatch, tp, fmt.Sprintf(`{"version":%d,"plan_key":%q}`, s.version(tp), other)),
		http.StatusOK)
	until(t, "", func() response { return s.check(f, tn.External) })
	disputed := newKey()
	s.duplicate(tn, f, 30, disputed)
	if r := s.pass(); r.Reversed != 1 {
		t.Fatalf("the pass reversed %d", r.Reversed)
	}
	final := s.balance(tn, f)
	s.loseStore(tn.ID)

	// The key filter: the original, the duplicate, and the reversal.
	page := mustStatus(t, s.adminDo(http.MethodGet, tp+"/events?idempotency_key="+disputed, ""), http.StatusOK)
	byKey := page["events"].([]any)
	kinds := map[string]int{}
	for _, e := range byKey {
		m := e.(map[string]any)
		kinds[str(m, "event_type")+"/"+str(m, "source")]++
	}
	if len(byKey) != 3 || kinds["consumed/api"] != 2 || kinds["refunded/system"] != 1 {
		t.Errorf("the key filter returned %v", kinds)
	}

	// The whole history, oldest first, chains row to row and ends at the
	// balance the store held.
	page = mustStatus(t, s.adminDo(http.MethodGet, tp+"/events?limit=200", ""), http.StatusOK)
	var rows []map[string]any
	for _, e := range page["events"].([]any) {
		rows = append(rows, e.(map[string]any))
	}
	if page["next_cursor"] != nil {
		t.Fatalf("the history did not fit one page")
	}
	slices.Reverse(rows)
	var at int64 = -1
	cycleSum := map[string]int64{}
	cycleFirst := map[string]int64{}
	for _, r := range rows {
		delta, after := num(t, r, "delta"), num(t, r, "balance_after")
		if at >= 0 && after-delta != at {
			t.Errorf("%s %s starts from %d after a row ending at %d", str(r, "event_type"), str(r, "id"), after-delta, at)
		}
		at = after
		c := str(r, "cycle_start") + "#" + fmt.Sprint(num(t, r, "cycle_index"))
		if _, ok := cycleFirst[c]; !ok {
			cycleFirst[c] = after - delta
		}
		cycleSum[c] += delta
	}
	if at != final {
		t.Errorf("the history ends at %d and the store held %d", at, final)
	}
	last := rows[len(rows)-1]
	c := str(last, "cycle_start") + "#" + fmt.Sprint(num(t, last, "cycle_index"))
	if cycleFirst[c]+cycleSum[c] != num(t, last, "balance_after") {
		t.Errorf("the open cycle's deltas sum to %d from %d, and its last balance_after is %d", cycleSum[c],
			cycleFirst[c], num(t, last, "balance_after"))
	}

	// Every administrative row has exactly one audit row under its request.
	audit := mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/audit?limit=200&tenant_external_id="+tn.External, ""),
		http.StatusOK)
	byRequest := map[string]int{}
	for _, e := range audit["entries"].([]any) {
		byRequest[str(e.(map[string]any), "request_id")]++
	}
	administrative := 0
	for _, r := range rows {
		if str(r, "source") == "admin" || (str(r, "source") == "system" && str(r, "event_type") == "refunded") {
			administrative++
			if n := byRequest[str(r, "request_id")]; n != 1 {
				t.Errorf("%s %s (%s) has %d audit rows under %s", str(r, "event_type"), str(r, "id"), str(r, "source"),
					n, str(r, "request_id"))
			}
		}
	}
	if administrative < 5 {
		t.Errorf("only %d administrative rows: %s", administrative, mustJSON(rows))
	}
	// The answer read nothing from the store: it is still empty.
	if keys := s.tenantKeys(tn.ID); len(keys) != 0 {
		t.Errorf("answering the dispute wrote %v", keys)
	}
}
