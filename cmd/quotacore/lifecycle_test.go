package main

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/store"
)

// IP-10's definition of done (plan lifecycle and cycle-boundary semantics).
// Windows are moved as IP-07's boundary tests move them: a tenant is seeded
// with an anchor in the past and a stored window, so the real clock sits where
// the test needs it.

// tenantOn seeds a tenant on an existing plan with the anchor given, pinned to
// the plan's cadences as provisioning pins it, and with no balance hash.
func (s *stack) tenantOn(planID, zone string, anchor time.Time, ents ...ent) tenant {
	s.t.Helper()
	ctx := context.Background()
	tn := tenant{External: externalID("lc"), Zone: zone, Anchor: anchor, ents: map[string]ent{}}
	for _, e := range ents {
		tn.ents[e.feature] = e
	}
	if err := s.seedDB.QueryRowContext(ctx, `INSERT INTO tenants (external_id, display_name, plan_id, timezone, anchor_at)
		VALUES ($1, 'T', $2, $3, $4) RETURNING id`, tn.External, planID, zone, anchor).Scan(&tn.ID); err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.seedDB.ExecContext(ctx, `INSERT INTO tenant_entitlement_pins (tenant_id, feature_id, interval)
		SELECT $1, feature_id, interval FROM plan_entitlements WHERE plan_id = $2`, tn.ID, planID); err != nil {
		s.t.Fatal(err)
	}
	if err := s.rdb.HSet(ctx, store.ConfigTenantsKey(), tn.ID, 1).Err(); err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { s.rdb.HDel(context.Background(), store.ConfigTenantsKey(), tn.ID) })
	return tn
}

// planEdit replaces a plan's entitlements, each "feature:limit:interval".
func (s *stack) planEdit(plan string, ents ...string) map[string]any {
	s.t.Helper()
	path := "/v1/admin/plans/" + plan
	var out []string
	for _, e := range ents {
		p := strings.Split(e, ":")
		out = append(out, fmt.Sprintf(`{"feature_key":%q,"limit":%s,"reset_interval":%q}`, p[0], p[1], p[2]))
	}
	return mustStatus(s.t, s.adminDo(http.MethodPatch, path,
		fmt.Sprintf(`{"version":%d,"entitlements":[%s]}`, s.version(path), strings.Join(out, ","))), http.StatusOK)
}

// anchorOf is a tenant's anchor as Postgres holds it.
func (s *stack) anchorOf(id string) time.Time {
	s.t.Helper()
	var at time.Time
	if err := s.seedDB.QueryRow(`SELECT anchor_at FROM tenants WHERE id = $1`, id).Scan(&at); err != nil {
		s.t.Fatal(err)
	}
	return at
}

