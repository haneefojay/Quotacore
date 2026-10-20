package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/admin"
	"github.com/quotacore/quotacore/internal/db"
	"github.com/quotacore/quotacore/internal/envelope"
)

// ip10Ops are the admin operations whose handlers are IP-10's. Until then each
// is a bare 404, and the codes only they return are listed in ip10Only.
var ip10Ops = map[string]bool{
	"updatePlan": true, "applyPlan": true, "planImpact": true, "grantTenant": true,
	"setTenantBalance": true, "forceRollover": true, "upsertTenantOverride": true, "clearTenantOverride": true,
}

// ip10Only are the catalogue's codes that only an IP-10 route returns. The
// list has to be emptied there, not forgotten.
var ip10Only = map[string]string{
	"confirmation_required": "IP-10 (plan apply)", "grant_exceeds_ceiling": "IP-10 (grant)",
	"reset_not_supported": "IP-10 (force-rollover)",
}

// adminRow is one row of the endpoint x error matrix that names admin routes.
type adminRow struct {
	endpoints []string
	codes     map[string]bool
}

var backticked = regexp.MustCompile("`([^`]+)`")

func adminRows(t *testing.T) []adminRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "product", "error-catalog.md"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []adminRow
	for _, line := range strings.Split(string(raw), "\n") {
		cells := strings.Split(line, " | ")
		if len(cells) != 2 || !strings.HasPrefix(cells[0], "| `") || !strings.Contains(cells[0], "/v1/admin/") {
			continue
		}
		row := adminRow{codes: map[string]bool{}}
		for _, m := range backticked.FindAllStringSubmatch(cells[0], -1) {
			row.endpoints = append(row.endpoints, m[1])
		}
		for _, m := range backticked.FindAllStringSubmatch(cells[1], -1) {
			row.codes[m[1]] = true
		}
		rows = append(rows, row)
	}
	if len(rows) < 9 {
		t.Fatalf("parsed %d admin matrix rows", len(rows))
	}
	return rows
}

// adminOps maps each admin endpoint to its contract operation.
func adminOps() map[string]envelope.Operation {
	out := map[string]envelope.Operation{}
	for _, op := range envelope.MustOperations() {
		if strings.HasPrefix(op.Path, "/v1/admin/") {
			out[op.Endpoint()] = op
		}
	}
	return out
}

// samplePath fills a contract path's parameters with well-formed values that
// name nothing.
func samplePath(path string) string {
	r := strings.NewReplacer("{id}", uuid.NewString(), "{feature_key}", "nothing.here")
	return r.Replace(path)
}

// panicStore panics on the reads a test turns it on for, so internal_error is
// provoked through the route's real recovery rather than a stub.
type panicStore struct {
	admin.Store
	on *atomic.Bool
}

func (p panicStore) boom() {
	if p.on.Load() {
		panic("the test's store panics")
	}
}
func (p panicStore) ListKeys(ctx context.Context, pg admin.Page) ([]contract.Key, error) {
	p.boom()
	return p.Store.ListKeys(ctx, pg)
}
func (p panicStore) GetFeature(ctx context.Context, k string) (contract.Feature, error) {
	p.boom()
	return p.Store.GetFeature(ctx, k)
}
func (p panicStore) GetPlan(ctx context.Context, id string) (contract.Plan, error) {
	p.boom()
	return p.Store.GetPlan(ctx, id)
}
func (p panicStore) GetTenant(ctx context.Context, id string) (contract.Tenant, error) {
	p.boom()
	return p.Store.GetTenant(ctx, id)
}
func (p panicStore) ListAudit(ctx context.Context, f admin.AuditFilter, pg admin.Page) ([]contract.AuditEntry, error) {
	p.boom()
	return p.Store.ListAudit(ctx, f, pg)
}
func (p panicStore) ListEvents(ctx context.Context, id string, f admin.EventFilter, pg admin.Page) (admin.EventPage, error) {
	p.boom()
	return p.Store.ListEvents(ctx, id, f, pg)
}

