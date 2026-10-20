package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/quotacore/quotacore/internal/admin"
	"github.com/quotacore/quotacore/internal/auth"
	"github.com/quotacore/quotacore/internal/db"
)

// asTenant is the harness's view of a tenant created over the API.
func asTenant(t *testing.T, m map[string]any, ents ...ent) tenant {
	t.Helper()
	anchor, err := time.Parse(time.RFC3339Nano, str(m, "anchor_at"))
	if err != nil {
		t.Fatal(err)
	}
	tn := tenant{ID: str(m, "id"), External: str(m, "external_id"), Zone: str(m, "timezone"), Anchor: anchor, ents: map[string]ent{}}
	for _, e := range ents {
		tn.ents[e.feature] = e
	}
	return tn
}

// until polls send until it answers want, within NFR-D3's second.
func until(t *testing.T, want string, send func() response) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		r := send()
		if r.code(t) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still %d %s after a second, want %s", r.Status, r.Body, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *stack) check(feature, external string) response {
	return s.do(http.MethodPost, "/v1/check", s.runtime, metered(feature, 1, external))
}

// provisioned creates a feature, a plan on it and a tenant on the plan, all
// over the API.
func (s *stack) provisioned(limit int64) (string, string, tenant) {
	s.t.Helper()
	f, plan := uniq("pf"), uniq("pp")
	s.createFeature(f)
	s.createPlan(plan, limit, "monthly", f)
	m := s.createTenant(externalID("pt"), plan, "UTC")
	return f, plan, asTenant(s.t, m, ent{f, limit, "monthly", false})
}

func (s *stack) version(path string) int64 {
	s.t.Helper()
	return num(s.t, mustStatus(s.t, s.adminDo(http.MethodGet, path, ""), http.StatusOK), "version")
}

// keyIdentity is the audit actor the stack's admin key writes.
func (s *stack) keyIdentity(token string) (id, name string) {
	s.t.Helper()
	tok, err := auth.Parse(token)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := s.seedDB.QueryRow(`SELECT id, name FROM api_keys WHERE prefix = $1`, tok.Prefix).Scan(&id, &name); err != nil {
		s.t.Fatal(err)
	}
	return id, name
}