// rows counts the rows a query selects.
func (s *stack) rows(query string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.seedDB.QueryRow(query, args...).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// pinsOf is a tenant's pin rows, rendered.
func (s *stack) pinsOf(id string) string {
	s.t.Helper()
	var out string
	if err := s.seedDB.QueryRow(`SELECT coalesce(string_agg(p::text, ',' ORDER BY p::text), '') FROM tenant_entitlement_pins p
		WHERE tenant_id = $1`, id).Scan(&out); err != nil {
		s.t.Fatal(err)
	}
	return out
}

func ms(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

// eventsOf is the deltas of the events of one type handed to the sink for
// one tenant, on one feature or, with "", on any.
func (s *stack) eventsOf(tenantID, feature, typ string) []int64 {
	s.events.mu.Lock()
	defer s.events.mu.Unlock()
	var deltas []int64
	for _, e := range s.events.events {
		if e.TenantID == tenantID && e.Type == typ && (feature == "" || e.FeatureKey == feature) {
			deltas = append(deltas, e.Delta)
		}
	}
	return deltas
}

// TestPlanEditLandsAtEachTenantsOwnBoundary is IP-10 DoD 1 (DR-014, T-05, E-2,
// E-3): an edit moves no field and no expiry of any assigned tenant's hash,
// each of three tenants anchored across a month takes the new limit at its
// own boundary and keeps its own cadence, and a tenant created after the edit
// gets the new limit and interval at once.
func TestPlanEditLandsAtEachTenantsOwnBoundary(t *testing.T) {
	s := newStack(t)
	f, plan := uniq("ef"), uniq("ep")
	s.createFeature(f)
	p := s.createPlan(plan, 100, "monthly", f)
	now := time.Now()
	var tenants []tenant
	for _, back := range []int{3, 13, 23} {
		tn := s.tenantOn(str(p, "id"), "UTC", now.AddDate(0, -2, -back), ent{f, 100, "monthly", false})
		s.seedCurrent(tn, f, 40)
		tenants = append(tenants, tn)
	}
	before := map[string]state{}
	for _, tn := range tenants {
		before[tn.ID] = s.state(tn, f)
	}

	s.planEdit(plan, f+":500:daily")
	for _, tn := range tenants {
		if after := s.state(tn, f); !reflect.DeepEqual(after.Hash, before[tn.ID].Hash) || after.Expiry != before[tn.ID].Expiry {
			t.Errorf("the edit moved %s's hash: %v %v, then %v %v", tn.ID, before[tn.ID].Hash, before[tn.ID].Expiry,
				after.Hash, after.Expiry)
		}
	}

	for _, tn := range tenants {
		// Inside the window the old limit stands and the boundary does not move.
		mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
		h := s.state(tn, f).Hash
		if h["limit"] != "100" || h["balance"] != "39" || h["window_end"] != before[tn.ID].Hash["window_end"] {
			t.Errorf("%s inside its window after the edit: %v", tn.ID, h)
		}
		// At its own boundary it takes the new limit, on its own monthly cadence.
		cur := s.currentWindow(tn, f, time.Now())
		prevStart := cur.Start.AddDate(0, -1, 0)
		s.seed(tn, f, stored{Balance: 3, Limit: 100, Index: cur.Index - 1, Start: prevStart, End: &cur.Start})
		r := mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
		h = s.state(tn, f).Hash
		want := map[string]string{"balance": "499", "limit": "500", "bonus": "0", "cycle_index": fmt.Sprint(cur.Index),
			"window_start": ms(cur.Start), "window_end": ms(*cur.End)}
		if !reflect.DeepEqual(h, want) {
			t.Errorf("%s at its boundary: %v (response %v), want %v", tn.ID, h, r, want)
		}
	}

	fresh := asTenant(t, s.createTenant(externalID("after"), plan, "UTC"), ent{f, 500, "daily", false})
	h := s.state(fresh, f).Hash
	start, _ := strconv.ParseInt(h["window_start"], 10, 64)
	end, _ := strconv.ParseInt(h["window_end"], 10, 64)
	if h["limit"] != "500" || h["balance"] != "500" || time.Duration(end-start)*time.Millisecond != 24*time.Hour {
		t.Errorf("a tenant created after the edit holds %v, want 500 on a daily window", h)
	}
}

// TestAPlanChangeIsAHardReset is IP-10 DoD 2 (DR-013, UC-06, Q-01, Q-02,
// INV-T3): a change from a 1000-limit plan at balance 300 to a 500-limit plan
// re-anchors the tenant at the change, discards the balance and the bonus,
// keeps its override, is audited as tenant.change_plan, hands the ledger a
// plan_changed event of +200, and reaches a second instance within NFR-D3: a
// feature only the old plan had is refused there within a second.
func TestAPlanChangeIsAHardReset(t *testing.T) {
	s := newStack(t)
	other := newStack(t, func(o *stackOptions) { o.dbURL = s.dbURL })
	f, g, h, big, small := uniq("hf"), uniq("hg"), uniq("hh"), uniq("hbig"), uniq("hsmall")
	for _, k := range []string{f, g, h} {
		s.createFeature(k)
	}
	mustStatus(t, s.adminDo(http.MethodPost, "/v1/admin/plans", `{"plan_key":"`+big+`","name":"P","entitlements":[`+
		`{"feature_key":"`+f+`","limit":1000,"reset_interval":"monthly"},{"feature_key":"`+g+`","limit":10,"reset_interval":"monthly"},`+
		`{"feature_key":"`+h+`","limit":10,"reset_interval":"monthly"}]}`),
		http.StatusCreated)
	mustStatus(t, s.adminDo(http.MethodPost, "/v1/admin/plans", `{"plan_key":"`+small+`","name":"P","entitlements":[`+
		`{"feature_key":"`+f+`","limit":500,"reset_interval":"monthly"},{"feature_key":"`+g+`","limit":10,"reset_interval":"monthly"}]}`),
		http.StatusCreated)
	tn := asTenant(t, s.createTenant(externalID("hard"), big, "UTC"), ent{f, 1000, "monthly", false})
	tp := "/v1/admin/tenants/" + tn.ID
	mustStatus(t, s.consume(s.runtime, f, 725, tn.External, newKey()), http.StatusOK)
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/grant", `{"feature_key":"`+f+`","amount":25,"reason":"goodwill"}`), http.StatusOK)
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/overrides",
		`{"feature_key":"`+g+`","limit_value":20,"interval":null,"apply_at":"next_boundary"}`), http.StatusOK)
	if h := s.state(tn, f).Hash; h["balance"] != "300" || h["bonus"] != "25" {
		t.Fatalf("before the change the hash is %v, want balance 300 with a bonus of 25", h)
	}
	mustStatus(t, other.do(http.MethodPost, "/v1/check", s.runtime, metered(h, 1, tn.External)), http.StatusOK)
	audits := s.rows(`SELECT count(*) FROM audit_log WHERE action = 'tenant.change_plan' AND subject_id = $1`, tn.ID)
	oldAnchor := s.anchorOf(tn.ID)

	m := mustStatus(t, s.adminDo(http.MethodPatch, tp, fmt.Sprintf(`{"version":%d,"plan_key":%q}`, s.version(tp), small)), http.StatusOK)
	anchor := s.anchorOf(tn.ID)
	if !anchor.After(oldAnchor) {
		t.Errorf("the anchor moved from %v to %v, not forward", oldAnchor, anchor)
	}
	if got, _ := time.Parse(time.RFC3339Nano, str(m, "anchor_at")); !got.Equal(anchor.Truncate(time.Second)) {
		t.Errorf("the response's anchor %v is not the stored %v", got, anchor)
	}
	if hash := s.state(tn, f).Hash; hash["balance"] != "500" || hash["limit"] != "500" || hash["bonus"] != "0" ||
		hash["window_start"] != ms(anchor) {
		t.Errorf("after the change the hash is %v, want a fresh 500 starting at %s", hash, ms(anchor))
	}
	if n := s.rows(`SELECT count(*) FROM tenant_entitlement_overrides o JOIN features f ON f.id = o.feature_id
		WHERE o.tenant_id = $1 AND f.key = $2`, tn.ID, g); n != 1 {
		t.Errorf("the override on %s did not survive the change: %d rows", g, n)
	}
	if n := s.rows(`SELECT count(*) FROM audit_log WHERE action = 'tenant.change_plan' AND subject_id = $1`, tn.ID); n != audits+1 {
		t.Errorf("tenant.change_plan audited %d times, want once", n-audits)
	}
	if d := s.eventsOf(tn.ID, f, "plan_changed"); len(d) != 1 || d[0] != 200 {
		t.Errorf("plan_changed events for the tenant: %v, want one of +200", d)
	}
	// The same, as the rows the writer appended (IP-10 hand-on, IP-12).
	if rows := rowsFor(s.ledger(tn), f); !sameShapes(rows, expect{"consumed", "api", -725, 275},
		expect{"granted", "admin", 25, 300}, expect{"plan_changed", "admin", 200, 500}) {
		t.Errorf("the ledger for %s is %v", f, shapes(rows))
	}
	if n := s.rows(`SELECT count(*) FROM pending_reanchors WHERE tenant_id = $1`, tn.ID); n != 0 {
		t.Errorf("the inline drain left %d pending rows", n)
	}

	until(t, "feature_not_in_plan", func() response {
		return other.do(http.MethodPost, "/v1/check", s.runtime, metered(h, 1, tn.External))
	})
}