// controlDigest is the control plane's resources as a refusal must leave them:
// every row of the catalogue, the tenants and the keys, and the audit log's
// length. A key's last use is the one column a request may move.
func (s *stack) controlDigest() string {
	s.t.Helper()
	var out []string
	for _, q := range []string{
		`SELECT coalesce(md5(string_agg(t::text, ',' ORDER BY t::text)), '') FROM features t`,
		`SELECT coalesce(md5(string_agg(t::text, ',' ORDER BY t::text)), '') FROM plans t`,
		`SELECT coalesce(md5(string_agg(t::text, ',' ORDER BY t::text)), '') FROM plan_entitlements t`,
		`SELECT coalesce(md5(string_agg(t::text, ',' ORDER BY t::text)), '') FROM tenants t`,
		`SELECT coalesce(md5(string_agg(id::text || name || scope || coalesce(revoked_at::text, ''), ',' ORDER BY id)), '') FROM api_keys`,
		`SELECT count(*)::text FROM audit_log`,
	} {
		var v string
		if err := s.seedDB.QueryRow(q).Scan(&v); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, v)
	}
	return strings.Join(out, " ")
}

// adminProvocation is one admin request that must be refused with code.
type adminProvocation struct {
	code     string
	endpoint string
	send     func() response
}

// adminProvocations runs every refusal IP-09's routes can give, checking each
// against its matrix row and checking that it changed nothing (T-08 on the
// control plane). It returns the codes provoked, by matrix row.
func adminProvocations(t *testing.T) map[int]map[string]bool {
	t.Helper()
	rows := adminRows(t)
	rowOf := map[string]int{}
	for i, r := range rows {
		for _, e := range r.endpoints {
			rowOf[e] = i
		}
	}
	ops := adminOps()

	_, redisURL := integrationURLs(t)
	cut := newProxy(t, hostOf(t, redisURL))
	var panics atomic.Bool
	s := newStack(t, func(o *stackOptions) {
		o.redisURL = urlVia(t, redisURL, cut.addr())
		o.adminStore = func(a *db.Admin) admin.Store { return panicStore{Store: a, on: &panics} }
	})
	f, plan, tn := s.provisioned(10)
	gone := asTenant(t, s.createTenant(externalID("gone"), plan, "UTC"))
	mustStatus(t, s.adminDo(http.MethodDelete, "/v1/admin/tenants/"+gone.ID, `{"reason":"churned"}`), http.StatusOK)
	invalidTok := "qc_admin_0123456789abcdef.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	huge := `{"name":"` + strings.Repeat("a", 70000) + `"}`

	var ps []adminProvocation
	add := func(code, method, path, token, body string) {
		ps = append(ps, adminProvocation{code: code, endpoint: method + " " + path,
			send: func() response { return s.do(method, samplePath(path), token, body) }})
	}
	at := func(code, endpoint string, send func() response) {
		ps = append(ps, adminProvocation{code: code, endpoint: endpoint, send: send})
	}
	adm := func(method, path, body string) func() response {
		return func() response { return s.adminDo(method, path, body) }
	}

	// Authentication and scope on every route IP-09 serves; the body cap on
	// every one that takes a body.
	var endpoints []string
	for e := range ops {
		endpoints = append(endpoints, e)
	}
	sort.Strings(endpoints)
	for _, e := range endpoints {
		op := ops[e]
		if ip10Ops[op.ID] || len(op.Codes) == 0 {
			continue
		}
		body := ""
		if op.HasBody {
			body = `{}`
		}
		add("unauthorized", op.Method, op.Path, "", body)
		add("api_key_invalid", op.Method, op.Path, invalidTok, body)
		add("insufficient_scope", op.Method, op.Path, s.runtime, body)
		if op.HasBody {
			add("payload_too_large", op.Method, op.Path, s.admin, huge)
		}
	}

	fp, pp, tp := "/v1/admin/features/"+f, "/v1/admin/plans/"+plan, "/v1/admin/tenants/"+tn.ID
	// Features.
	at("validation_failed", "POST /v1/admin/features", adm(http.MethodPost, "/v1/admin/features", `{}`))
	at("feature_not_found", "GET /v1/admin/features/{feature_key}", adm(http.MethodGet, "/v1/admin/features/nothing.here", ""))
	at("feature_key_exists", "POST /v1/admin/features", adm(http.MethodPost, "/v1/admin/features", `{"feature_key":"`+f+`","name":"F","unit":"token"}`))
	at("feature_in_use", "DELETE /v1/admin/features/{feature_key}", adm(http.MethodDelete, fp, `{"reason":"x"}`))
	at("feature_unit_immutable", "PATCH /v1/admin/features/{feature_key}", adm(http.MethodPatch, fp, `{"version":1,"unit":"request"}`))
	at("resource_version_conflict", "PATCH /v1/admin/features/{feature_key}", adm(http.MethodPatch, fp, `{"version":99,"name":"N"}`))
	at("not_implemented", "POST /v1/admin/features", adm(http.MethodPost, "/v1/admin/features", `{"feature_key":"`+uniq("bool")+`","name":"F","unit":"flag","kind":"boolean"}`))
	// Plans.
	at("validation_failed", "POST /v1/admin/plans", adm(http.MethodPost, "/v1/admin/plans", `{}`))
	at("plan_not_found", "GET /v1/admin/plans/{id}", adm(http.MethodGet, "/v1/admin/plans/nothing", ""))
	at("plan_key_exists", "POST /v1/admin/plans", adm(http.MethodPost, "/v1/admin/plans",
		`{"plan_key":"`+plan+`","name":"P","entitlements":[{"feature_key":"`+f+`","limit":1,"reset_interval":"never"}]}`))
	at("plan_in_use", "DELETE /v1/admin/plans/{id}", adm(http.MethodDelete, pp, `{"reason":"x"}`))
	at("resource_version_conflict", "POST /v1/admin/plans/{id}/archive", adm(http.MethodPost, pp+"/archive", `{"version":99,"reason":"x"}`))
	// Tenants.
	at("validation_failed", "POST /v1/admin/tenants", adm(http.MethodPost, "/v1/admin/tenants", `{}`))
	at("invalid_timezone", "POST /v1/admin/tenants", adm(http.MethodPost, "/v1/admin/tenants",
		`{"external_id":"`+externalID("tz")+`","display_name":"T","plan_key":"`+plan+`","timezone":"Mars/Olympus"}`))
	at("tenant_not_found", "GET /v1/admin/tenants/{id}", adm(http.MethodGet, "/v1/admin/tenants/"+uuid.NewString(), ""))
	at("tenant_exists", "POST /v1/admin/tenants", adm(http.MethodPost, "/v1/admin/tenants",
		`{"external_id":"`+tn.External+`","display_name":"T","plan_key":"`+plan+`"}`))
	at("tenant_deleted", "POST /v1/admin/tenants/{id}/suspend", adm(http.MethodPost, "/v1/admin/tenants/"+gone.ID+"/suspend", `{"version":2,"reason":"x"}`))
	at("resource_version_conflict", "PATCH /v1/admin/tenants/{id}", adm(http.MethodPatch, tp, `{"version":99,"display_name":"N"}`))
	at("payload_too_large", "POST /v1/admin/tenants", adm(http.MethodPost, "/v1/admin/tenants",
		`{"external_id":"`+externalID("big")+`","display_name":"T","plan_key":"`+plan+`","metadata":{"x":"`+strings.Repeat("a", 5000)+`"}}`))
	// Events, audit and keys.
	at("validation_failed", "GET /v1/admin/tenants/{id}/events", adm(http.MethodGet, tp+"/events?event_type=nonsense", ""))
	at("tenant_not_found", "GET /v1/admin/tenants/{id}/events", adm(http.MethodGet, "/v1/admin/tenants/"+uuid.NewString()+"/events", ""))
	at("validation_failed", "GET /v1/admin/audit", adm(http.MethodGet, "/v1/admin/audit?from=yesterday", ""))
	at("validation_failed", "POST /v1/admin/keys", adm(http.MethodPost, "/v1/admin/keys", `{}`))
	at("api_key_not_found", "DELETE /v1/admin/keys/{id}", adm(http.MethodDelete, "/v1/admin/keys/"+uuid.NewString(), ""))

	// The fast store goes away: a list page past the first needs the cursor
	// key, which this process has not read yet, and a new tenant needs its
	// opening balances.
	cursor := "e30.AAAA"
	redisDown := func(send func() response) func() response {
		return func() response { cut.set("refuse"); return send() }
	}
	at("service_unavailable", "GET /v1/admin/features", redisDown(adm(http.MethodGet, "/v1/admin/features?cursor="+cursor, "")))
	at("service_unavailable", "GET /v1/admin/plans", adm(http.MethodGet, "/v1/admin/plans?cursor="+cursor, ""))
	at("service_unavailable", "GET /v1/admin/tenants", adm(http.MethodGet, "/v1/admin/tenants?cursor="+cursor, ""))
	at("service_unavailable", "POST /v1/admin/tenants", adm(http.MethodPost, "/v1/admin/tenants",
		`{"external_id":"`+externalID("cut")+`","display_name":"T","plan_key":"`+plan+`"}`))
	at("service_unavailable", "GET /v1/admin/tenants/{id}/events", adm(http.MethodGet, tp+"/events?cursor="+cursor, ""))
	at("service_unavailable", "GET /v1/admin/audit", adm(http.MethodGet, "/v1/admin/audit?cursor="+cursor, ""))
	at("service_unavailable", "GET /v1/admin/keys", adm(http.MethodGet, "/v1/admin/keys?cursor="+cursor, ""))

	// The store panics: the route's recovery answers internal_error.
	panicking := func(send func() response) func() response {
		return func() response { cut.set("pass"); panics.Store(true); return send() }
	}
	at("internal_error", "GET /v1/admin/features/{feature_key}", panicking(adm(http.MethodGet, fp, "")))
	at("internal_error", "GET /v1/admin/plans/{id}", adm(http.MethodGet, pp, ""))
	at("internal_error", "GET /v1/admin/tenants/{id}", adm(http.MethodGet, tp, ""))
	at("internal_error", "GET /v1/admin/tenants/{id}/events", adm(http.MethodGet, tp+"/events", ""))
	at("internal_error", "GET /v1/admin/audit", adm(http.MethodGet, "/v1/admin/audit", ""))
	at("internal_error", "GET /v1/admin/keys", adm(http.MethodGet, "/v1/admin/keys", ""))

	// The control plane goes away last; the admin key stays cached.
	pgDown := func(send func() response) func() response {
		return func() response { panics.Store(false); _ = s.sql.Close(); return send() }
	}
	at("control_plane_unavailable", "GET /v1/admin/features", pgDown(adm(http.MethodGet, "/v1/admin/features", "")))
	at("control_plane_unavailable", "GET /v1/admin/plans", adm(http.MethodGet, "/v1/admin/plans", ""))
	at("control_plane_unavailable", "GET /v1/admin/tenants", adm(http.MethodGet, "/v1/admin/tenants", ""))
	at("control_plane_unavailable", "GET /v1/admin/tenants/{id}/events", adm(http.MethodGet, tp+"/events", ""))
	at("control_plane_unavailable", "GET /v1/admin/audit", adm(http.MethodGet, "/v1/admin/audit", ""))
	at("control_plane_unavailable", "GET /v1/admin/keys", adm(http.MethodGet, "/v1/admin/keys", ""))

	seen := map[int]map[string]bool{}
	for _, p := range ps {
		i, found := rowOf[p.endpoint]
		if !found {
			t.Fatalf("%s is in no matrix row", p.endpoint)
		}
		before := s.controlDigest()
		r := p.send()
		got := r.code(t)
		if got != p.code {
			t.Errorf("%s, provoking %s: got %d %s", p.endpoint, p.code, r.Status, r.Body)
			continue
		}
		if !rows[i].codes[got] {
			t.Errorf("%s returned %s, which its matrix row does not list", p.endpoint, got)
		}
		if after := s.controlDigest(); after != before {
			t.Errorf("%s on %s changed the control plane: %s, then %s", got, p.endpoint, before, after)
		}
		if seen[i] == nil {
			seen[i] = map[string]bool{}
		}
		seen[i][got] = true
	}

	// The admin limit is per client address, so it has a stack of its own
	// whose bucket holds one request.
	ls := newStack(t, func(o *stackOptions) { o.env["QUOTACORE_RATELIMIT_ADMIN_RPM"] = "1" })
	ls.adminDo(http.MethodGet, "/v1/admin/features", "")
	for i, row := range rows {
		op := ops[row.endpoints[0]]
		if ip10Ops[op.ID] || len(op.Codes) == 0 {
			continue
		}
		before := ls.controlDigest()
		r := ls.adminDo(op.Method, samplePath(op.Path), "")
		if r.code(t) != "rate_limited" || r.Header.Get("Retry-After") == "" {
			t.Errorf("%s over the admin limit: %d %s", row.endpoints[0], r.Status, r.Body)
			continue
		}
		if ls.controlDigest() != before {
			t.Errorf("rate_limited on %s changed the control plane", row.endpoints[0])
		}
		if seen[i] == nil {
			seen[i] = map[string]bool{}
		}
		seen[i]["rate_limited"] = true
	}

	// Every code an IP-09 row lists is provoked, unless only IP-10 reaches it.
	for i, row := range rows {
		served := false
		for _, e := range row.endpoints {
			if op := ops[e]; !ip10Ops[op.ID] && len(op.Codes) > 0 {
				served = true
			}
		}
		if !served {
			continue
		}
		var missing []string
		for code := range row.codes {
			if !seen[i][code] && ip10Only[code] == "" {
				missing = append(missing, code)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s: the row lists %v and no request provoked them", row.endpoints[0], missing)
		}
	}
	return seen
}

// TestAdminEndpointErrorMatrix is IP-09's half of the endpoint x error matrix
// and of T-08: on every admin route IP-09 serves, every code its row lists is
// provoked (the IP-10-only codes excepted), each refusal is a code the row
// lists, and none changes a catalogue row, a tenant, a key or the audit log.
func TestAdminEndpointErrorMatrix(t *testing.T) {
	adminProvocations(t)
}

// TestAnAdminKeyReachesEveryIP09Route: an admin key reaches every admin route
// IP-09 serves, and each IP-10 route is a bare 404 until IP-10 mounts it.
func TestAnAdminKeyReachesEveryIP09Route(t *testing.T) {
	s := newStack(t)
	ops := adminOps()
	served := 0
	for _, op := range ops {
		body := ""
		if op.HasBody {
			body = `{}`
		}
		r := s.adminDo(op.Method, samplePath(op.Path), body)
		if ip10Ops[op.ID] {
			if r.Status != http.StatusNotFound || len(r.Body) != 0 {
				t.Errorf("%s is IP-10's and answered %d %s", op.Endpoint(), r.Status, r.Body)
			}
			continue
		}
		served++
		if r.Status == http.StatusUnauthorized || r.Status == http.StatusForbidden || (r.Status == http.StatusNotFound && len(r.Body) == 0) {
			t.Errorf("an admin key on %s: %d %s", op.Endpoint(), r.Status, r.Body)
		}
	}
	if served != 24 {
		t.Errorf("IP-09 serves %d admin operations, want 24", served)
	}
	// status takes no envelope: refused bare.
	if r := s.do(http.MethodGet, "/v1/admin/status", "", ""); r.Status != http.StatusUnauthorized || len(r.Body) != 0 {
		t.Errorf("status without a key: %d %s", r.Status, r.Body)
	}
}

// TestAdminRateLimitIsPerIPAndBeforeAuthentication is IP-09 D4: the admin
// limit counts the client address (the trusted proxies' X-Forwarded-For when
// the peer is one of them), refuses before the credential is read, and leaves
// the data plane to its own limit.
func TestAdminRateLimitIsPerIPAndBeforeAuthentication(t *testing.T) {
	s := newStack(t, func(o *stackOptions) {
		o.env["QUOTACORE_RATELIMIT_ADMIN_RPM"] = "1"
		o.env["QUOTACORE_TRUSTED_PROXIES"] = "127.0.0.1"
	})
	from := func(xff, token string) response {
		return s.do(http.MethodGet, "/v1/admin/features", token, "", "X-Forwarded-For", xff)
	}
	if r := from("203.0.113.1", ""); r.code(t) != "unauthorized" {
		t.Fatalf("the first request: %d %s", r.Status, r.Body)
	}
	r := from("203.0.113.1", "")
	if r.code(t) != "rate_limited" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("the second request, before any credential: %d %s", r.Status, r.Body)
	}
	if r := from("198.51.100.7, 203.0.113.1", s.admin); r.code(t) != "rate_limited" {
		t.Errorf("a client prefixing X-Forwarded-For escaped its address: %d %s", r.Status, r.Body)
	}
	if r := from("203.0.113.2", s.admin); r.Status != http.StatusOK {
		t.Errorf("another address shares the bucket: %d %s", r.Status, r.Body)
	}
	if r := s.check("mx.f", "nobody"); r.code(t) != "tenant_not_found" {
		t.Errorf("the admin limit reached the data plane: %d %s", r.Status, r.Body)
	}

	// Without trusted proxies the header is ignored: the peer is the address.
	plain := newStack(t, func(o *stackOptions) { o.env["QUOTACORE_RATELIMIT_ADMIN_RPM"] = "1" })
	plain.do(http.MethodGet, "/v1/admin/features", plain.admin, "", "X-Forwarded-For", "203.0.113.1")
	if r := plain.do(http.MethodGet, "/v1/admin/features", plain.admin, "", "X-Forwarded-For", "203.0.113.2"); r.code(t) != "rate_limited" {
		t.Errorf("an untrusted X-Forwarded-For chose the bucket: %d %s", r.Status, r.Body)
	}
}