// TestEveryMutationIsAuditedOnce is IP-09 DoD 4 (DR-041, NFR-S5): each
// mutating route writes exactly one audit row naming the calling key, the
// request id the response carried, the resource before and after, and the
// reason where the route takes one.
func TestEveryMutationIsAuditedOnce(t *testing.T) {
	s := newStack(t)
	keyID, keyName := s.keyIdentity(s.admin)
	f, plan, tn := s.provisioned(100)
	spare := uniq("spare")
	s.createFeature(spare)
	sparePlan := uniq("sparep")
	s.createPlan(sparePlan, 5, "daily", spare)
	freeFeature := uniq("free")
	s.createFeature(freeFeature)
	newKeyID := ""

	type step struct {
		action, method, path string
		body                 func() string
		reason, create, gone bool
	}
	v := func(path string) string { return fmt.Sprint(s.version(path)) }
	fp, pp, tp := "/v1/admin/features/"+f, "/v1/admin/plans/"+sparePlan, "/v1/admin/tenants/"+tn.ID
	steps := []step{
		{action: "api_key.create", method: http.MethodPost, path: "/v1/admin/keys", create: true,
			body: func() string { return `{"name":"` + uniq("aud") + `","scope":"runtime"}` }},
		{action: "api_key.revoke", method: http.MethodDelete, path: "", body: func() string { return "" }},
		{action: "feature.create", method: http.MethodPost, path: "/v1/admin/features", create: true,
			body: func() string { return `{"feature_key":"` + uniq("af") + `","name":"F","unit":"call"}` }},
		{action: "feature.update", method: http.MethodPatch, path: fp,
			body: func() string { return `{"version":` + v(fp) + `,"name":"Renamed"}` }},
		{action: "feature.delete", method: http.MethodDelete, path: "/v1/admin/features/" + freeFeature, reason: true, gone: true,
			body: func() string { return `{"reason":"created by mistake"}` }},
		{action: "plan.create", method: http.MethodPost, path: "/v1/admin/plans", create: true,
			body: func() string {
				return `{"plan_key":"` + uniq("ap") + `","name":"P","entitlements":[{"feature_key":"` + f + `","limit":1,"reset_interval":"never"}]}`
			}},
		{action: "plan.archive", method: http.MethodPost, path: pp + "/archive", reason: true,
			body: func() string { return `{"version":` + v(pp) + `,"reason":"retired"}` }},
		{action: "plan.delete", method: http.MethodDelete, path: pp, reason: true, gone: true,
			body: func() string { return `{"reason":"never sold"}` }},
		{action: "tenant.create", method: http.MethodPost, path: "/v1/admin/tenants", create: true,
			body: func() string {
				return `{"external_id":"` + externalID("at") + `","display_name":"T","plan_key":"` + plan + `"}`
			}},
		{action: "tenant.update", method: http.MethodPatch, path: tp,
			body: func() string { return `{"version":` + v(tp) + `,"display_name":"Renamed","metadata":{"crm":"x"}}` }},
		{action: "tenant.suspend", method: http.MethodPost, path: tp + "/suspend", reason: true,
			body: func() string { return `{"version":` + v(tp) + `,"reason":"unpaid invoice"}` }},
		{action: "tenant.resume", method: http.MethodPost, path: tp + "/resume", reason: true,
			body: func() string { return `{"version":` + v(tp) + `,"reason":"invoice paid"}` }},
		{action: "feature.archive", method: http.MethodPost, path: fp + "/archive", reason: true,
			body: func() string { return `{"version":` + v(fp) + `,"reason":"replaced"}` }},
		{action: "tenant.delete", method: http.MethodDelete, path: tp, reason: true,
			body: func() string { return `{"reason":"churned"}` }},
	}
	for _, st := range steps {
		path := st.path
		if st.action == "api_key.revoke" {
			path = "/v1/admin/keys/" + newKeyID
		}
		rid := uuid.NewString()
		r := s.do(st.method, path, s.admin, st.body(), "X-Request-Id", rid)
		if r.Status >= 300 {
			t.Fatalf("%s: %d %s", st.action, r.Status, r.Body)
		}
		if r.Header.Get("X-Request-Id") != rid {
			t.Fatalf("%s: the response carries request id %q, want %q", st.action, r.Header.Get("X-Request-Id"), rid)
		}
		if st.action == "api_key.create" {
			newKeyID = str(r.json(t), "id")
		}
		rows, err := s.seedDB.Query(`SELECT action, actor, actor_key_id::text, before IS NULL, after IS NULL, reason
  FROM audit_log WHERE request_id = $1`, rid)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			n++
			var action, actor, actorKey string
			var noBefore, noAfter bool
			var reason *string
			if err := rows.Scan(&action, &actor, &actorKey, &noBefore, &noAfter, &reason); err != nil {
				t.Fatal(err)
			}
			if action != st.action || actor != keyName || actorKey != keyID {
				t.Errorf("%s: audit row says %s by %s (%s), want %s by %s (%s)", st.action, action, actor, actorKey, st.action, keyName, keyID)
			}
			if noBefore != st.create || noAfter != st.gone {
				t.Errorf("%s: before null = %v, after null = %v", st.action, noBefore, noAfter)
			}
			if (reason != nil) != st.reason {
				t.Errorf("%s: reason = %v, want one: %v", st.action, reason, st.reason)
			}
		}
		rows.Close()
		if n != 1 {
			t.Errorf("%s wrote %d audit rows, want 1", st.action, n)
		}
	}
	var secrets int
	if err := s.seedDB.QueryRow(`SELECT count(*) FROM audit_log WHERE before::text LIKE '%key_hash%' OR after::text LIKE '%key_hash%'`).Scan(&secrets); err != nil || secrets != 0 {
		t.Errorf("%d audit rows carry key_hash (%v)", secrets, err)
	}
}