// TestAGrantIsCycleScoped is IP-10 DoD 3 (DR-020, Q-01, INV-C5): a grant's
// bonus and balance survive neither a plan change nor the next boundary.
func TestAGrantIsCycleScoped(t *testing.T) {
	s := newStack(t)
	f, a, b := uniq("gf"), uniq("ga"), uniq("gb")
	s.createFeature(f)
	pa := s.createPlan(a, 10, "hourly", f)
	s.createPlan(b, 10, "hourly", f)

	// Across a plan change.
	changed := asTenant(t, s.createTenant(externalID("gc"), a, "UTC"), ent{f, 10, "hourly", false})
	tp := "/v1/admin/tenants/" + changed.ID
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/grant", `{"feature_key":"`+f+`","amount":50,"reason":"goodwill"}`), http.StatusOK)
	mustStatus(t, s.adminDo(http.MethodPatch, tp, fmt.Sprintf(`{"version":%d,"plan_key":%q}`, s.version(tp), b)), http.StatusOK)
	if h := s.state(changed, f).Hash; h["balance"] != "10" || h["bonus"] != "0" {
		t.Errorf("the grant survived a plan change: %v", h)
	}

	// Across the next boundary: a window that closes in two seconds.
	tn := s.tenantOn(str(pa, "id"), "UTC", time.Now().Add(-time.Hour+2*time.Second), ent{f, 10, "hourly", false})
	cur := s.seedCurrent(tn, f, 10)
	r := mustStatus(t, s.adminDo(http.MethodPost, "/v1/admin/tenants/"+tn.ID+"/grant",
		`{"feature_key":"`+f+`","amount":50,"reason":"goodwill"}`), http.StatusOK)
	if num(t, r, "balance") != 60 || num(t, r, "bonus") != 50 {
		t.Fatalf("the grant answered %v, want balance 60 and bonus 50", r)
	}
	time.Sleep(time.Until(*cur.End) + 50*time.Millisecond)
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
	if h := s.state(tn, f).Hash; h["balance"] != "9" || h["bonus"] != "0" || h["cycle_index"] != fmt.Sprint(cur.Index+1) {
		t.Errorf("the grant survived the boundary: %v", h)
	}
}

