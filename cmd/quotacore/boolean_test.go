package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/quotacore/quotacore/internal/store"
)

// booleanTenant provisions, through the admin API, one tenant on a plan with
// one boolean feature and three metered ones on three cadences, one of them a
// limit of 0 (IP-17 DoD 5, UC-02). It returns the tenant and the feature keys:
// boolean, zero (monthly, limit 0), one (daily, limit 1) and life (never, 50).
func (s *stack) booleanTenant() (tn tenant, boolean, zero, one, life string) {
	s.t.Helper()
	boolean, zero, one, life = uniq("bo"), uniq("bz"), uniq("b1"), uniq("bl")
	s.createBooleanFeature(boolean)
	for _, f := range []string{zero, one, life} {
		s.createFeature(f)
	}
	plan := uniq("bp")
	mustStatus(s.t, s.adminDo(http.MethodPost, "/v1/admin/plans", `{"plan_key":"`+plan+`","name":"P","entitlements":[`+
		`{"feature_key":"`+boolean+`","limit":null,"reset_interval":null},`+
		`{"feature_key":"`+zero+`","limit":0,"reset_interval":"monthly"},`+
		`{"feature_key":"`+one+`","limit":1,"reset_interval":"daily"},`+
		`{"feature_key":"`+life+`","limit":50,"reset_interval":"never"}]}`), http.StatusCreated)
	tn = asTenant(s.t, s.createTenant(externalID("bt"), plan, "UTC"))
	return tn, boolean, zero, one, life
}

// TestT16BooleanEntitlementHasNoCounter is T-16's first half, IP-17 DoD 1 and
// 2 (DR-050): every check and every consume of 1 is allowed, the balance has
// no ceiling, no bonus and no exhaustion, no balance hash exists, a retry
// replays, and a consume of 5000 or any refund names the mismatch and records
// nothing.
func TestT16BooleanEntitlementHasNoCounter(t *testing.T) {
	s := newStack(t)
	tn, boolean, _, _, _ := s.booleanTenant()

	for i := 0; i < 50; i++ {
		for _, amount := range []int64{0, 1} {
			r := s.do(http.MethodPost, "/v1/check", s.runtime, metered(boolean, amount, tn.External))
			b := mustStatus(t, r, http.StatusOK)
			if b["allowed"] != true || b["kind"] != "boolean" || b["balance"] != nil || b["limit"] != nil ||
				b["bonus"] != nil || b["cycle_end"] != nil {
				t.Fatalf("check %d of a boolean: %s", amount, r.Body)
			}
		}
		r := s.consume(s.runtime, boolean, 1, tn.External, newKey())
		b := mustStatus(t, r, http.StatusOK)
		if b["kind"] != "boolean" || b["balance"] != nil || b["limit"] != nil || b["bonus"] != nil ||
			num(t, b, "consumed") != 1 || b["cycle_start"] != nil || b["replayed"] != false {
			t.Fatalf("consume 1 of a boolean: %s", r.Body)
		}
	}

	key := newKey()
	first := mustStatus(t, s.consume(s.runtime, boolean, 1, tn.External, key), http.StatusOK)
	again := s.consume(s.runtime, boolean, 1, tn.External, key)
	if b := mustStatus(t, again, http.StatusOK); b["replayed"] != true || again.Header.Get("Idempotent-Replay") != "true" ||
		first["replayed"] != false {
		t.Errorf("a retried boolean consume is not a replay: %s", again.Body)
	}
	if r := s.consume(s.runtime, boolean, 0, tn.External, key); r.code(t) != "idempotency_key_reuse" {
		t.Errorf("the key on a different amount: %d %s", r.Status, r.Body)
	}

	exists, err := s.rdb.Exists(context.Background(), mustKey(t, tn.ID, boolean)).Result()
	if err != nil || exists != 0 {
		t.Errorf("a boolean entitlement has a balance hash: %d %v", exists, err)
	}

	r := s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External+"&feature_key="+boolean, s.runtime, "")
	entries, _ := mustStatus(t, r, http.StatusOK)["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("balance of a boolean: %s", r.Body)
	}
	e := entries[0].(map[string]any)
	for _, f := range []string{"balance", "limit", "bonus", "consumed", "cycle_start", "cycle_end"} {
		if v, present := e[f]; !present || v != nil {
			t.Errorf("a boolean's balance entry has %s = %v, want null: %s", f, v, r.Body)
		}
	}
	if e["kind"] != "boolean" {
		t.Errorf("a boolean's balance entry has kind %v", e["kind"])
	}

	s.flush()
	ledger := s.rows(`SELECT count(*) FROM usage_events WHERE tenant_id = $1`, tn.ID)
	refused := []struct {
		name string
		send func() response
	}{
		{"consume 5000", func() response { return s.consume(s.runtime, boolean, 5000, tn.External, newKey()) }},
		{"consume 2", func() response { return s.consume(s.runtime, boolean, 2, tn.External, newKey()) }},
		{"check 5000", func() response {
			return s.do(http.MethodPost, "/v1/check", s.runtime, metered(boolean, 5000, tn.External))
		}},
		{"refund 1", func() response {
			return s.do(http.MethodPost, "/v1/refund", s.runtime, metered(boolean, 1, tn.External), "Idempotency-Key", newKey())
		}},
	}
	for _, c := range refused {
		r := c.send()
		if r.Status != http.StatusConflict || r.code(t) != "feature_not_metered" {
			t.Errorf("%s of a boolean: %d %s, want 409 feature_not_metered", c.name, r.Status, r.Body)
		}
	}
	reused := newKey()
	if r := s.consume(s.runtime, boolean, 5000, tn.External, reused); r.code(t) != "feature_not_metered" {
		t.Fatalf("consume 5000: %s", r.Body)
	}
	// The refusal consumed no key: the same key then applies (DR-025).
	if r := s.consume(s.runtime, boolean, 1, tn.External, reused); r.Status != http.StatusOK {
		t.Errorf("a key a refusal saw was then refused: %d %s", r.Status, r.Body)
	}
	s.flush()
	if n := s.rows(`SELECT count(*) FROM usage_events WHERE tenant_id = $1`, tn.ID); n != ledger {
		t.Errorf("boolean calls wrote %d ledger rows; an inclusion has no delta (DR-042, Q-33)", n-ledger)
	}
}