// TestAnAbortedMutationLeavesNoAuditRow is IP-09 DoD 4's other half (INV-X3):
// a commit that fails after the write and its audit row leaves neither.
func TestAnAbortedMutationLeavesNoAuditRow(t *testing.T) {
	var fail atomic.Bool
	s := newStack(t, func(o *stackOptions) {
		o.adminStore = func(a *db.Admin) admin.Store {
			return a.WithBeforeCommit(func(context.Context) error {
				if fail.Load() {
					return errors.New("the commit is refused by the test")
				}
				return nil
			})
		}
	})
	f, plan, tn := s.provisioned(10)
	count := func() (n int) {
		if err := s.seedDB.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	fail.Store(true)
	for _, r := range []response{
		s.adminDo(http.MethodPost, "/v1/admin/features", `{"feature_key":"`+uniq("ab")+`","name":"F","unit":"call"}`),
		s.adminDo(http.MethodPost, "/v1/admin/tenants", `{"external_id":"`+externalID("ab")+`","display_name":"T","plan_key":"`+plan+`"}`),
		s.adminDo(http.MethodPost, "/v1/admin/features/"+f+"/archive", `{"version":1,"reason":"x"}`),
		s.adminDo(http.MethodPost, "/v1/admin/tenants/"+tn.ID+"/suspend", `{"version":1,"reason":"x"}`),
		s.adminDo(http.MethodPost, "/v1/admin/keys", `{"name":"`+uniq("ab")+`","scope":"admin"}`),
	} {
		if r.Status < 500 {
			t.Errorf("a failed commit answered %d %s", r.Status, r.Body)
		}
	}
	fail.Store(false)
	if count() != before {
		t.Fatalf("aborted mutations left %d audit rows", count()-before)
	}
	var features, tenants int
	_ = s.seedDB.QueryRow(`SELECT count(*) FROM features WHERE key LIKE 'ab-%'`).Scan(&features)
	_ = s.seedDB.QueryRow(`SELECT count(*) FROM tenants WHERE external_id LIKE 'ab-%'`).Scan(&tenants)
	got := mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/tenants/"+tn.ID, ""), http.StatusOK)
	if features != 0 || tenants != 0 || str(got, "state") != "active" || num(t, got, "version") != 1 {
		t.Fatalf("aborted mutations left %d features, %d tenants, and the tenant %v", features, tenants, got)
	}
}

// TestSuspensionDeniesAndPreservesState is IP-09 DoD 5 (DR-032, INV-T2): a
// suspended tenant is refused, suspension and resumption touch no balance
// field, expiry or idempotency record, and the tenant resumes from the same
// balance.
func TestSuspensionDeniesAndPreservesState(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	if r := s.consume(s.runtime, f, 30, tn.External, newKey()); r.Status != http.StatusOK {
		t.Fatalf("consume: %d %s", r.Status, r.Body)
	}
	before := s.state(tn, f)
	path := "/v1/admin/tenants/" + tn.ID
	mustStatus(t, s.adminDo(http.MethodPost, path+"/suspend", `{"version":`+fmt.Sprint(s.version(path))+`,"reason":"unpaid"}`), http.StatusOK)
	until(t, "tenant_suspended", func() response { return s.check(f, tn.External) })
	if r := s.consume(s.runtime, f, 1, tn.External, newKey()); r.code(t) != "tenant_suspended" {
		t.Fatalf("consume while suspended: %d %s", r.Status, r.Body)
	}
	if mid := s.state(tn, f); !reflect.DeepEqual(before, mid) {
		t.Fatalf("suspension changed the state:\nbefore %+v\nafter  %+v", before, mid)
	}
	mustStatus(t, s.adminDo(http.MethodPost, path+"/resume", `{"version":`+fmt.Sprint(s.version(path))+`,"reason":"paid"}`), http.StatusOK)
	until(t, "", func() response { return s.check(f, tn.External) })
	if after := s.state(tn, f); !reflect.DeepEqual(before, after) {
		t.Fatalf("resumption changed the state:\nbefore %+v\nafter  %+v", before, after)
	}
	r := s.consume(s.runtime, f, 5, tn.External, newKey())
	if r.Status != http.StatusOK || num(t, r.json(t), "balance") != 65 {
		t.Fatalf("the resumed tenant does not continue from 70: %d %s", r.Status, r.Body)
	}
}

// TestArchivingAFeaturePreservesItsBalance is IP-09 DoD 6 (DR-016, INV-F1):
// an archived feature is refused, its hash and expiry are untouched and still
// readable, and the ledger is not rewritten.
func TestArchivingAFeaturePreservesItsBalance(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	if r := s.consume(s.runtime, f, 40, tn.External, newKey()); r.Status != http.StatusOK {
		t.Fatalf("consume: %d %s", r.Status, r.Body)
	}
	// The writer appends the consume's row; the comparison starts once it has
	// (IP-12).
	if rows := s.ledger(tn); !sameShapes(rows, expect{"consumed", "api", -40, 60}) {
		t.Fatalf("the ledger before the archive is %v", shapes(rows))
	}
	ledger := func() string {
		var row string
		if err := s.seedDB.QueryRow(`SELECT string_agg(id::text || delta || balance_after || created_at::text, ',') FROM usage_events WHERE tenant_id = $1`, tn.ID).Scan(&row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	beforeLedger, before := ledger(), s.state(tn, f)
	fp := "/v1/admin/features/" + f
	mustStatus(t, s.adminDo(http.MethodPost, fp+"/archive", `{"version":`+fmt.Sprint(s.version(fp))+`,"reason":"replaced"}`), http.StatusOK)
	// The probe is a check: a consume that lands before the invalidation
	// does would itself change the hash this test compares.
	until(t, "feature_archived", func() response { return s.check(f, tn.External) })
	if after := s.state(tn, f); !reflect.DeepEqual(before, after) {
		t.Fatalf("archiving changed the hash:\nbefore %+v\nafter  %+v", before, after)
	}
	if ledger() != beforeLedger {
		t.Fatal("archiving rewrote the ledger")
	}
	b := s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External+"&feature_key="+f, s.runtime, "")
	if b.Status != http.StatusOK || !strings.Contains(string(b.Body), `"balance":60`) {
		t.Fatalf("the archived feature's balance is not readable: %d %s", b.Status, b.Body)
	}
	// Archiving again is a 200 that changes nothing and writes no audit row.
	var audits int
	countArchives := func() int {
		_ = s.seedDB.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'feature.archive'`).Scan(&audits)
		return audits
	}
	n := countArchives()
	mustStatus(t, s.adminDo(http.MethodPost, fp+"/archive", `{"version":`+fmt.Sprint(s.version(fp))+`,"reason":"again"}`), http.StatusOK)
	if countArchives() != n {
		t.Error("archiving an archived feature wrote an audit row")
	}
}

// TestConcurrentEditsConflict is IP-09 DoD 9 (FS-12): two writes carrying the
// same version race; one wins, the other is 409 resource_version_conflict
// naming the current version, and exactly one audit row is written.
func TestConcurrentEditsConflict(t *testing.T) {
	s := newStack(t)
	f, plan, tn := s.provisioned(10)
	for _, c := range []struct {
		path, method, action string
		body                 func(i int) string
	}{
		{"/v1/admin/features/" + f, http.MethodPatch, "feature.update", func(i int) string { return `{"version":1,"name":"N` + fmt.Sprint(i) + `"}` }},
		{"/v1/admin/plans/" + plan + "/archive", http.MethodPost, "plan.archive", func(i int) string { return `{"version":1,"reason":"r` + fmt.Sprint(i) + `"}` }},
		{"/v1/admin/tenants/" + tn.ID, http.MethodPatch, "tenant.update", func(i int) string { return `{"version":1,"display_name":"N` + fmt.Sprint(i) + `"}` }},
	} {
		var wg sync.WaitGroup
		out := make([]response, 2)
		start := make(chan struct{})
		for i := range out {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				out[i] = s.adminDo(c.method, c.path, c.body(i))
			}(i)
		}
		close(start)
		wg.Wait()
		wins, losses := 0, 0
		var winner response
		for _, r := range out {
			switch {
			case r.Status == http.StatusOK:
				wins++
				winner = r
			case r.code(t) == "resource_version_conflict" && r.details(t)["current_version"] == float64(2):
				losses++
			default:
				t.Errorf("%s: %d %s", c.action, r.Status, r.Body)
			}
		}
		if wins != 1 || losses != 1 {
			t.Fatalf("%s: %d won and %d lost, want one of each", c.action, wins, losses)
		}
		var audits int
		_ = s.seedDB.QueryRow(`SELECT count(*) FROM audit_log WHERE action = $1`, c.action).Scan(&audits)
		if audits != 1 {
			t.Errorf("%s wrote %d audit rows, want 1", c.action, audits)
		}
		got := s.adminDo(http.MethodGet, strings.TrimSuffix(c.path, "/archive"), "")
		w := winner.json(t)
		g := got.json(t)
		if str(w, "name") != str(g, "name") || str(w, "display_name") != str(g, "display_name") || num(t, g, "version") != 2 {
			t.Errorf("%s: stored %s, the winner wrote %s", c.action, got.Body, winner.Body)
		}
	}
}

// TestDeletingAnAssignedPlanIsPlanInUseOverHTTP closes IP-08 DoD 4 on the
// wire: a plan any tenant holds, a soft-deleted one included, is refused and
// unchanged; an unassigned one is deleted.
func TestDeletingAnAssignedPlanIsPlanInUseOverHTTP(t *testing.T) {
	s := newStack(t)
	_, plan, tn := s.provisioned(10)
	path := "/v1/admin/plans/" + plan
	if r := s.adminDo(http.MethodDelete, path, `{"reason":"cleanup"}`); r.code(t) != "plan_in_use" {
		t.Fatalf("deleting an assigned plan: %d %s", r.Status, r.Body)
	}
	mustStatus(t, s.adminDo(http.MethodDelete, "/v1/admin/tenants/"+tn.ID, `{"reason":"churned"}`), http.StatusOK)
	if r := s.adminDo(http.MethodDelete, path, `{"reason":"cleanup"}`); r.code(t) != "plan_in_use" {
		t.Fatalf("deleting a plan held only by a deleted tenant: %d %s", r.Status, r.Body)
	}
	mustStatus(t, s.adminDo(http.MethodGet, path, ""), http.StatusOK)
	free := uniq("free")
	s.createPlan(free, 1, "never", str(mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/plans/"+plan, ""), http.StatusOK)["entitlements"].([]any)[0].(map[string]any), "feature_key"))
	mustStatus(t, s.adminDo(http.MethodDelete, "/v1/admin/plans/"+free, `{"reason":"never sold"}`), http.StatusNoContent)
	if r := s.adminDo(http.MethodGet, "/v1/admin/plans/"+free, ""); r.code(t) != "plan_not_found" {
		t.Fatalf("a deleted plan: %d %s", r.Status, r.Body)
	}
}

// TestDeletingAReferencedFeatureIsFeatureInUseOverHTTP: a feature a plan holds
// is refused; an unreferenced one is deleted.
func TestDeletingAReferencedFeatureIsFeatureInUseOverHTTP(t *testing.T) {
	s := newStack(t)
	f, _, _ := s.provisioned(10)
	if r := s.adminDo(http.MethodDelete, "/v1/admin/features/"+f, `{"reason":"cleanup"}`); r.code(t) != "feature_in_use" {
		t.Fatalf("deleting a referenced feature: %d %s", r.Status, r.Body)
	}
	free := uniq("free")
	s.createFeature(free)
	mustStatus(t, s.adminDo(http.MethodDelete, "/v1/admin/features/"+free, `{"reason":"mistake"}`), http.StatusNoContent)
	if r := s.adminDo(http.MethodGet, "/v1/admin/features/"+free, ""); r.code(t) != "feature_not_found" {
		t.Fatalf("a deleted feature: %d %s", r.Status, r.Body)
	}
}

// TestChangingAReferencedUnitIsRefusedOverHTTP closes IP-08 DoD 5 on the wire
// (INV-F2): an unreferenced feature's unit changes; a referenced one's is 409
// feature_unit_immutable and nothing changes.
func TestChangingAReferencedUnitIsRefusedOverHTTP(t *testing.T) {
	s := newStack(t)
	free := uniq("unit")
	s.createFeature(free)
	m := mustStatus(t, s.adminDo(http.MethodPatch, "/v1/admin/features/"+free, `{"version":1,"unit":"request"}`), http.StatusOK)
	if str(m, "unit") != "request" {
		t.Fatalf("the unit did not change: %v", m)
	}
	f, _, _ := s.provisioned(10)
	path := "/v1/admin/features/" + f
	if r := s.adminDo(http.MethodPatch, path, `{"version":1,"unit":"request"}`); r.code(t) != "feature_unit_immutable" {
		t.Fatalf("changing a referenced unit: %d %s", r.Status, r.Body)
	}
	got := mustStatus(t, s.adminDo(http.MethodGet, path, ""), http.StatusOK)
	if str(got, "unit") != "token" || num(t, got, "version") != 1 {
		t.Fatalf("a refused unit change changed the feature: %v", got)
	}
}

// TestTenantCreateRefusesPIIAtTheBoundary closes IP-08 DoD 7: every
// identifier in IP-08's corpus is refused naming external_id, no row is
// written, and neither the response nor the log contains the value.
func TestTenantCreateRefusesPIIAtTheBoundary(t *testing.T) {
	s := newStack(t)
	_, plan, _ := s.provisioned(10)
	for _, bad := range []string{"a@b", "jane.doe@example.com", strings.Repeat("x", 129), "", "two words", "zoë"} {
		r := s.adminDo(http.MethodPost, "/v1/admin/tenants",
			`{"external_id":`+jsonString(bad)+`,"display_name":"T","plan_key":"`+plan+`"}`)
		if r.code(t) != "validation_failed" || !strings.Contains(string(r.Body), `"field":"external_id"`) {
			t.Errorf("external_id %q: %d %s", bad, r.Status, r.Body)
		}
		if bad != "" && (strings.Contains(string(r.Body), bad) || strings.Contains(s.logs.String(), bad)) {
			t.Errorf("the value %q is echoed", bad)
		}
	}
	var n int
	if err := s.seedDB.QueryRow(`SELECT count(*) FROM tenants`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d tenants exist, want the one provisioned (%v)", n, err)
	}
}

func jsonString(s string) string { return mustJSON(s) }