// TestAnOverrideCannotAddAFeature is IP-10 DoD 4 (DR-015, INV-TE1): an
// override for a feature outside the plan is refused, one above the plan's
// limit is accepted, and one on a feature the plan later drops enforces until
// the retirement boundary and is feature_not_in_plan after it.
func TestAnOverrideCannotAddAFeature(t *testing.T) {
	s := newStack(t)
	f, g, outside, plan := uniq("of"), uniq("og"), uniq("oo"), uniq("op")
	for _, k := range []string{f, g, outside} {
		s.createFeature(k)
	}
	mustStatus(t, s.adminDo(http.MethodPost, "/v1/admin/plans", `{"plan_key":"`+plan+`","name":"P","entitlements":[`+
		`{"feature_key":"`+f+`","limit":5,"reset_interval":"hourly"},{"feature_key":"`+g+`","limit":5,"reset_interval":"hourly"}]}`),
		http.StatusCreated)
	var planID string
	if err := s.seedDB.QueryRow(`SELECT id FROM plans WHERE key = $1`, plan).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	tn := s.tenantOn(planID, "UTC", time.Now().Add(-time.Hour+3*time.Second), ent{f, 5, "hourly", false}, ent{g, 5, "hourly", false})
	cur := s.seedCurrent(tn, f, 5)
	s.seedCurrent(tn, g, 5)
	op := "/v1/admin/tenants/" + tn.ID + "/overrides"

	r := s.adminDo(http.MethodPost, op, `{"feature_key":"`+outside+`","limit_value":50,"interval":null,"apply_at":"immediate"}`)
	if r.Status != http.StatusBadRequest || r.code(t) != "validation_failed" {
		t.Errorf("an override for a feature outside the plan answered %d %s", r.Status, r.Body)
	}
	mustStatus(t, s.adminDo(http.MethodPost, op, `{"feature_key":"`+g+`","limit_value":50,"interval":null,"apply_at":"immediate"}`),
		http.StatusOK)
	if h := s.state(tn, g).Hash; h["limit"] != "50" {
		t.Errorf("an override above the plan's limit applied immediately left the hash at %v", h)
	}

	s.planEdit(plan, f+":5:hourly")
	if r := s.consume(s.runtime, g, 10, tn.External, newKey()); r.Status != http.StatusOK {
		t.Errorf("the overridden feature, dropped from the plan, is refused before its boundary: %d %s", r.Status, r.Body)
	}
	time.Sleep(time.Until(*cur.End) + 50*time.Millisecond)
	if r := s.consume(s.runtime, g, 1, tn.External, newKey()); r.Status != http.StatusForbidden || r.code(t) != "feature_not_in_plan" {
		t.Errorf("past its retirement boundary the overridden feature answered %d %s, want 403 feature_not_in_plan", r.Status, r.Body)
	}
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
}

// TestApplyNowReanchorsAndIsAudited is IP-10 DoD 5's first half (DR-046, P-3,
// INV-P2): a reason is required, every tenant on the plan is re-anchored once
// at the apply, and the apply is audited once with its reason.
func TestApplyNowReanchorsAndIsAudited(t *testing.T) {
	s := newStack(t)
	f, _, first := s.provisioned(100)
	var plan string
	if err := s.seedDB.QueryRow(`SELECT p.key FROM plans p JOIN tenants t ON t.plan_id = p.id WHERE t.id = $1`, first.ID).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	second := asTenant(t, s.createTenant(externalID("an"), plan, "Asia/Kolkata"), ent{f, 100, "monthly", false})
	pp := "/v1/admin/plans/" + plan
	for _, tn := range []tenant{first, second} {
		mustStatus(t, s.consume(s.runtime, f, 30, tn.External, newKey()), http.StatusOK)
	}
	r := s.adminDo(http.MethodPost, pp+"/apply", `{}`)
	if r.Status != http.StatusBadRequest || r.code(t) != "validation_failed" {
		t.Errorf("an apply without a reason answered %d %s", r.Status, r.Body)
	}
	version := s.version(pp)
	m := mustStatus(t, s.adminDo(http.MethodPost, pp+"/apply", `{"reason":"the new pricing"}`), http.StatusOK)
	if num(t, m, "affected_tenants") != 2 || num(t, m, "version") != version+1 {
		t.Errorf("the apply answered %v, want 2 tenants and version %d", m, version+1)
	}
	applied, _ := time.Parse(time.RFC3339Nano, str(m, "applied_at"))
	for _, tn := range []tenant{first, second} {
		anchor := s.anchorOf(tn.ID)
		if !anchor.Truncate(time.Second).Equal(applied) {
			t.Errorf("%s is anchored at %v, want the apply's %v", tn.ID, anchor, applied)
		}
		if h := s.state(tn, f).Hash; h["balance"] != "100" || h["window_start"] != ms(anchor) || h["cycle_index"] != "0" {
			t.Errorf("%s after the apply: %v", tn.ID, h)
		}
		if d := s.eventsOf(tn.ID, f, "plan_changed"); len(d) != 1 || d[0] != 30 {
			t.Errorf("%s was re-anchored with events %v, want one of +30", tn.ID, d)
		}
	}
	if n := s.rows(`SELECT count(*) FROM audit_log WHERE action = 'plan.apply_now' AND reason = 'the new pricing'`); n != 1 {
		t.Errorf("plan.apply_now audited %d times with its reason, want once", n)
	}
	if n := s.rows(`SELECT count(*) FROM pending_reanchors`); n != 0 {
		t.Errorf("the apply left %d pending re-anchors", n)
	}
}