// TestNoSecretOrBodyValueInAnyLogLine is IP-09 DoD 8 (NFR-S6): across the key,
// tenant and refusal paths, no log line carries a key's secret, a display
// name, a metadata value or a refused identifier.
func TestNoSecretOrBodyValueInAnyLogLine(t *testing.T) {
	s := newStack(t)
	_, plan, _ := s.provisioned(10)
	k := mustStatus(t, s.adminDo(http.MethodPost, "/v1/admin/keys", `{"name":"`+uniq("log")+`","scope":"runtime"}`), http.StatusCreated)
	secret := str(k, "key")
	if secret == "" {
		t.Fatalf("no secret: %v", k)
	}
	mustStatus(t, s.adminDo(http.MethodPost, "/v1/admin/tenants",
		`{"external_id":"`+externalID("log")+`","display_name":"Display-Marker-81","plan_key":"`+plan+`","metadata":{"crm":"Meta-Marker-82"}}`), http.StatusCreated)
	s.adminDo(http.MethodPost, "/v1/admin/tenants", `{"external_id":"leak.marker@example.com","display_name":"T","plan_key":"`+plan+`"}`)
	s.adminDo(http.MethodPost, "/v1/admin/features", `{"feature_key":"`+uniq("log")+`","name":"Name-Marker-83","unit":"token","bogus":"Body-Marker-84"}`)
	s.do(http.MethodGet, "/v1/balance?tenant_external_id=nobody", secret, "")
	s.do(http.MethodGet, "/v1/admin/features", secret, "")
	logs := s.logs.String()
	if !strings.Contains(logs, "request_id") {
		t.Fatal("the stack logged nothing; the test proves nothing")
	}
	// No log attribute is named for a request field: a field's value reaches
	// a log line only by being logged under its name.
	fields := requestFieldNames(t)
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("a log line is not JSON: %s", line)
		}
		for k := range m {
			if fields[k] {
				t.Errorf("a log line carries the request field %q: %s", k, line)
			}
		}
	}
	_, tail, _ := strings.Cut(secret, ".")
	for _, v := range []string{secret, tail, "Display-Marker-81", "Meta-Marker-82", "leak.marker@example.com", "Name-Marker-83", "Body-Marker-84"} {
		if strings.Contains(logs, v) {
			t.Errorf("a log line carries %q", v)
		}
	}
}

