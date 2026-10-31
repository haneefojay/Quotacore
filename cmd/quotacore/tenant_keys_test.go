package main

import (
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// issueBound issues a runtime key bound to one tenant over the API (DR-053).
func (s *stack) issueBound(name, external string) map[string]any {
	s.t.Helper()
	k := mustStatus(s.t, s.adminDo(http.MethodPost, "/v1/admin/keys",
		`{"name":"`+name+`","scope":"runtime","tenant_external_id":"`+external+`"}`), http.StatusCreated)
	if k["tenant_external_id"] != external || k["scope"] != "runtime" {
		s.t.Fatalf("a bound key reads back as %v", k)
	}
	return k
}

// boundTenants provisions one plan with a metered feature and four tenants on
// it: a, whose key the test binds, and b, c and d, which the key must not
// reach. c is suspended and d deleted.
func (s *stack) boundTenants() (feature string, a, b, c, d tenant) {
	s.t.Helper()
	feature, plan := uniq("bk"), uniq("bp")
	s.createFeature(feature)
	s.createPlan(plan, 100, "daily", feature)
	a = asTenant(s.t, s.createTenant(externalID("ba"), plan, "UTC"))
	b = asTenant(s.t, s.createTenant(externalID("bb"), plan, "UTC"))
	c = asTenant(s.t, s.createTenant(externalID("bc"), plan, "UTC"))
	d = asTenant(s.t, s.createTenant(externalID("bd"), plan, "UTC"))
	mustStatus(s.t, s.adminDo(http.MethodPost, "/v1/admin/tenants/"+c.ID+"/suspend", `{"version":1,"reason":"t19"}`), http.StatusOK)
	mustStatus(s.t, s.adminDo(http.MethodDelete, "/v1/admin/tenants/"+d.ID, `{"reason":"t19"}`), http.StatusOK)
	return feature, a, b, c, d
}

// TestT19ABoundKeyReachesOneTenant is IP-20 DoD 1: a key bound to A
// addresses B, a suspended C, a deleted D and a tenant that never existed on
// every data-plane route, and every call is 404 tenant_not_found with exactly
// the details an unknown tenant gets, changing nothing. On every admin route
// the key is 403 insufficient_scope. A's own calls succeed.
func TestT19ABoundKeyReachesOneTenant(t *testing.T) {
	s := newStack(t)
	feature, a, b, c, d := s.boundTenants()
	tok := str(s.issueBound(uniq("t19"), a.External), "key")
	unknown := tenant{ID: "00000000-0000-4000-8000-000000000000", External: externalID("never")}

	routes := func(external string) map[string]func() response {
		return map[string]func() response{
			"consume": func() response { return s.consume(tok, feature, 1, external, newKey()) },
			"refund": func() response {
				return s.do(http.MethodPost, "/v1/refund", tok, metered(feature, 1, external), "Idempotency-Key", newKey())
			},
			"check":   func() response { return s.do(http.MethodPost, "/v1/check", tok, metered(feature, 1, external)) },
			"balance": func() response { return s.do(http.MethodGet, "/v1/balance?tenant_external_id="+external, tok, "") },
			"balance one": func() response {
				return s.do(http.MethodGet, "/v1/balance?tenant_external_id="+external+"&feature_key="+feature, tok, "")
			},
		}
	}
	// What an unknown tenant is told, from the unbound runtime key, is the
	// answer a bound key gets for every tenant but its own.
	want := map[string]map[string]any{}
	for name, call := range routes(unknown.External) {
		r := call()
		if r.Status != http.StatusNotFound || r.code(t) != "tenant_not_found" {
			t.Fatalf("%s for an unknown tenant: %d %s", name, r.Status, r.Body)
		}
		want[name] = r.details(t)
	}
	for _, target := range []tenant{b, c, d} {
		before := s.state(target, feature)
		s.flush()
		ledger := s.rows(`SELECT count(*) FROM usage_events WHERE tenant_id = $1`, target.ID)
		for name, call := range routes(target.External) {
			r := call()
			if r.Status != http.StatusNotFound || r.code(t) != "tenant_not_found" {
				t.Errorf("%s for %s with A's key: %d %s", name, target.External, r.Status, r.Body)
				continue
			}
			got := r.details(t)
			if got["tenant_external_id"] != target.External || len(got) != len(want[name]) {
				t.Errorf("%s for %s: details %v; an unknown tenant's are %v", name, target.External, got, want[name])
			}
		}
		if after := s.state(target, feature); !reflect.DeepEqual(before, after) {
			t.Errorf("A's key changed %s's state:\nbefore %+v\nafter  %+v", target.External, before, after)
		}
		s.flush()
		if n := s.rows(`SELECT count(*) FROM usage_events WHERE tenant_id = $1`, target.ID); n != ledger {
			t.Errorf("A's key wrote %d ledger rows for %s", n-ledger, target.External)
		}
	}

	// The key works for its own tenant on every route.
	if r := s.consume(tok, feature, 3, a.External, newKey()); r.Status != http.StatusOK || num(t, r.json(t), "balance") != 97 {
		t.Fatalf("consume on its own tenant: %d %s", r.Status, r.Body)
	}
	for name, call := range routes(a.External) {
		if r := call(); r.Status != http.StatusOK {
			t.Errorf("%s on its own tenant: %d %s", name, r.Status, r.Body)
		}
	}

	// Every admin route: the scope gate runs before any handler, so a bound
	// key reaches no configuration and no tenant through the control plane.
	for _, op := range adminOps() {
		r := s.do(op.Method, samplePath(op.Path), tok, "{}")
		if r.Status != http.StatusForbidden {
			t.Errorf("%s with a bound runtime key: %d %s", op.Endpoint(), r.Status, r.Body)
			continue
		}
		if len(op.Codes) > 0 && r.code(t) != "insufficient_scope" {
			t.Errorf("%s with a bound runtime key: %s", op.Endpoint(), r.Body)
		}
	}
}

// TestT19ABoundKeyIsRevokedLikeAnyOther is IP-20 DoD 3 (INV-K1): revoking a
// bound key refuses it on the very next request, as for an unbound one.
func TestT19ABoundKeyIsRevokedLikeAnyOther(t *testing.T) {
	s := newStack(t)
	feature, a, _, _, _ := s.boundTenants()
	k := s.issueBound(uniq("t19-rev"), a.External)
	if r := s.consume(str(k, "key"), feature, 1, a.External, newKey()); r.Status != http.StatusOK {
		t.Fatalf("before the revoke: %d %s", r.Status, r.Body)
	}
	mustStatus(t, s.adminDo(http.MethodDelete, "/v1/admin/keys/"+str(k, "id"), ""), http.StatusNoContent)
	if r := s.consume(str(k, "key"), feature, 1, a.External, newKey()); r.Status != http.StatusUnauthorized || r.code(t) != "api_key_invalid" {
		t.Fatalf("the next request after the revoke: %d %s", r.Status, r.Body)
	}
}

// TestT19ADeletedTenantHasNoUsableKey is IP-20 DoD 6 (INV-X7): deleting a
// tenant revokes its bound keys in the same transaction, each audited, so the
// serving instance refuses the key at once and a second instance, which had
// it cached, within one refresh interval. A key bound to another tenant, and
// an unbound key, are untouched.
func TestT19ADeletedTenantHasNoUsableKey(t *testing.T) {
	s := newStack(t)
	other := newStack(t, func(o *stackOptions) { o.dbURL = s.dbURL })
	feature, a, b, _, _ := s.boundTenants()
	k := s.issueBound(uniq("t19-del"), a.External)
	kb := s.issueBound(uniq("t19-keep"), b.External)
	tok := str(k, "key")
	if r := other.consume(tok, feature, 1, a.External, newKey()); r.Status != http.StatusOK {
		t.Fatalf("the second instance before the delete: %d %s", r.Status, r.Body)
	}
	mustStatus(t, s.adminDo(http.MethodDelete, "/v1/admin/tenants/"+a.ID, `{"reason":"churned"}`), http.StatusOK)
	deleted := time.Now()

	if r := s.consume(tok, feature, 1, a.External, newKey()); r.Status != http.StatusUnauthorized || r.code(t) != "api_key_invalid" {
		t.Errorf("the serving instance after the delete: %d %s", r.Status, r.Body)
	}
	// One refresh interval is QUOTACORE_CONFIG_REFRESH_INTERVAL, 1 s in the
	// stack; the invalidation the delete publishes is usually much sooner.
	for {
		r := other.consume(tok, feature, 1, a.External, newKey())
		if r.Status == http.StatusUnauthorized && r.code(t) == "api_key_invalid" {
			break
		}
		if time.Since(deleted) > 2*time.Second {
			t.Fatalf("the second instance still accepts the deleted tenant's key after %v: %d %s", time.Since(deleted), r.Status, r.Body)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var state, reason string
	if err := s.seedDB.QueryRow(`SELECT k.state::text, coalesce(a.reason, '') FROM api_keys k
  JOIN audit_log a ON a.subject_id = k.id AND a.action = 'api_key.revoke' WHERE k.id = $1`, str(k, "id")).Scan(&state, &reason); err != nil {
		t.Fatalf("the revocation's audit row: %v", err)
	}
	if state != "revoked" || reason != "tenant deleted" {
		t.Errorf("the deleted tenant's key is %s, audited with %q", state, reason)
	}
	if r := s.consume(str(kb, "key"), feature, 1, b.External, newKey()); r.Status != http.StatusOK {
		t.Errorf("another tenant's bound key after the delete: %d %s", r.Status, r.Body)
	}
	if r := s.consume(s.runtime, feature, 1, b.External, newKey()); r.Status != http.StatusOK {
		t.Errorf("an unbound key after the delete: %d %s", r.Status, r.Body)
	}
	listed := mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/keys?tenant_external_id="+a.External, ""), http.StatusOK)
	keys, _ := listed["keys"].([]any)
	if len(keys) != 1 || keys[0].(map[string]any)["state"] != "revoked" {
		t.Errorf("the deleted tenant's key list: %v", listed)
	}
}

// TestT19IssuingABoundKey is the admin surface's half of DR-053, and IP-20
// DoD 8's: the request a v0.1 client sends, with no tenant, still issues an
// unbound key; an admin key cannot be bound; an unknown tenant is 404 and a
// deleted one 409; the list filters by tenant and reports the binding.
func TestT19IssuingABoundKey(t *testing.T) {
	s := newStack(t)
	_, a, b, _, d := s.boundTenants()
	plain := s.issueKey(uniq("t19-plain"), "runtime")
	if v, present := plain["tenant_external_id"]; !present || v != nil {
		t.Errorf("an unbound key's tenant_external_id: %v (present %t), want null", v, present)
	}
	bound := s.issueBound(uniq("t19-a"), a.External)
	s.issueBound(uniq("t19-b"), b.External)

	r := s.adminDo(http.MethodPost, "/v1/admin/keys", `{"name":"`+uniq("t19-adm")+`","scope":"admin","tenant_external_id":"`+a.External+`"}`)
	if r.Status != http.StatusBadRequest || r.code(t) != "validation_failed" || !strings.Contains(string(r.Body), "tenant_external_id") {
		t.Errorf("an admin key bound to a tenant: %d %s", r.Status, r.Body)
	}
	if r := s.adminDo(http.MethodPost, "/v1/admin/keys", `{"name":"`+uniq("t19-u")+`","scope":"runtime","tenant_external_id":"`+externalID("nobody")+`"}`); r.code(t) != "tenant_not_found" {
		t.Errorf("a key bound to an unknown tenant: %d %s", r.Status, r.Body)
	}
	if r := s.adminDo(http.MethodPost, "/v1/admin/keys", `{"name":"`+uniq("t19-d")+`","scope":"runtime","tenant_external_id":"`+d.External+`"}`); r.code(t) != "tenant_deleted" {
		t.Errorf("a key bound to a deleted tenant: %d %s", r.Status, r.Body)
	}

	listed := mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/keys?tenant_external_id="+a.External, ""), http.StatusOK)
	keys, _ := listed["keys"].([]any)
	if len(keys) != 1 || keys[0].(map[string]any)["id"] != str(bound, "id") || keys[0].(map[string]any)["tenant_external_id"] != a.External {
		t.Errorf("A's keys: %v", listed)
	}
	all := mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/keys?limit=100", ""), http.StatusOK)
	for _, k := range all["keys"].([]any) {
		if _, present := k.(map[string]any)["tenant_external_id"]; !present {
			t.Errorf("a listed key has no tenant_external_id: %v", k)
		}
	}
}

// TestT19TheTenantIsNotALabel is IP-20 DoD 7 (NFR-O2): consuming through
// keys bound to two tenants adds no series to quotacore_consume_total that
// the same outcomes through an unbound key had not already made, and no
// series anywhere carries a tenant label.
func TestT19TheTenantIsNotALabel(t *testing.T) {
	s := newStack(t)
	feature, a, b, _, _ := s.boundTenants()
	series := func() []string {
		families, err := s.reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range families {
			for _, m := range f.GetMetric() {
				for _, l := range m.GetLabel() {
					if strings.Contains(l.GetName(), "tenant") {
						t.Errorf("%s carries the label %s", f.GetName(), l.GetName())
					}
				}
				if f.GetName() == "quotacore_consume_total" {
					out = append(out, m.String())
				}
			}
		}
		sort.Strings(out)
		return out
	}
	// The outcomes a bound key will produce, first through the unbound key.
	s.consume(s.runtime, feature, 1, a.External, newKey())
	s.consume(s.runtime, feature, 1000, a.External, newKey())
	before := len(series())
	for _, tn := range []tenant{a, b} {
		tok := str(s.issueBound(uniq("t19-m"), tn.External), "key")
		s.consume(tok, feature, 1, tn.External, newKey())
		s.consume(tok, feature, 1000, tn.External, newKey())
	}
	if after := len(series()); after != before {
		t.Errorf("quotacore_consume_total has %d series after bound keys and %d before", after, before)
	}
}

// TestT19ACapReportsWhatItCounts is IP-20 DoD 5 (DR-054): one tenant holds an
// allocation cap, a plain metered feature, a boolean and a rolling one, and
// GET /v1/balance carries cap on the first alone, counting creations less
// deletions. A cap that resets, and a cap on a boolean, are refused.
func TestT19ACapReportsWhatItCounts(t *testing.T) {
	s := newStack(t)
	capped, counter, flag, window := uniq("cap"), uniq("cnt"), uniq("flg"), uniq("win")
	f := mustStatus(t, s.adminDo(http.MethodPost, "/v1/admin/features",
		`{"feature_key":"`+capped+`","name":"Projects","unit":"project","allocation_cap":true}`), http.StatusCreated)
	if f["allocation_cap"] != true {
		t.Fatalf("the cap feature reads back as %v", f)
	}
	if plain := s.createFeature(counter); plain["allocation_cap"] != false {
		t.Errorf("a plain feature's allocation_cap: %v", plain)
	}
	s.createBooleanFeature(flag)
	s.createRollingFeature(window)

	if r := s.adminDo(http.MethodPost, "/v1/admin/features",
		`{"feature_key":"`+uniq("bad")+`","name":"B","unit":"flag","kind":"boolean","allocation_cap":true}`); r.code(t) != "validation_failed" ||
		!strings.Contains(string(r.Body), "allocation_cap") {
		t.Errorf("a boolean cap: %d %s", r.Status, r.Body)
	}
	if r := s.adminDo(http.MethodPost, "/v1/admin/plans", `{"plan_key":"`+uniq("cp")+`","name":"P","entitlements":[`+
		`{"feature_key":"`+capped+`","limit":3,"reset_interval":"monthly"}]}`); r.code(t) != "validation_failed" ||
		!strings.Contains(string(r.Body), "entitlements[0].reset_interval") {
		t.Errorf("a cap that resets monthly: %d %s", r.Status, r.Body)
	}

	plan := uniq("cp")
	mustStatus(t, s.adminDo(http.MethodPost, "/v1/admin/plans", `{"plan_key":"`+plan+`","name":"P","entitlements":[`+
		`{"feature_key":"`+capped+`","limit":3,"reset_interval":"never"},`+
		`{"feature_key":"`+counter+`","limit":100,"reset_interval":"daily"},`+
		`{"feature_key":"`+flag+`","limit":null,"reset_interval":null},`+
		`{"feature_key":"`+window+`","limit":10,"reset_interval":null,"window":"rolling_5m"}]}`), http.StatusCreated)
	tn := asTenant(t, s.createTenant(externalID("cap"), plan, "UTC"))

	// UC-10: three creations, a fourth refused, one deletion.
	for i := 0; i < 3; i++ {
		mustStatus(t, s.consume(s.runtime, capped, 1, tn.External, newKey()), http.StatusOK)
	}
	if r := s.consume(s.runtime, capped, 1, tn.External, newKey()); r.code(t) != "quota_exceeded" {
		t.Fatalf("a fourth creation: %d %s", r.Status, r.Body)
	}
	mustStatus(t, s.do(http.MethodPost, "/v1/refund", s.runtime, metered(capped, 1, tn.External), "Idempotency-Key", newKey()), http.StatusOK)
	mustStatus(t, s.consume(s.runtime, counter, 5, tn.External, newKey()), http.StatusOK)

	bal := mustStatus(t, s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External, s.runtime, ""), http.StatusOK)
	entries, _ := bal["entries"].([]any)
	if len(entries) != 4 {
		t.Fatalf("four entries, got %v", bal)
	}
	for _, e := range entries {
		m := e.(map[string]any)
		c, present := m["cap"]
		if str(m, "feature_key") != capped {
			if present {
				t.Errorf("%s carries a cap: %v", str(m, "feature_key"), m)
			}
			continue
		}
		cm, _ := c.(map[string]any)
		if cm["resource"] != "project" || num(t, cm, "count") != 2 || num(t, m, "consumed") != 2 {
			t.Errorf("the cap's entry: %v", m)
		}
	}
	one := mustStatus(t, s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External+"&feature_key="+capped, s.runtime, ""), http.StatusOK)
	if e := one["entries"].([]any)[0].(map[string]any); e["cap"] == nil {
		t.Errorf("one feature's balance: %v", one)
	}
	// The administrative balance routes answer the same shape.
	set := mustStatus(t, s.adminDo(http.MethodPost, "/v1/admin/tenants/"+tn.ID+"/set",
		`{"feature_key":"`+capped+`","value":0,"reason":"t19"}`), http.StatusOK)
	if c, _ := set["cap"].(map[string]any); c == nil || num(t, c, "count") != 3 {
		t.Errorf("a set on a cap answers %v", set)
	}
}