// TestT12WideApplyNeedsAConfirmation is T-12 (IP-10 DoD 5, D7, INV-P4).
func TestT12WideApplyNeedsAConfirmation(t *testing.T) {
	s := newStack(t, func(o *stackOptions) { o.env["QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD"] = "2" })
	f := uniq("wf")
	s.createFeature(f)
	wide, other, exact := uniq("wide"), uniq("wother"), uniq("wexact")
	for _, p := range []string{wide, other, exact} {
		s.createPlan(p, 100, "monthly", f)
	}
	var tenants []tenant
	for range 3 {
		tenants = append(tenants, asTenant(t, s.createTenant(externalID("w"), wide, "UTC"), ent{f, 100, "monthly", false}))
	}
	for range 3 {
		s.createTenant(externalID("wo"), other, "UTC")
	}
	for range 2 {
		s.createTenant(externalID("we"), exact, "UTC")
	}
	pp := "/v1/admin/plans/" + wide
	apply := func(path, reason, token string) response {
		body := `{"reason":"` + reason + `"`
		if token != "" {
			body += `,"confirmation_token":"` + token + `"`
		}
		return s.adminDo(http.MethodPost, path+"/apply", body+"}")
	}
	audits := s.rows(`SELECT count(*) FROM audit_log`)
	anchors := map[string]time.Time{}
	for _, tn := range tenants {
		anchors[tn.ID] = s.anchorOf(tn.ID)
	}
	// confirm asks for a token and checks the 409 wrote nothing but it.
	confirm := func(path, reason string, tenants int64) string {
		t.Helper()
		r := apply(path, reason, "")
		if r.Status != http.StatusConflict || r.code(t) != "confirmation_required" {
			t.Fatalf("an apply above the threshold answered %d %s", r.Status, r.Body)
		}
		d := r.details(t)
		if int64(d["affected_tenants"].(float64)) != tenants || int64(d["threshold"].(float64)) != 2 {
			t.Errorf("the 409 counted %v", d)
		}
		token := d["confirmation_token"].(string)
		if ttl := s.rdb.PTTL(context.Background(), "qc:admin:cnf:"+token).Val(); ttl <= 0 || ttl > 15*time.Minute {
			t.Errorf("the token is stored with a time to live of %v", ttl)
		}
		return token
	}
	unchanged := func(when string) {
		t.Helper()
		if n := s.rows(`SELECT count(*) FROM audit_log`); n != audits {
			t.Errorf("%s: the audit log grew by %d", when, n-audits)
		}
		for id, at := range anchors {
			if got := s.anchorOf(id); !got.Equal(at) {
				t.Errorf("%s: %s was re-anchored", when, id)
			}
		}
	}

	token := confirm(pp, "wide", 3)
	r := apply(pp, "wide", "")
	d := r.details(t)
	earliest := time.Time{}
	for _, tn := range tenants {
		if end := *s.currentWindow(tn, f, time.Now()).End; earliest.IsZero() || end.Before(earliest) {
			earliest = end
		}
	}
	if got, _ := time.Parse(time.RFC3339, d["earliest_cycle_end"].(string)); !got.Equal(earliest.Truncate(time.Second)) {
		t.Errorf("the 409's earliest end is %v, want %v", d["earliest_cycle_end"], earliest)
	}
	unchanged("two 409s")

	// Another reason consumes the token without honouring it, so it is spent.
	if r := apply(pp, "another", token); r.Status != http.StatusConflict {
		t.Errorf("a token for another reason answered %d", r.Status)
	}
	if r := apply(pp, "wide", token); r.Status != http.StatusConflict {
		t.Errorf("a reused token answered %d", r.Status)
	}
	// Another plan's token.
	if r := apply(pp, "wide", confirm("/v1/admin/plans/"+other, "wide", 3)); r.Status != http.StatusConflict {
		t.Errorf("another plan's token answered %d", r.Status)
	}
	// An expired token: the store's expiry is its deletion.
	expired := confirm(pp, "wide", 3)
	s.rdb.Del(context.Background(), "qc:admin:cnf:"+expired)
	if r := apply(pp, "wide", expired); r.Status != http.StatusConflict {
		t.Errorf("an expired token answered %d", r.Status)
	}
	// A token for an older version of the plan.
	old := confirm(pp, "wide", 3)
	mustStatus(t, s.adminDo(http.MethodPatch, pp, fmt.Sprintf(`{"version":%d,"name":"Renamed"}`, s.version(pp))), http.StatusOK)
	if r := apply(pp, "wide", old); r.Status != http.StatusConflict {
		t.Errorf("a token for an older version answered %d", r.Status)
	}
	audits++ // the rename
	// A token issued before the count grew.
	grew := confirm(pp, "wide", 3)
	extra := asTenant(t, s.createTenant(externalID("w"), wide, "UTC"), ent{f, 100, "monthly", false})
	audits++ // the tenant
	if r := apply(pp, "wide", grew); r.Status != http.StatusConflict || int64(r.details(t)["affected_tenants"].(float64)) != 4 {
		t.Errorf("a token issued for 3 tenants answered %d %s for 4", r.Status, r.Body)
	}
	unchanged("every refused token")
	anchors[extra.ID] = s.anchorOf(extra.ID)

	// The confirmed apply.
	m := mustStatus(t, apply(pp, "wide", confirm(pp, "wide", 4)), http.StatusOK)
	if num(t, m, "affected_tenants") != 4 {
		t.Errorf("the confirmed apply answered %v", m)
	}
	if n := s.rows(`SELECT count(*) FROM audit_log WHERE action = 'plan.apply_now'`); n != 1 {
		t.Errorf("plan.apply_now audited %d times", n)
	}
	// A plan with exactly the threshold needs no token.
	mustStatus(t, apply("/v1/admin/plans/"+exact, "exact", ""), http.StatusOK)
}

