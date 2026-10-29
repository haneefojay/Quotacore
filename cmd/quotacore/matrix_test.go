package main

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// A provocation is one request that must be refused with one code on one
// route, and the balance it must leave alone.
type provocation struct {
	code   string
	route  string // the matrix's endpoint, "POST /v1/consume"
	tenant tenant
	ftr    string
	send   func() response
}

const (
	routeConsume = "POST /v1/consume"
	routeRefund  = "POST /v1/refund"
	routeCheck   = "POST /v1/check"
	routeBalance = "GET /v1/balance"
)

// provocations seeds what each refusal needs and returns every refusal the
// four data-plane routes can give in v0.1, in an order where the last ones
// break the stack (the control plane is closed for the final group).
func provocations(t *testing.T, s *stack) []provocation {
	t.Helper()
	ext := externalID("mx")
	a := s.tenant(ext, "UTC", monthlyAnchor,
		ent{"mx.f", 100, "monthly", false}, ent{"mx.zero", 100, "monthly", false},
		ent{"mx.full", 100, "monthly", false}, ent{"mx.old", 100, "monthly", true},
		ent{"mx.gone", 100, "monthly", false})
	s.seedCurrent(a, "mx.f", 50)
	s.seedCurrent(a, "mx.zero", 0)
	s.seedCurrent(a, "mx.full", 100)
	s.seedCurrent(a, "mx.old", 100)
	paused := s.tenant(externalID("mxp"), "UTC", monthlyAnchor, ent{"mx.f", 100, "monthly", false})
	s.seedCurrent(paused, "mx.f", 50)
	if _, err := s.seedDB.Exec(`UPDATE tenants SET state = 'suspended' WHERE id = $1`, paused.ID); err != nil {
		t.Fatal(err)
	}
	mars := s.tenant(externalID("mxz"), "UTC", monthlyAnchor, ent{"mx.f", 100, "monthly", false})
	s.seedCurrent(mars, "mx.f", 50)
	if _, err := s.seedDB.Exec(`UPDATE tenants SET timezone = 'Mars/Olympus' WHERE id = $1`, mars.ID); err != nil {
		t.Fatal(err)
	}
	cold := s.tenant(externalID("mxc"), "UTC", monthlyAnchor, ent{"mx.f", 100, "monthly", false})
	s.seedCurrent(cold, "mx.f", 50)

	reused := newKey()
	if r := s.consume(s.runtime, "mx.f", 1, a.External, reused); r.Status != 200 {
		t.Fatalf("seed the reused key: %d %s", r.Status, r.Body)
	}
	refundKey := newKey()
	if r := s.do(http.MethodPost, "/v1/refund", s.runtime, metered("mx.f", 1, a.External), "Idempotency-Key", refundKey); r.Status != 200 {
		t.Fatalf("seed the reused refund key: %d %s", r.Status, r.Body)
	}

	post := func(path, token, body string, headers ...string) func() response {
		return func() response { return s.do(http.MethodPost, path, token, body, headers...) }
	}
	withKey := func(path, token, body string) func() response {
		return post(path, token, body, "Idempotency-Key", newKey())
	}
	bal := func(token, query string) func() response {
		return func() response { return s.do(http.MethodGet, "/v1/balance?"+query, token, "") }
	}
	body := metered("mx.f", 1, a.External)
	big := `{"feature_key":"mx.f","amount":1,"tenant_external_id":"` + a.External + `","metadata":{"x":"` + strings.Repeat("a", 70000) + `"}}`
	bigMeta := `{"feature_key":"mx.f","amount":1,"tenant_external_id":"` + a.External + `","metadata":{"x":"` + strings.Repeat("a", 5000) + `"}}`
	bad := `{"feature_key":"mx.f","amount":1.5,"tenant_external_id":"` + a.External + `"}`
	neg := metered("mx.f", -1, a.External)
	window := `{"feature_key":"mx.f","amount":1,"tenant_external_id":"` + a.External + `","window":"rolling_1h"}`
	invalidTok := "qc_runtime_0123456789abcdef.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	var out []provocation
	add := func(code, route string, tn tenant, send func() response) {
		out = append(out, provocation{code: code, route: route, tenant: tn, ftr: "mx.f", send: send})
	}
	for _, m := range []struct{ route, path string }{{routeConsume, "/v1/consume"}, {routeRefund, "/v1/refund"}, {routeCheck, "/v1/check"}} {
		add("validation_failed", m.route, a, withKey(m.path, s.runtime, bad))
		add("invalid_window", m.route, a, withKey(m.path, s.runtime, window))
		add("unauthorized", m.route, a, withKey(m.path, "", body))
		add("api_key_invalid", m.route, a, withKey(m.path, invalidTok, body))
		add("feature_not_in_plan", m.route, a, withKey(m.path, s.runtime, metered("mx.nothing", 1, a.External)))
		add("feature_archived", m.route, a, withKey(m.path, s.runtime, metered("mx.old", 1, a.External)))
		add("tenant_suspended", m.route, paused, withKey(m.path, s.runtime, metered("mx.f", 1, paused.External)))
		add("tenant_not_found", m.route, a, withKey(m.path, s.runtime, metered("mx.f", 1, "nobody-"+a.External)))
		add("service_unavailable", m.route, a, withKey(m.path, s.runtime, metered("mx.gone", 1, a.External)))
		add("internal_error", m.route, mars, withKey(m.path, s.runtime, metered("mx.f", 1, mars.External)))
		if m.route != routeCheck {
			add("invalid_amount", m.route, a, withKey(m.path, s.runtime, neg))
			add("missing_idempotency_key", m.route, a, post(m.path, s.runtime, body))
			add("payload_too_large", m.route, a, withKey(m.path, s.runtime, big))
			add("payload_too_large", m.route, a, withKey(m.path, s.runtime, bigMeta))
		} else {
			// check does not list 413; an oversized body is validation_failed.
			add("validation_failed", m.route, a, post(m.path, s.runtime, big))
		}
	}
	add("idempotency_key_reuse", routeConsume, a, post("/v1/consume", s.runtime, metered("mx.f", 2, a.External), "Idempotency-Key", reused))
	add("idempotency_key_reuse", routeRefund, a, post("/v1/refund", s.runtime, metered("mx.f", 2, a.External), "Idempotency-Key", refundKey))
	add("quota_exceeded", routeConsume, a, withKey("/v1/consume", s.runtime, metered("mx.zero", 1, a.External)))
	add("refund_exceeds_grant", routeRefund, a, withKey("/v1/refund", s.runtime, metered("mx.full", 1, a.External)))
	out[len(out)-1].ftr, out[len(out)-2].ftr = "mx.full", "mx.zero"

	add("validation_failed", routeBalance, a, bal(s.runtime, "feature_key=Bad"))
	add("unauthorized", routeBalance, a, bal("", "tenant_external_id="+a.External))
	add("api_key_invalid", routeBalance, a, bal(invalidTok, "tenant_external_id="+a.External))
	add("feature_not_found", routeBalance, a, bal(s.runtime, "tenant_external_id="+a.External+"&feature_key=mx.nothing"))
	add("tenant_not_found", routeBalance, a, bal(s.runtime, "tenant_external_id=nobody"))
	add("service_unavailable", routeBalance, a, bal(s.runtime, "tenant_external_id="+a.External))
	add("internal_error", routeBalance, mars, bal(s.runtime, "tenant_external_id="+mars.External))

	// The control plane goes away last: a tenant the snapshot has not seen is
	// then control_plane_unavailable, on every route, while the keys stay
	// cached.
	closeControlPlane := func(send func() response) func() response {
		return func() response {
			_ = s.sql.Close()
			return send()
		}
	}
	add("control_plane_unavailable", routeConsume, cold, closeControlPlane(withKey("/v1/consume", s.runtime, metered("mx.f", 1, cold.External))))
	add("control_plane_unavailable", routeRefund, cold, withKey("/v1/refund", s.runtime, metered("mx.f", 1, cold.External)))
	add("control_plane_unavailable", routeCheck, cold, withKey("/v1/check", s.runtime, metered("mx.f", 1, cold.External)))
	add("control_plane_unavailable", routeBalance, cold, bal(s.runtime, "tenant_external_id="+cold.External))
	return out
}

