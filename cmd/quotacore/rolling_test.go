package main

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/quotacore/quotacore/internal/store"
)

// rollingTenant provisions, through the admin API, one tenant on a plan with
// a metered feature (daily, 100) and a rolling one (rolling_5m, 10), which is
// IP-19 DoD 2's fixed window and sliding window side by side.
func (s *stack) rollingTenant() (tn tenant, fixed, sliding string) {
	s.t.Helper()
	fixed, sliding = uniq("rm"), uniq("rr")
	s.createFeature(fixed)
	s.createRollingFeature(sliding)
	plan := uniq("rp")
	p := mustStatus(s.t, s.adminDo(http.MethodPost, "/v1/admin/plans", `{"plan_key":"`+plan+`","name":"P","entitlements":[`+
		`{"feature_key":"`+fixed+`","limit":100,"reset_interval":"daily"},`+
		`{"feature_key":"`+sliding+`","limit":10,"reset_interval":null,"window":"rolling_5m"}]}`), http.StatusCreated)
	for _, e := range p["entitlements"].([]any) {
		m := e.(map[string]any)
		switch str(m, "feature_key") {
		case sliding:
			if m["window"] != "rolling_5m" || m["reset_interval"] != nil || num(s.t, m, "limit") != 10 {
				s.t.Fatalf("the rolling entitlement reads back as %v", m)
			}
		case fixed:
			if _, present := m["window"]; present {
				s.t.Fatalf("a metered entitlement reads back with a window: %v", m)
			}
		}
	}
	tn = asTenant(s.t, s.createTenant(externalID("rt"), plan, "UTC"))
	return tn, fixed, sliding
}

func windowed(feature string, amount int64, external, window string) string {
	return `{"feature_key":"` + feature + `","amount":` + strconv.FormatInt(amount, 10) +
		`,"tenant_external_id":"` + external + `","window":"` + window + `"}`
}

// TestT18AFixedAndASlidingWindowCoexist is IP-19 DoD 2: one tenant holds a
// metered and a rolling feature, each enforced by its own cadence, each in
// exactly one key of its own, and the balance route tells them apart. A
// rolling call writes no ledger row (Q-37).
func TestT18AFixedAndASlidingWindowCoexist(t *testing.T) {
	s := newStack(t)
	tn, fixed, sliding := s.rollingTenant()
	ctx := context.Background()

	if r := s.consume(s.runtime, fixed, 30, tn.External, newKey()); r.Status != http.StatusOK ||
		num(t, r.json(t), "balance") != 70 {
		t.Fatalf("metered consume: %d %s", r.Status, r.Body)
	}
	s.flush()
	ledger := s.rows(`SELECT count(*) FROM usage_events WHERE tenant_id = $1`, tn.ID)

	key := newKey()
	r := s.do(http.MethodPost, "/v1/consume", s.runtime, windowed(sliding, 4, tn.External, "rolling_5m"), "Idempotency-Key", key)
	b := mustStatus(t, r, http.StatusOK)
	if b["kind"] != "rolling" || num(t, b, "balance") != 6 || num(t, b, "limit") != 10 || b["bonus"] != nil ||
		b["cycle_start"] != nil || b["cycle_end"] != nil || b["window"] != "rolling_5m" ||
		num(t, b, "window_precision_seconds") != 5 || b["replayed"] != false {
		t.Fatalf("rolling consume: %s", r.Body)
	}
	again := s.do(http.MethodPost, "/v1/consume", s.runtime, windowed(sliding, 4, tn.External, "rolling_5m"), "Idempotency-Key", key)
	if b := mustStatus(t, again, http.StatusOK); b["replayed"] != true || num(t, b, "balance") != 6 {
		t.Errorf("a retried rolling consume: %s", again.Body)
	}
	c := s.do(http.MethodPost, "/v1/check", s.runtime, windowed(sliding, 7, tn.External, "rolling_5m"))
	if b := mustStatus(t, c, http.StatusOK); b["allowed"] != false || b["kind"] != "rolling" || num(t, b, "balance") != 6 {
		t.Errorf("check 7 of 6 left: %s", c.Body)
	}
	denied := s.do(http.MethodPost, "/v1/consume", s.runtime, windowed(sliding, 7, tn.External, "rolling_5m"), "Idempotency-Key", newKey())
	if denied.Status != http.StatusTooManyRequests || denied.code(t) != "quota_exceeded" {
		t.Fatalf("consume 7 of 6 left: %d %s", denied.Status, denied.Body)
	}
	details := denied.json(t)["error"].(map[string]any)["details"].(map[string]any)
	if details["window"] != "rolling_5m" || details["next_release_at"] == nil {
		t.Errorf("a rolling denial names its window and its next release: %v", details)
	}
	ref := s.do(http.MethodPost, "/v1/refund", s.runtime, windowed(sliding, 1, tn.External, "rolling_5m"), "Idempotency-Key", newKey())
	if b := mustStatus(t, ref, http.StatusOK); num(t, b, "balance") != 7 {
		t.Errorf("refund 1: %s", ref.Body)
	}
	over := s.do(http.MethodPost, "/v1/refund", s.runtime, windowed(sliding, 9, tn.External, "rolling_5m"), "Idempotency-Key", newKey())
	if over.Status != http.StatusConflict || over.code(t) != "refund_exceeds_grant" {
		t.Errorf("refund 9 of 3 counted: %d %s", over.Status, over.Body)
	}

	bal := s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External, s.runtime, "")
	entries, _ := mustStatus(t, bal, http.StatusOK)["entries"].([]any)
	got := map[string]map[string]any{}
	for _, e := range entries {
		m := e.(map[string]any)
		got[str(m, "feature_key")] = m
	}
	if e := got[fixed]; e["kind"] != "metered" || num(t, e, "balance") != 70 || e["cycle_end"] == nil {
		t.Errorf("the metered entry: %v", e)
	} else if _, present := e["window"]; present {
		t.Errorf("the metered entry has a window: %v", e)
	}
	if e := got[sliding]; e["kind"] != "rolling" || num(t, e, "balance") != 7 || num(t, e, "consumed") != 3 ||
		e["window"] != "rolling_5m" || num(t, e, "window_precision_seconds") != 5 || e["cycle_end"] != nil || e["bonus"] != nil {
		t.Errorf("the rolling entry: %v", e)
	}

	// One key per tenant-feature: the metered feature's balance hash, and the
	// rolling feature's bucket hash, and nothing else for either.
	bucket, _ := store.BucketCounterKey(tn.ID, sliding)
	marker, _ := store.WindowMarkerKey(tn.ID, sliding)
	for key, want := range map[string]int64{
		mustKey(t, tn.ID, fixed): 1, bucket: 1, mustKey(t, tn.ID, sliding): 0, marker: 0,
	} {
		if n, err := s.rdb.Exists(ctx, key).Result(); err != nil || n != want {
			t.Errorf("%s exists %d, want %d: %v", key, n, want, err)
		}
	}
	s.flush()
	if n := s.rows(`SELECT count(*) FROM usage_events WHERE tenant_id = $1`, tn.ID); n != ledger {
		t.Errorf("rolling calls wrote %d ledger rows; a window has no cycle to reconcile (Q-37)", n-ledger)
	}
}