// TestImpactChangesNothing is IP-10 DoD 6 (UC-19, Q-14): twelve tenants
// anchored over three months answer 12 and the earliest of their boundaries,
// a never-only plan answers null, and the read changes nothing in either
// store, a confirmation key included.
func TestImpactChangesNothing(t *testing.T) {
	s := newStack(t)
	f, n := uniq("if"), uniq("in")
	s.createFeature(f)
	s.createFeature(n)
	plan, never := uniq("ip"), uniq("inever")
	p := s.createPlan(plan, 100, "monthly", f)
	s.createPlan(never, 5, "never", n)
	s.createTenant(externalID("in"), never, "UTC")
	now := time.Now()
	var ids []string
	var earliest time.Time
	zones := []string{"UTC", "Europe/Berlin", "America/New_York", "Asia/Kolkata"}
	for i := range 12 {
		tn := s.tenantOn(str(p, "id"), zones[i%4], now.Add(-time.Duration(i)*7*24*time.Hour), ent{f, 100, "monthly", false})
		s.seedCurrent(tn, f, 100)
		ids = append(ids, tn.ID)
		if end := *s.currentWindow(tn, f, now).End; earliest.IsZero() || end.Before(earliest) {
			earliest = end
		}
	}
	pp := "/v1/admin/plans/" + plan
	version := s.version(pp)
	control, fast := s.controlDigest(), s.fastDigest(ids...)
	cnf := len(s.rdb.Keys(context.Background(), "qc:admin:cnf:*").Val())

	m := mustStatus(t, s.adminDo(http.MethodGet, pp+"/impact", ""), http.StatusOK)
	if num(t, m, "affected_tenants") != 12 || m["exceeds_confirmation_threshold"] != false {
		t.Errorf("impact answered %v, want 12 tenants under the threshold", m)
	}
	if got, err := time.Parse(time.RFC3339Nano, str(m, "earliest_cycle_end")); err != nil || !got.Equal(earliest.Truncate(time.Second)) {
		t.Errorf("impact's earliest end is %v, want %v", m["earliest_cycle_end"], earliest)
	}
	if m := mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/plans/"+never+"/impact", ""), http.StatusOK); m["earliest_cycle_end"] != nil {
		t.Errorf("a never-only plan's impact is %v, want a null earliest end", m)
	}
	if s.version(pp) != version || s.controlDigest() != control || s.fastDigest(ids...) != fast ||
		len(s.rdb.Keys(context.Background(), "qc:admin:cnf:*").Val()) != cnf {
		t.Error("impact changed something")
	}
}