// rateLimitProvocations is the one refusal that needs its own limit: a stack
// whose per-key bucket holds one request.
func rateLimitProvocations(t *testing.T) (*stack, []provocation) {
	s := newStack(t, func(o *stackOptions) {
		o.env["QUOTACORE_RATELIMIT_RUNTIME_RPS"] = "0.001"
		o.env["QUOTACORE_RATELIMIT_RUNTIME_BURST"] = "1"
	})
	tn := s.tenant(externalID("rl"), "UTC", monthlyAnchor, ent{"mx.f", 100, "monthly", false})
	s.seedCurrent(tn, "mx.f", 50)
	var out []provocation
	for _, m := range []struct{ route, method, path, body string }{
		{routeConsume, http.MethodPost, "/v1/consume", metered("mx.f", 1, tn.External)},
		{routeRefund, http.MethodPost, "/v1/refund", metered("mx.f", 1, tn.External)},
		{routeCheck, http.MethodPost, "/v1/check", metered("mx.f", 1, tn.External)},
		{routeBalance, http.MethodGet, "/v1/balance?tenant_external_id=" + tn.External, ""},
	} {
		tok := s.key("runtime")
		s.do(m.method, m.path, tok, m.body, "Idempotency-Key", newKey()) // spends the one token
		out = append(out, provocation{code: "rate_limited", route: m.route, tenant: tn, ftr: "mx.f",
			send: func() response { return s.do(m.method, m.path, tok, m.body, "Idempotency-Key", newKey()) }})
	}
	return s, out
}

var matrixLine = regexp.MustCompile("^\\| `([A-Z]+ /[^`]+)` \\| (.+) \\|$")