// requestFieldNames is every name a request can carry a value under, read
// from the contract: each operation's parameters and the properties of its
// body, nested schemas included.
func requestFieldNames(t *testing.T) map[string]bool {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(contract.Document, &doc); err != nil {
		t.Fatal(err)
	}
	components := doc["components"].(map[string]any)
	resolve := func(n any) map[string]any {
		m, _ := n.(map[string]any)
		if ref, ok := m["$ref"].(string); ok {
			parts := strings.Split(strings.TrimPrefix(ref, "#/components/"), "/")
			m, _ = components[parts[0]].(map[string]any)[parts[1]].(map[string]any)
		}
		return m
	}
	names := map[string]bool{}
	seen := map[string]bool{}
	var walk func(n any)
	walk = func(n any) {
		if m, ok := n.(map[string]any); ok {
			if ref, ok := m["$ref"].(string); ok {
				if seen[ref] {
					return
				}
				seen[ref] = true
			}
			m = resolve(m)
			for name, prop := range asMap(m["properties"]) {
				names[name] = true
				walk(prop)
			}
			walk(m["items"])
			walk(m["additionalProperties"])
			for _, k := range []string{"allOf", "oneOf", "anyOf"} {
				for _, sub := range asSlice(m[k]) {
					walk(sub)
				}
			}
		}
	}
	for _, item := range asMap(doc["paths"]) {
		for _, op := range asMap(item) {
			o := asMap(op)
			for _, p := range asSlice(o["parameters"]) {
				if name, ok := resolve(p)["name"].(string); ok {
					names[strings.ToLower(name)] = true
				}
			}
			for _, media := range asMap(resolve(o["requestBody"])["content"]) {
				walk(asMap(media)["schema"])
			}
		}
	}
	names["authorization"] = true
	if !names["external_id"] || !names["metadata"] || !names["reason"] || !names["idempotency-key"] {
		t.Fatalf("the contract's request fields were not read: %v", names)
	}
	return names
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asSlice(v any) []any        { s, _ := v.([]any); return s }