// TestAbsentIsNotZero is IP-10 DoD 7 (DR-012, E-1, E-4, INV-E1): an absent
// feature is feature_not_in_plan and unlisted, a 0-limit one is enforced and
// listed, and both again across an added and a removed feature.
func TestAbsentIsNotZero(t *testing.T) {
	s := newStack(t)
	zero, absent, plan := uniq("zz"), uniq("za"), uniq("zp")
	s.createFeature(zero)
	s.createFeature(absent)
	p := s.createPlan(plan, 0, "hourly", zero)
	tn := s.tenantOn(str(p, "id"), "UTC", time.Now().Add(-time.Hour+4*time.Second), ent{zero, 0, "hourly", false})
	cur := s.seedCurrent(tn, zero, 0)
	listed := func(tn tenant, feature string) bool {
		t.Helper()
		r := s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External, s.runtime, "")
		if r.Status != http.StatusOK {
			t.Fatalf("balance: %d %s", r.Status, r.Body)
		}
		return strings.Contains(string(r.Body), `"`+feature+`"`)
	}
	isAbsent := func(when string, tn tenant, feature string) {
		t.Helper()
		if r := s.consume(s.runtime, feature, 1, tn.External, newKey()); r.Status != http.StatusForbidden || r.code(t) != "feature_not_in_plan" {
			t.Errorf("%s: consuming an absent feature answered %d %s", when, r.Status, r.Body)
		}
		if listed(tn, feature) {
			t.Errorf("%s: the balance route lists an absent feature", when)
		}
	}
	isZero := func(when string, tn tenant, feature string) {
		t.Helper()
		r := s.consume(s.runtime, feature, 1, tn.External, newKey())
		if r.Status != http.StatusTooManyRequests || r.code(t) != "quota_exceeded" || r.details(t)["balance"] != 0.0 {
			t.Errorf("%s: consuming a 0-limit feature answered %d %s", when, r.Status, r.Body)
		}
		if r := s.check(feature, tn.External); r.Status != http.StatusOK || r.json(t)["allowed"] != false {
			t.Errorf("%s: checking a 0-limit feature answered %d %s", when, r.Status, r.Body)
		}
		if !listed(tn, feature) {
			t.Errorf("%s: the balance route does not list a 0-limit feature", when)
		}
	}
	isAbsent("at first", tn, absent)
	isZero("at first", tn, zero)

	s.planEdit(plan, zero+":0:hourly", absent+":3:hourly")
	isAbsent("an existing tenant after the feature is added", tn, absent)
	fresh := asTenant(t, s.createTenant(externalID("zn"), plan, "UTC"))
	mustStatus(t, s.consume(s.runtime, absent, 1, fresh.External, newKey()), http.StatusOK)

	s.planEdit(plan, absent+":3:hourly")
	isZero("after the feature is removed, before the boundary", tn, zero)
	time.Sleep(time.Until(*cur.End) + 50*time.Millisecond)
	isAbsent("after the feature is removed, past the boundary", tn, zero)
}

// TestArchivingAPlanChangesNoTenant is IP-10 DoD 8 (INV-P1, P-4).
func TestArchivingAPlanChangesNoTenant(t *testing.T) {
	s := newStack(t)
	f, plan, tn := s.provisioned(10)
	pp := "/v1/admin/plans/" + plan
	before, anchor, pins := s.state(tn, f), s.anchorOf(tn.ID), s.pinsOf(tn.ID)
	mustStatus(t, s.adminDo(http.MethodPost, pp+"/archive", fmt.Sprintf(`{"version":%d,"reason":"retired"}`, s.version(pp))), http.StatusOK)
	after := s.state(tn, f)
	if !reflect.DeepEqual(after.Hash, before.Hash) || after.Expiry != before.Expiry || !s.anchorOf(tn.ID).Equal(anchor) || s.pinsOf(tn.ID) != pins {
		t.Errorf("archiving the plan changed its tenant")
	}
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
	other := asTenant(t, s.createTenant(externalID("ao"), s.createPlanKey(f), "UTC"))
	op := "/v1/admin/tenants/" + other.ID
	r := s.adminDo(http.MethodPatch, op, fmt.Sprintf(`{"version":%d,"plan_key":%q}`, s.version(op), plan))
	if r.Status != http.StatusBadRequest || r.code(t) != "validation_failed" {
		t.Errorf("a change to an archived plan answered %d %s", r.Status, r.Body)
	}
}

func (s *stack) createPlanKey(feature string) string {
	key := uniq("pk")
	s.createPlan(key, 10, "monthly", feature)
	return key
}