func matrixRows(t *testing.T) map[string]map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "product", "error-catalog.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		m := matrixLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		codes := map[string]bool{}
		for _, c := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(m[2], -1) {
			codes[c[1]] = true
		}
		rows[m[1]] = codes
	}
	return rows
}

// TestEndpointErrorMatrix: on each of the four data-plane routes, every code
// its row of the endpoint x error matrix lists and v0.1 can reach is provoked,
// and every refusal returns a code the row lists. insufficient_scope is listed
// on all four and unreachable on them, because both scopes pass the data
// plane; the test says so rather than skipping it silently.
func TestEndpointErrorMatrix(t *testing.T) {
	rows := matrixRows(t)
	s := newStack(t)
	all := provocations(t, s)
	_, limited := rateLimitProvocations(t)
	all = append(all, limited...)

	seen := map[string]map[string]bool{}
	for _, p := range all {
		r := p.send()
		got := r.code(t)
		if got != p.code {
			t.Errorf("%s, provoking %s: got %d %s", p.route, p.code, r.Status, r.Body)
		}
		if !rows[p.route][got] {
			t.Errorf("%s returned %s, which its matrix row does not list", p.route, got)
		}
		if seen[p.route] == nil {
			seen[p.route] = map[string]bool{}
		}
		seen[p.route][got] = true
	}
	unreachable := map[string]string{"insufficient_scope": "both scopes pass the data plane (NFR-S4)"}
	for _, route := range []string{routeConsume, routeRefund, routeCheck, routeBalance} {
		if rows[route] == nil {
			t.Fatalf("no matrix row for %s", route)
		}
		var missing []string
		for code := range rows[route] {
			if !seen[route][code] && unreachable[code] == "" {
				missing = append(missing, code)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s: the row lists %v and no request provoked them", route, missing)
		}
	}
}

// TestT08EveryRefusalChangesNothing (IP-05 DoD 3, T-08): for every code of the
// catalogue a data-plane route can return, the refusal leaves the balance
// hash, its expiry, the tenant's idempotency records and the events handed to
// the ledger exactly as they were. The control plane's refusals are provoked
// by adminProvocations, which checks they change nothing in either store.
func TestT08EveryRefusalChangesNothing(t *testing.T) {
	s := newStack(t)
	all := provocations(t, s)
	ls, limited := rateLimitProvocations(t)
	covered := map[string]bool{}
	check := func(ps []provocation, st *stack) {
		for _, p := range ps {
			before := st.state(p.tenant, p.ftr)
			st.flush()
			ledgerBefore := st.rows(`SELECT count(*) FROM usage_events`) + st.rows(`SELECT count(*) FROM usage_event_dead_letters`)
			r := p.send()
			if r.code(t) != p.code {
				t.Errorf("%s %s: got %d %s", p.route, p.code, r.Status, r.Body)
				continue
			}
			time.Sleep(time.Millisecond)
			if after := st.state(p.tenant, p.ftr); !reflect.DeepEqual(before, after) {
				t.Errorf("%s on %s changed the state:\nbefore %+v\nafter  %+v", p.code, p.route, before, after)
			}
			// The ledger half (IP-05 DoD 3, IP-12): a refusal writes no row
			// and no dead letter.
			st.flush()
			if n := st.rows(`SELECT count(*) FROM usage_events`) + st.rows(`SELECT count(*) FROM usage_event_dead_letters`); n != ledgerBefore {
				t.Errorf("%s on %s wrote %d ledger rows", p.code, p.route, n-ledgerBefore)
			}
			covered[p.code] = true
		}
	}
	check(all, s)
	check(limited, ls)

	s2 := newStack(t)
	tn := s2.tenant(externalID("scope"), "UTC", monthlyAnchor, ent{"mx.f", 100, "monthly", false})
	s2.seedCurrent(tn, "mx.f", 50)
	before := s2.state(tn, "mx.f")
	if r := s2.do(http.MethodPost, "/v1/admin/tenants/"+tn.ID+"/grant", s2.runtime, `{}`); r.code(t) != "insufficient_scope" {
		t.Errorf("a runtime key on grant: %d %s", r.Status, r.Body)
	}
	if after := s2.state(tn, "mx.f"); !reflect.DeepEqual(before, after) {
		t.Error("insufficient_scope changed the state")
	}
	covered["insufficient_scope"] = true
	for _, codes := range adminProvocations(t) {
		for code := range codes {
			covered[code] = true
		}
	}

	for code := range catalogue(t) {
		if !covered[code] {
			t.Errorf("%s is not provoked", code)
		}
	}
}

var catalogueRow = regexp.MustCompile("(?m)^\\| `([a-z][a-z0-9_]*)` \\| (\\d{3}) \\|")

func catalogue(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "product", "error-catalog.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	section := text[strings.Index(text, "\n## 2. "):strings.Index(text, "\n## 3. ")]
	codes := map[string]bool{}
	for _, m := range catalogueRow.FindAllStringSubmatch(section, -1) {
		codes[m[1]] = true
	}
	if len(codes) < 30 {
		t.Fatalf("parsed %d catalogue codes", len(codes))
	}
	return codes
}