// TestT16NotInPlanIsNotExhausted is T-16's second half, IP-17 DoD 3 (DR-012,
// UC-02): an absent feature is 403 feature_not_in_plan and a present one with
// nothing left is 429 quota_exceeded, and the two are told apart in the
// response, in quotacore_errors_total and in the ledger. Neither refusal
// writes a row (DR-025); the exhausted feature has the consumption that
// exhausted it, and the absent one has nothing at all.
func TestT16NotInPlanIsNotExhausted(t *testing.T) {
	s := newStack(t)
	tn, _, zero, one, _ := s.booleanTenant()
	absent := uniq("ba")
	s.createFeature(absent)

	if r := s.consume(s.runtime, one, 1, tn.External, newKey()); r.Status != http.StatusOK {
		t.Fatalf("spend the one unit: %d %s", r.Status, r.Body)
	}
	s.flush()
	ledger := s.rows(`SELECT count(*) FROM usage_events WHERE tenant_id = $1`, tn.ID)
	notInPlan := s.counter("quotacore_errors_total", "feature_not_in_plan", routeConsume)
	exceeded := s.counter("quotacore_errors_total", "quota_exceeded", routeConsume)

	a := s.consume(s.runtime, absent, 1, tn.External, newKey())
	if a.Status != http.StatusForbidden || a.code(t) != "feature_not_in_plan" {
		t.Errorf("an absent feature: %d %s, want 403 feature_not_in_plan", a.Status, a.Body)
	}
	for _, f := range []string{zero, one} {
		r := s.consume(s.runtime, f, 1, tn.External, newKey())
		if r.Status != http.StatusTooManyRequests || r.code(t) != "quota_exceeded" {
			t.Errorf("an exhausted feature %s: %d %s, want 429 quota_exceeded", f, r.Status, r.Body)
		}
	}
	if got := s.counter("quotacore_errors_total", "feature_not_in_plan", routeConsume) - notInPlan; got != 1 {
		t.Errorf("errors_total{code=feature_not_in_plan} rose by %v, want 1", got)
	}
	if got := s.counter("quotacore_errors_total", "quota_exceeded", routeConsume) - exceeded; got != 2 {
		t.Errorf("errors_total{code=quota_exceeded} rose by %v, want 2", got)
	}

	s.flush()
	if n := s.rows(`SELECT count(*) FROM usage_events WHERE tenant_id = $1`, tn.ID); n != ledger {
		t.Errorf("the refusals wrote %d ledger rows (DR-025)", n-ledger)
	}
	byFeature := func(f string) int {
		return s.rows(`SELECT count(*) FROM usage_events u JOIN features f ON f.id = u.feature_id
 WHERE u.tenant_id = $1 AND f.key = $2`, tn.ID, f)
	}
	if n := byFeature(one); n == 0 {
		t.Error("the exhausted feature has no consumption in the ledger")
	}
	if n := byFeature(absent); n != 0 {
		t.Errorf("the absent feature has %d ledger rows", n)
	}
}