// TestT18InvalidWindow is IP-19 DoD 7: a window outside the supported set, a
// window sent to a metered feature, none sent to a rolling one, and a
// different one, are each 400 invalid_window, and none of them changes
// anything.
func TestT18InvalidWindow(t *testing.T) {
	s := newStack(t)
	tn, fixed, sliding := s.rollingTenant()
	cases := []struct{ name, body string }{
		{"a window outside the set", windowed(sliding, 1, tn.External, "rolling_2h")},
		{"a window that is not a string", `{"feature_key":"` + sliding + `","amount":1,"tenant_external_id":"` + tn.External + `","window":3600}`},
		{"a window on a metered feature", windowed(fixed, 1, tn.External, "rolling_5m")},
		{"no window on a rolling feature", metered(sliding, 1, tn.External)},
		{"a different window", windowed(sliding, 1, tn.External, "rolling_1h")},
	}
	for _, c := range cases {
		for _, route := range []string{"/v1/consume", "/v1/refund", "/v1/check"} {
			r := s.do(http.MethodPost, route, s.runtime, c.body, "Idempotency-Key", newKey())
			if r.Status != http.StatusBadRequest || r.code(t) != "invalid_window" {
				t.Errorf("%s on %s: %d %s, want 400 invalid_window", c.name, route, r.Status, r.Body)
			}
		}
	}
	bucket, _ := store.BucketCounterKey(tn.ID, sliding)
	if n, err := s.rdb.Exists(context.Background(), bucket).Result(); err != nil || n != 0 {
		t.Errorf("a refused window wrote the bucket hash: %d %v", n, err)
	}
	bal := s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External+"&feature_key="+fixed, s.runtime, "")
	entries, _ := mustStatus(t, bal, http.StatusOK)["entries"].([]any)
	if len(entries) != 1 || num(t, entries[0].(map[string]any), "balance") != 100 {
		t.Errorf("a refused window moved the metered balance: %s", bal.Body)
	}
}