// TestT06PlanAppliedAcrossAnOutage is T-06 as IP-10 extends it (DR-009, D2).
func TestT06PlanAppliedAcrossAnOutage(t *testing.T) {
	t.Run("an edit then an idle", func(t *testing.T) {
		s := newStack(t)
		f, plan := uniq("tf"), uniq("tp")
		s.createFeature(f)
		p := s.createPlan(plan, 100, "hourly", f)
		now := time.Now()
		anchor := now.AddDate(-5, 0, 0).Truncate(time.Minute)
		idles := []time.Duration{time.Hour, 24 * time.Hour, 31 * 24 * time.Hour, 97 * 24 * time.Hour,
			400 * 24 * time.Hour, 4 * 365 * 24 * time.Hour}
		var tenants []tenant
		for _, idle := range idles {
			tn := s.tenantOn(str(p, "id"), "UTC", anchor, ent{f, 100, "hourly", false})
			w := s.currentWindow(tn, f, now.Add(-idle))
			s.seed(tn, f, stored{Balance: 7, Limit: 100, Index: w.Index, Start: w.Start, End: w.End})
			tenants = append(tenants, tn)
		}
		s.planEdit(plan, f+":300:hourly")
		for i, tn := range tenants {
			mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
			cur := s.currentWindow(tn, f, time.Now())
			want := map[string]string{"balance": "299", "limit": "300", "bonus": "0", "cycle_index": fmt.Sprint(cur.Index),
				"window_start": ms(cur.Start), "window_end": ms(*cur.End)}
			if h := s.state(tn, f).Hash; !reflect.DeepEqual(h, want) {
				t.Errorf("after an idle of %v: %v, want %v", idles[i], h, want)
			}
		}
	})

	t.Run("a change and an apply while the store is away", func(t *testing.T) {
		_, redisURL := integrationURLs(t)
		cut := newProxy(t, hostOf(t, redisURL))
		s := newStack(t, func(o *stackOptions) { o.redisURL = urlVia(t, redisURL, cut.addr()) })
		f, a, b := uniq("of"), uniq("oa"), uniq("ob")
		s.createFeature(f)
		s.createPlan(a, 100, "monthly", f)
		s.createPlan(b, 50, "monthly", f)
		var tenants []tenant
		for range 3 {
			tn := asTenant(t, s.createTenant(externalID("o"), a, "UTC"), ent{f, 100, "monthly", false})
			mustStatus(t, s.consume(s.runtime, f, 10, tn.External, newKey()), http.StatusOK)
			tenants = append(tenants, tn)
		}
		changed := tenants[0]
		tp := "/v1/admin/tenants/" + changed.ID
		version := s.version(tp)

		cut.set("refuse")
		mustStatus(t, s.adminDo(http.MethodPatch, tp, fmt.Sprintf(`{"version":%d,"plan_key":%q}`, version, b)), http.StatusOK)
		mustStatus(t, s.adminDo(http.MethodPost, "/v1/admin/plans/"+a+"/apply", `{"reason":"while away"}`), http.StatusOK)
		if n := s.rows(`SELECT count(*) FROM pending_reanchors`); n != 3 {
			t.Fatalf("%d pending re-anchors while the store is away, want 3", n)
		}
		for _, tn := range tenants {
			if h := s.state(tn, f).Hash; h["balance"] != "90" {
				t.Errorf("%s's hash moved while the store was away: %v", tn.ID, h)
			}
		}

		// Two instances drain together once the store is back.
		second := newStack(t, func(o *stackOptions) { o.dbURL = s.dbURL })
		cut.set("pass")
		deadline := time.Now().Add(10 * time.Second)
		for s.rows(`SELECT count(*) FROM pending_reanchors`) > 0 {
			if time.Now().After(deadline) {
				t.Fatal("the pending re-anchors were not drained")
			}
			time.Sleep(50 * time.Millisecond)
		}
		digest := func() string { return s.controlDigest() + s.fastDigest(tenants[0].ID, tenants[1].ID, tenants[2].ID) }
		for _, tn := range tenants {
			anchor, limit := s.anchorOf(tn.ID), "100"
			if tn.ID == changed.ID {
				limit = "50"
			}
			if h := s.state(tn, f).Hash; h["balance"] != limit || h["window_start"] != ms(anchor) {
				t.Errorf("%s after the drain: %v, want a fresh %s from %s", tn.ID, h, limit, ms(anchor))
			}
			events := len(s.eventsOf(tn.ID, f, "plan_changed")) + len(second.eventsOf(tn.ID, f, "plan_changed"))
			if events != 1 {
				t.Errorf("%s was re-anchored %d times", tn.ID, events)
			}
		}

		// A restart, and another drain, change nothing.
		before := digest()
		newStack(t, func(o *stackOptions) { o.dbURL = s.dbURL })
		time.Sleep(1500 * time.Millisecond)
		if after := digest(); after != before {
			t.Errorf("a restart changed the drained state:\n%s\nthen\n%s", before, after)
		}
	})
}
