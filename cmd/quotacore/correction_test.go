package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// IP-18's definition of done: `set` and `grant` as first-class operations
// (DR-019, DR-020, DR-041, DR-042, DR-051, T-17).

// window is the part of a balance hash and a tenant row that says which cycle
// the tenant is on. Neither a grant nor a set may move any of it (IP-18 DoD 4).
type window struct {
	Start, End, Index string
	Anchor            time.Time
}

func (s *stack) windowOf(tn tenant, feature string) window {
	s.t.Helper()
	h := s.state(tn, feature).Hash
	return window{Start: h["window_start"], End: h["window_end"], Index: h["cycle_index"], Anchor: s.anchorOf(tn.ID)}
}

// auditRow is one audit_log row of a balance route, decoded.
type auditRow struct {
	Before, After struct {
		Balance    int64  `json:"balance"`
		Limit      *int64 `json:"limit"`
		Bonus      *int64 `json:"bonus"`
		CycleIndex int64  `json:"cycle_index"`
	}
	Reason, RequestID string
	ByAdminKey        bool
}

// audits is every audit row of one action on one tenant, oldest first.
func (s *stack) audits(action, tenantID string) []auditRow {
	s.t.Helper()
	rows, err := s.seedDB.QueryContext(context.Background(), `
SELECT a.before::text, a.after::text, coalesce(a.reason, ''), a.request_id,
       EXISTS (SELECT 1 FROM api_keys k WHERE k.id = a.actor_key_id AND k.scope = 'admin')
  FROM audit_log a WHERE a.action = $1 AND a.subject_id = $2 ORDER BY a.created_at, a.id`, action, tenantID)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		var before, after string
		if err := rows.Scan(&before, &after, &r.Reason, &r.RequestID, &r.ByAdminKey); err != nil {
			s.t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(before), &r.Before); err != nil {
			s.t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(after), &r.After); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

func deref(p *int64) string {
	if p == nil {
		return "absent"
	}
	return fmt.Sprint(*p)
}

// sides says whether an audit row records the before and after a test
// expects, as (balance, limit, bonus) on each side.
func sides(r auditRow, before, after [3]int64) bool {
	got := fmt.Sprintf("%d %s %s / %d %s %s", r.Before.Balance, deref(r.Before.Limit), deref(r.Before.Bonus),
		r.After.Balance, deref(r.After.Limit), deref(r.After.Bonus))
	want := fmt.Sprintf("%d %d %d / %d %d %d", before[0], before[1], before[2], after[0], after[1], after[2])
	return got == want
}

// TestT17ACorrectionReconciles is T-17's integration half, IP-18 DoD 1, 3, 4
// and 5 and the exit criterion: on a limit of 1000 after a consume of 300, a
// grant of 50, a set down to 400, a set up to 900 and a set to the same 900
// each answer 200; each is audited once with before and after balance, limit
// and bonus, its reason, and the admin key that made it; the ledger holds a
// granted +50 and set deltas of -350 and +500 and nothing for the equal set;
// the cycle reconciles (DR-042, INV-C4) and verify-tenant is clean; each
// ledger row joins its audit row by request id, so an operator can show a
// customer why the balance moved; and no grant or set moves the window or the
// anchor.
func TestT17ACorrectionReconciles(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(1000)
	tp := "/v1/admin/tenants/" + tn.ID
	mustStatus(t, s.consume(s.runtime, f, 300, tn.External, newKey()), http.StatusOK)
	start := s.windowOf(tn, f)

	type step struct {
		path, body, action, reason string
		before, after              [3]int64
	}
	steps := []step{
		{"/grant", `"amount":50`, "tenant.grant", "goodwill for INC-1", [3]int64{700, 1000, 0}, [3]int64{750, 1000, 50}},
		{"/set", `"value":400`, "tenant.set", "migration overcounted", [3]int64{750, 1000, 50}, [3]int64{400, 1000, 50}},
		{"/set", `"value":900`, "tenant.set", "migration undercounted", [3]int64{400, 1000, 50}, [3]int64{900, 1000, 50}},
		{"/set", `"value":900`, "tenant.set", "confirmed", [3]int64{900, 1000, 50}, [3]int64{900, 1000, 50}},
	}
	requests := map[string]bool{}
	for i, st := range steps {
		r := s.adminDo(http.MethodPost, tp+st.path, `{"feature_key":"`+f+`",`+st.body+`,"reason":"`+st.reason+`"}`)
		b := mustStatus(t, r, http.StatusOK)
		if num(t, b, "balance") != st.after[0] || num(t, b, "bonus") != st.after[2] {
			t.Errorf("step %d answered %s", i, r.Body)
		}
		if w := s.windowOf(tn, f); w != start {
			t.Errorf("step %d moved the window from %+v to %+v (DoD 4)", i, start, w)
		}
		requests[r.Header.Get("X-Request-Id")] = true
	}

	grants, sets := s.audits("tenant.grant", tn.ID), s.audits("tenant.set", tn.ID)
	if len(grants) != 1 || len(sets) != 3 {
		t.Fatalf("audited %d grants and %d sets, want 1 and 3", len(grants), len(sets))
	}
	for i, a := range append(grants, sets...) {
		st := steps[i]
		if !sides(a, st.before, st.after) || a.Reason != st.reason || !a.ByAdminKey || !requests[a.RequestID] ||
			a.Before.CycleIndex != a.After.CycleIndex {
			t.Errorf("the audit row of step %d is %+v, want before %v and after %v, reason %q, by an admin key",
				i, a, st.before, st.after, st.reason)
		}
	}

	rows := rowsFor(s.ledger(tn), f)
	if !sameShapes(rows, expect{"consumed", "api", -300, 700}, expect{"granted", "admin", 50, 750},
		expect{"set", "admin", -350, 400}, expect{"set", "admin", 500, 900}) {
		t.Fatalf("the ledger is %v", shapes(rows))
	}
	var sum int64
	for _, r := range rows {
		sum += r.Delta
	}
	if got := s.balance(tn, f); got != 1000+sum {
		t.Errorf("balance %d is not the opening 1000 plus the deltas %d (DR-042, INV-C4)", got, sum)
	}
	audited := map[string]bool{}
	for _, a := range append(grants, sets...) {
		audited[a.RequestID] = true
	}
	for _, r := range rows[1:] {
		if !audited[r.RequestID] {
			t.Errorf("the %s row of %+d joins no audit row by request id %q", r.Type, r.Delta, r.RequestID)
		}
	}

	time.Sleep(s.settle + 100*time.Millisecond)
	c, out := s.commands()
	if code, err := c.run(context.Background(), []string{"verify-tenant", tn.External}); err != nil || code != exitClean {
		t.Errorf("verify-tenant after the corrections: exit %d, %v: %s", code, err, out)
	}
}

// TestT17ARefusedCorrectionChangesNothing is IP-18 DoD 3 (DR-019, T-04): a set
// above limit plus bonus and a grant past 2^53 - 1 are 409
// grant_exceeds_ceiling, and leave the hash, the audit log and the ledger as
// they were.
func TestT17ARefusedCorrectionChangesNothing(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	tp := "/v1/admin/tenants/" + tn.ID
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/grant", `{"feature_key":"`+f+`","amount":5,"reason":"x"}`), http.StatusOK)
	s.flush()
	hash := fmt.Sprint(s.state(tn, f).Hash)
	audits := s.rows(`SELECT count(*) FROM audit_log WHERE subject_id = $1`, tn.ID)
	events := s.rows(`SELECT count(*) FROM usage_events WHERE tenant_id = $1`, tn.ID)

	for _, c := range []struct{ path, body string }{
		{"/set", `"value":106`},
		{"/grant", `"amount":9007199254740991`},
	} {
		r := s.adminDo(http.MethodPost, tp+c.path, `{"feature_key":"`+f+`",`+c.body+`,"reason":"x"}`)
		if r.Status != http.StatusConflict || r.code(t) != "grant_exceeds_ceiling" {
			t.Errorf("%s %s answered %d %s, want 409 grant_exceeds_ceiling", c.path, c.body, r.Status, r.Body)
		}
	}
	s.flush()
	if got := fmt.Sprint(s.state(tn, f).Hash); got != hash {
		t.Errorf("a refused correction changed the hash from %s to %s", hash, got)
	}
	if n := s.rows(`SELECT count(*) FROM audit_log WHERE subject_id = $1`, tn.ID); n != audits {
		t.Errorf("a refused correction wrote %d audit rows", n-audits)
	}
	if n := s.rows(`SELECT count(*) FROM usage_events WHERE tenant_id = $1`, tn.ID); n != events {
		t.Errorf("a refused correction wrote %d ledger rows", n-events)
	}
	// A set at the ceiling exactly is a set.
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/set", `{"feature_key":"`+f+`","value":105,"reason":"x"}`), http.StatusOK)
}

// TestT17ACorrectionIsReversedByASet is DR-051: there is no undo route. A set
// is reversed by a set to the before its audit row records, and a grant by a
// set to the balance less the amount, which leaves the grant's bonus, and so
// its ceiling, in place until the boundary (Q-35). Each reversal is audited
// and ledgered like the change it reverses.
func TestT17ACorrectionIsReversedByASet(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	tp := "/v1/admin/tenants/" + tn.ID
	mustStatus(t, s.consume(s.runtime, f, 40, tn.External, newKey()), http.StatusOK)

	mustStatus(t, s.adminDo(http.MethodPost, tp+"/set", `{"feature_key":"`+f+`","value":10,"reason":"wrong tenant"}`), http.StatusOK)
	sets := s.audits("tenant.set", tn.ID)
	if len(sets) != 1 || sets[0].Before.Balance != 60 {
		t.Fatalf("the set's audit row is %+v", sets)
	}
	undo := fmt.Sprintf(`{"feature_key":%q,"value":%d,"reason":"reverse the set"}`, f, sets[0].Before.Balance)
	if b := mustStatus(t, s.adminDo(http.MethodPost, tp+"/set", undo), http.StatusOK); num(t, b, "balance") != 60 {
		t.Errorf("reversing the set answered %v", b)
	}

	g := mustStatus(t, s.adminDo(http.MethodPost, tp+"/grant", `{"feature_key":"`+f+`","amount":30,"reason":"goodwill"}`), http.StatusOK)
	back := fmt.Sprintf(`{"feature_key":%q,"value":%d,"reason":"reverse the grant"}`, f, num(t, g, "balance")-30)
	if b := mustStatus(t, s.adminDo(http.MethodPost, tp+"/set", back), http.StatusOK); num(t, b, "balance") != 60 || num(t, b, "bonus") != 30 {
		t.Errorf("reversing the grant answered %v, want balance 60 with the bonus of 30 left until the boundary", b)
	}
	if rows := rowsFor(s.ledger(tn), f); !sameShapes(rows, expect{"consumed", "api", -40, 60}, expect{"set", "admin", -50, 10},
		expect{"set", "admin", 50, 60}, expect{"granted", "admin", 30, 90}, expect{"set", "admin", -30, 60}) {
		t.Errorf("the ledger is %v", shapes(rows))
	}
	if n := len(s.audits("tenant.set", tn.ID)); n != 3 {
		t.Errorf("%d set audit rows, want 3: each reversal is audited", n)
	}
}