// TestT16MixedPlanEnforcesEachByItsType is IP-17 DoD 5: one tenant with one
// boolean and three metered entitlements on three cadences, each enforced by
// its own type, and the balance route reporting all four correctly.
func TestT16MixedPlanEnforcesEachByItsType(t *testing.T) {
	s := newStack(t)
	tn, boolean, zero, one, life := s.booleanTenant()

	if r := s.consume(s.runtime, boolean, 1, tn.External, newKey()); r.Status != http.StatusOK {
		t.Errorf("boolean: %d %s", r.Status, r.Body)
	}
	if r := s.consume(s.runtime, zero, 1, tn.External, newKey()); r.code(t) != "quota_exceeded" {
		t.Errorf("limit 0: %d %s", r.Status, r.Body)
	}
	if r := s.consume(s.runtime, life, 20, tn.External, newKey()); r.Status != http.StatusOK || num(t, r.json(t), "balance") != 30 {
		t.Errorf("never: %d %s", r.Status, r.Body)
	}
	if r := s.consume(s.runtime, one, 2, tn.External, newKey()); r.code(t) != "quota_exceeded" {
		t.Errorf("daily, 2 of 1: %d %s", r.Status, r.Body)
	}

	r := s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External, s.runtime, "")
	entries, _ := mustStatus(t, r, http.StatusOK)["entries"].([]any)
	if len(entries) != 4 {
		t.Fatalf("the balance route lists %d entries, want 4: %s", len(entries), r.Body)
	}
	got := map[string]map[string]any{}
	for _, e := range entries {
		m := e.(map[string]any)
		got[str(m, "feature_key")] = m
	}
	if e := got[boolean]; e["kind"] != "boolean" || e["balance"] != nil || e["limit"] != nil {
		t.Errorf("boolean entry: %v", e)
	}
	want := map[string][3]any{zero: {"metered", float64(0), float64(0)}, one: {"metered", float64(1), float64(1)},
		life: {"metered", float64(30), float64(50)}}
	for f, w := range want {
		if e := got[f]; e["kind"] != w[0] || e["balance"] != w[1] || e["limit"] != w[2] {
			t.Errorf("%s entry: %v, want kind %v balance %v limit %v", f, e, w[0], w[1], w[2])
		}
	}
	if e := got[life]; e["cycle_end"] != nil {
		t.Errorf("the never entry has a cycle end: %v", e)
	}
	if e := got[one]; e["cycle_end"] == nil {
		t.Errorf("the daily entry has no cycle end: %v", e)
	}
}

func mustKey(t *testing.T, tenantID, feature string) string {
	t.Helper()
	k, err := store.BalanceKey(tenantID, feature)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
