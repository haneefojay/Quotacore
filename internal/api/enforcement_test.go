package api

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/store"
)

// TestEveryResponseCarriesTheRequestId (IP-07 DoD 6): every response has
// X-Request-Id, including a 404, a 405, the probes and the documentation; an
// error body's request_id is the header's; a valid inbound id is adopted and
// an invalid one is replaced.
func TestEveryResponseCarriesTheRequestId(t *testing.T) {
	h := newHarness(t)
	responses := map[string]reply{
		"healthz":       h.do(http.MethodGet, "/healthz", "", ""),
		"readyz":        h.do(http.MethodGet, "/readyz", "", ""),
		"openapi":       h.do(http.MethodGet, "/openapi.json", "", ""),
		"docs":          h.do(http.MethodGet, "/docs", "", ""),
		"404":           h.do(http.MethodGet, "/nowhere", "", ""),
		"405":           h.do(http.MethodDelete, "/v1/consume", "", ""),
		"401":           h.do(http.MethodPost, "/v1/consume", "", meteredBody),
		"403":           h.do(http.MethodGet, "/v1/admin/keys", h.runtime, ""),
		"400":           h.consume(`{}`),
		"200":           h.consume(meteredBody),
		"balance":       h.do(http.MethodGet, "/v1/balance?tenant_external_id=nobody", h.runtime, ""),
		"admin passes":  h.do(http.MethodGet, "/v1/admin/keys", h.admin, ""),
		"status (bare)": h.do(http.MethodGet, "/v1/admin/status", h.runtime, ""),
	}
	for name, r := range responses {
		id := r.Header.Get("X-Request-Id")
		if !requestIDShape.MatchString(id) {
			t.Errorf("%s: X-Request-Id = %q, want a ULID or a UUID", name, id)
		}
		if r.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", name, r.Header.Get("Cache-Control"))
		}
		if r.Status >= 400 && len(r.Body) > 0 {
			if _, bodyID, _, _ := r.envelope(t); bodyID != id {
				t.Errorf("%s: body request_id %q differs from the header %q", name, bodyID, id)
			}
		}
	}
	if r := responses["status (bare)"]; r.Status != http.StatusForbidden || len(r.Body) != 0 {
		t.Errorf("a runtime key on GET /v1/admin/status = %d %q, want a bare 403 (the route returns no envelope)", r.Status, r.Body)
	}

	for inbound, adopted := range map[string]bool{
		"01J8Z6M2QK9V3X7B4C0N5TDEFA":           true,
		"0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b": true,
		"01J8Z6M2QK9V3X7B4C0N5TDEFU":           false, // U is not Crockford
		"hello\nlevel=error":                   false,
		"":                                     false,
	} {
		r := h.do(http.MethodGet, "/healthz", "", "", "X-Request-Id", inbound)
		got := r.Header.Get("X-Request-Id")
		if adopted && got != inbound {
			t.Errorf("valid inbound %q was replaced by %q", inbound, got)
		}
		if !adopted && got == inbound {
			t.Errorf("invalid inbound %q was adopted", inbound)
		}
	}
}

// TestEveryLogLineOfARequestCarriesItsId (IP-07 DoD 6, DR-043, NFR-O6): each
// request writes log lines that all carry its id and no other, and the
// captured output never contains the token's secret, the external_id or the
// metadata.
func TestEveryLogLineOfARequestCarriesItsId(t *testing.T) {
	h := newHarness(t)
	const metadata = `{"conv":"c_secret_conversation"}`
	body := `{"feature_key":"llm.tokens.output","amount":100,"tenant_external_id":"acme-prod-01","metadata":` + metadata + `}`
	ids := map[string]bool{}
	for _, r := range []reply{
		h.consume(body),
		h.consume(`{"amount":1.5}`),
		h.do(http.MethodPost, "/v1/check", h.runtime, body),
		h.do(http.MethodGet, "/v1/balance?tenant_external_id=acme-prod-01", h.runtime, ""),
		h.do(http.MethodGet, "/nowhere", "", ""),
		h.do(http.MethodGet, "/healthz", "", ""),
		h.do(http.MethodGet, "/v1/admin/keys", h.runtime, ""),
	} {
		ids[r.Header.Get("X-Request-Id")] = true
	}
	h.scripts.err = errors.New("boom")
	ids[h.consume(body).Header.Get("X-Request-Id")] = true

	lines := h.logLines()
	if len(lines) < len(ids) {
		t.Fatalf("%d log lines for %d requests", len(lines), len(ids))
	}
	seen := map[string]bool{}
	for _, l := range lines {
		id, _ := l["request_id"].(string)
		if !ids[id] {
			t.Errorf("a log line carries request_id %q, which is no request's: %v", id, l)
		}
		seen[id] = true
	}
	for id := range ids {
		if !seen[id] {
			t.Errorf("request %s wrote no log line", id)
		}
	}

	h.logMu.Lock()
	out := h.logs.String()
	h.logMu.Unlock()
	_, secret, _ := strings.Cut(h.runtime, ".")
	for what, needle := range map[string]string{
		"the secret":      secret,
		"the token":       h.runtime,
		"the external_id": "acme-prod-01",
		"the metadata":    "c_secret_conversation",
		"a metadata key":  `"conv"`,
	} {
		if strings.Contains(out, needle) {
			t.Errorf("the log contains %s", what)
		}
	}
}

var catalogueRow = regexp.MustCompile("(?m)^\\| `([a-z][a-z0-9_]*)` \\| (\\d{3}) \\|")

func catalogueCodes(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "product", "error-catalog.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	from, to := strings.Index(text, "\n## 2. "), strings.Index(text, "\n## 3. ")
	if from < 0 || to < from {
		t.Fatal("error-catalog.md no longer has sections 2 and 3")
	}
	codes := map[string]bool{}
	for _, m := range catalogueRow.FindAllStringSubmatch(text[from:to], -1) {
		codes[m[1]] = true
	}
	return codes
}

// TestErrorsTotalCodesAreTheCatalogue (IP-07 DoD 7): the code label values of
// quotacore_errors_total, gathered from a registry, are exactly the codes of
// error-catalog.md section 2 and exactly the generated ErrorCode enum.
func TestErrorsTotalCodesAreTheCatalogue(t *testing.T) {
	h := newHarness(t)
	reg := prometheus.NewRegistry()
	if err := h.plane.Register(reg); err != nil {
		t.Fatal(err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	gathered := map[string]bool{}
	for _, f := range families {
		if f.GetName() != "quotacore_errors_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "code" {
					gathered[l.GetValue()] = true
				}
			}
		}
	}
	enum := map[string]bool{}
	for _, c := range contract.ErrorCode("").AllValues() {
		enum[string(c)] = true
	}
	catalogue := catalogueCodes(t)
	compare := func(aName string, a map[string]bool, bName string, b map[string]bool) {
		for c := range a {
			if !b[c] {
				t.Errorf("%s has %s and %s does not", aName, c, bName)
			}
		}
	}
	compare("the registry", gathered, "the catalogue", catalogue)
	compare("the catalogue", catalogue, "the registry", gathered)
	compare("the enum", enum, "the catalogue", catalogue)
	compare("the catalogue", catalogue, "the enum", enum)
	if len(gathered) == 0 {
		t.Fatal("no quotacore_errors_total series were gathered")
	}

	// Every envelope goes through the counter, labelled with the operation.
	before := counterValue(t, reg, "quotacore_errors_total", "validation_failed", "POST /v1/consume")
	h.consume(`{}`)
	if after := counterValue(t, reg, "quotacore_errors_total", "validation_failed", "POST /v1/consume"); after != before+1 {
		t.Errorf("errors_total{validation_failed, POST /v1/consume} went %v → %v, want +1", before, after)
	}
}

func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels ...string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	metric:
		for _, m := range f.GetMetric() {
			for i, l := range m.GetLabel() {
				if i >= len(labels) || l.GetValue() != labels[i] {
					continue metric
				}
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestARuntimeKeyIsRefusedOnEveryControlPlaneRoute (IP-07 DoD 8, NFR-S4): every
// /v1/admin operation in the contract answers a runtime key with 403
// insufficient_scope in the envelope, naming both scopes, before any handler
// exists; GET /v1/admin/status, which returns no envelope, answers a bare 403.
// An admin key passes the gate on all of them.
func TestARuntimeKeyIsRefusedOnEveryControlPlaneRoute(t *testing.T) {
	h := newHarness(t)
	n := 0
	for _, op := range envelope.MustOperations() {
		if op.RequiredScope != "admin" {
			continue
		}
		n++
		path := strings.NewReplacer("{feature_key}", "llm.tokens", "{id}", "0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b").Replace(op.Path)
		body := ""
		if op.HasBody {
			body = `{}`
		}
		r := h.do(op.Method, path, h.runtime, body)
		if r.Status != http.StatusForbidden {
			t.Errorf("%s with a runtime key = %d, want 403", op.Endpoint(), r.Status)
			continue
		}
		if op.ID == "status" {
			if len(r.Body) != 0 {
				t.Errorf("%s: a body on a route that returns no envelope: %s", op.Endpoint(), r.Body)
			}
		} else {
			code, _, details, _ := r.envelope(t)
			if code != "insufficient_scope" || details["required"] != "admin" || details["presented"] != "runtime" {
				t.Errorf("%s: %s %v, want insufficient_scope naming admin and runtime", op.Endpoint(), code, details)
			}
		}
		if r := h.do(op.Method, path, h.admin, body); r.Status == http.StatusForbidden || r.Status == http.StatusUnauthorized {
			t.Errorf("%s with an admin key = %d, want the gate to pass", op.Endpoint(), r.Status)
		}
	}
	if n < 25 {
		t.Errorf("only %d admin operations were found in the contract", n)
	}
}

// TestAuthenticationFailures: no credential is unauthorized, anything else
// that is not a valid key is api_key_invalid, both with WWW-Authenticate, and
// an admin key works on the data plane.
func TestAuthenticationFailures(t *testing.T) {
	h := newHarness(t)
	for name, header := range map[string]string{
		"none":      "",
		"malformed": "Bearer qc_runtime_zz.nope",
		"unknown":   "Bearer qc_runtime_0123456789abcdef.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"basic":     "Basic dXNlcjpwYXNz",
	} {
		r := h.do(http.MethodPost, "/v1/check", "", meteredBody, "Authorization", header)
		want := "api_key_invalid"
		if header == "" {
			want = "unauthorized"
		}
		if code, _, _, _ := r.envelope(t); r.Status != 401 || code != want {
			t.Errorf("%s: %d %s, want 401 %s", name, r.Status, code, want)
		}
		if r.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: 401 without WWW-Authenticate", name)
		}
	}
	if r := h.do(http.MethodPost, "/v1/check", h.admin, meteredBody); r.Status != 200 {
		t.Errorf("an admin key on check = %d %s, want 200", r.Status, r.Body)
	}
}

// TestValidationAggregates: three bad fields produce one validation_failed
// naming all three; a float amount and an unknown field are refused; a
// single problem with its own code is answered with that code (IP-07, S2).
func TestValidationAggregates(t *testing.T) {
	h := newHarness(t)
	fieldsOf := func(details map[string]any) []string {
		var out []string
		list, _ := details["errors"].([]any)
		for _, e := range list {
			m, _ := e.(map[string]any)
			f, _ := m["field"].(string)
			out = append(out, f)
		}
		sort.Strings(out)
		return out
	}
	cases := []struct {
		name    string
		body    string
		headers []string
		code    string
		fields  []string
	}{
		{"three bad fields", `{"feature_key":"Bad Key","amount":4200.0,"tenant_external_id":"a@b"}`, nil,
			"validation_failed", []string{"amount", "feature_key", "tenant_external_id"}},
		{"float amount", `{"feature_key":"llm.tokens.output","amount":4200.0,"tenant_external_id":"acme-prod-01"}`, nil,
			"validation_failed", []string{"amount"}},
		{"exponent amount", `{"feature_key":"llm.tokens.output","amount":42e2,"tenant_external_id":"acme-prod-01"}`, nil,
			"validation_failed", []string{"amount"}},
		{"string amount", `{"feature_key":"llm.tokens.output","amount":"42","tenant_external_id":"acme-prod-01"}`, nil,
			"validation_failed", []string{"amount"}},
		{"above 2^53", `{"feature_key":"llm.tokens.output","amount":9007199254740992,"tenant_external_id":"acme-prod-01"}`, nil,
			"validation_failed", []string{"amount"}},
		{"unknown field", `{"feature_key":"llm.tokens.output","amount":1,"tenant_external_id":"acme-prod-01","amout":1}`, nil,
			"validation_failed", []string{"amout"}},
		{"missing fields", `{}`, nil, "validation_failed", []string{"amount", "feature_key", "tenant_external_id"}},
		{"not an object", `[1]`, nil, "validation_failed", []string{"body"}},
		{"negative alone", `{"feature_key":"llm.tokens.output","amount":-5,"tenant_external_id":"acme-prod-01"}`, nil,
			"invalid_amount", nil},
		{"negative and another", `{"feature_key":"llm.tokens.output","amount":-5,"tenant_external_id":"a@b"}`, nil,
			"validation_failed", []string{"amount", "tenant_external_id"}},
		{"window alone", `{"feature_key":"llm.tokens.output","amount":1,"tenant_external_id":"acme-prod-01","window":"rolling_1h"}`, nil,
			"invalid_window", nil},
		{"no key alone", meteredBody, []string{"Idempotency-Key", ""}, "missing_idempotency_key", nil},
		{"no key and a bad field", `{"feature_key":"llm.tokens.output","amount":1.5,"tenant_external_id":"acme-prod-01"}`,
			[]string{"Idempotency-Key", ""}, "validation_failed", []string{"Idempotency-Key", "amount"}},
		{"bad key", meteredBody, []string{"Idempotency-Key", "short"}, "validation_failed", []string{"Idempotency-Key"}},
		{"key with a colon", meteredBody, []string{"Idempotency-Key", "order:8841:charge"}, "", nil},
		{"form content type", meteredBody, []string{"Content-Type", "application/x-www-form-urlencoded"},
			"validation_failed", []string{"Content-Type"}},
		{"compressed", meteredBody, []string{"Content-Encoding", "gzip"}, "validation_failed", []string{"Content-Encoding"}},
	}
	for _, c := range cases {
		calls := len(h.scripts.calls)
		r := h.consume(c.body, c.headers...)
		if c.code == "" {
			if r.Status != 200 {
				t.Errorf("%s: %d %s, want 200", c.name, r.Status, r.Body)
			}
			continue
		}
		code, _, details, _ := r.envelope(t)
		if r.Status != 400 || code != c.code {
			t.Errorf("%s: %d %s, want 400 %s (%s)", c.name, r.Status, code, c.code, r.Body)
			continue
		}
		if c.fields != nil {
			if got := fieldsOf(details); strings.Join(got, ",") != strings.Join(c.fields, ",") {
				t.Errorf("%s: details.errors names %v, want %v", c.name, got, c.fields)
			}
		}
		if len(h.scripts.calls) != calls {
			t.Errorf("%s: a refused request reached the script", c.name)
		}
	}
	// check takes no key and refuses no missing one; balance requires its tenant.
	if r := h.do(http.MethodPost, "/v1/check", h.runtime, meteredBody); r.Status != 200 {
		t.Errorf("check without a key = %d, want 200", r.Status)
	}
	r := h.do(http.MethodGet, "/v1/balance", h.runtime, "")
	if code, _, details, _ := r.envelope(t); r.Status != 400 || code != "validation_failed" || fieldsOf(details)[0] != "tenant_external_id" {
		t.Errorf("balance without a tenant = %d %s %v", r.Status, code, details)
	}
}

// TestBodyAndMetadataCaps (NFR-T6, S4): a body over the cap is 413 before it is
// read, metadata over its cap is 413 after decoding, and on check, whose row
// does not list 413, both are validation_failed.
func TestBodyAndMetadataCaps(t *testing.T) {
	h := newHarness(t, func(o *RouterOptions) { o.MaxBodyBytes = 1024 })
	big := `{"feature_key":"llm.tokens.output","amount":1,"tenant_external_id":"acme-prod-01","metadata":{"x":"` + strings.Repeat("a", 2000) + `"}}`
	if code, _, _, _ := h.consume(big).envelope(t); code != "payload_too_large" {
		t.Errorf("an oversized body = %s, want payload_too_large", code)
	}
	r := h.do(http.MethodPost, "/v1/check", h.runtime, big)
	if code, _, _, _ := r.envelope(t); r.Status != 400 || code != "validation_failed" {
		t.Errorf("an oversized check body = %d %s, want 400 validation_failed", r.Status, code)
	}
	// An unauthenticated oversized body is refused for its size first: the
	// cap is step 1.
	if r := h.do(http.MethodPost, "/v1/consume", "", big); r.Status != 413 {
		t.Errorf("unauthenticated oversized body = %d, want 413", r.Status)
	}

	h = newHarness(t)
	meta := `{"feature_key":"llm.tokens.output","amount":1,"tenant_external_id":"acme-prod-01","metadata":{"x":"` + strings.Repeat("a", 4100) + `"}}`
	r = h.consume(meta)
	code, _, details, _ := r.envelope(t)
	if r.Status != 413 || code != "payload_too_large" || details["field"] != "metadata" {
		t.Errorf("oversized metadata = %d %s %v, want 413 naming metadata", r.Status, code, details)
	}
	if r := h.do(http.MethodPost, "/v1/check", h.runtime, meta); r.Status != 400 {
		t.Errorf("oversized metadata on check = %d, want 400", r.Status)
	}
}

// TestGateRefusalsAndTheirOrder: an archived feature, a suspended tenant, a
// feature outside the plan and an unknown tenant are refused before the
// script, with their codes; an archived feature is refused before the
// tenant's state is considered (DR-015).
func TestGateRefusalsAndTheirOrder(t *testing.T) {
	h := newHarness(t)
	cases := map[string]struct {
		body   string
		status int
		code   string
	}{
		"archived":    {`{"feature_key":"old.feature","amount":1,"tenant_external_id":"acme-prod-01"}`, 403, "feature_archived"},
		"suspended":   {`{"feature_key":"llm.tokens.output","amount":1,"tenant_external_id":"paused"}`, 403, "tenant_suspended"},
		"not in plan": {`{"feature_key":"img.generations","amount":1,"tenant_external_id":"acme-prod-01"}`, 403, "feature_not_in_plan"},
		"unknown":     {`{"feature_key":"llm.tokens.output","amount":1,"tenant_external_id":"nobody"}`, 404, "tenant_not_found"},
	}
	for name, c := range cases {
		for _, path := range []string{"/v1/consume", "/v1/refund", "/v1/check"} {
			calls := len(h.scripts.calls)
			r := h.do(http.MethodPost, path, h.runtime, c.body, "Idempotency-Key", "01JCQ9W3K2M9XQ3F8V6T0B5NDE")
			if code, _, _, _ := r.envelope(t); r.Status != c.status || code != c.code {
				t.Errorf("%s on %s: %d %s, want %d %s", name, path, r.Status, code, c.status, c.code)
			}
			if len(h.scripts.calls) != calls {
				t.Errorf("%s on %s reached the script", name, path)
			}
		}
	}
}

// TestScriptOutcomesBecomeResponses: the mapping in the IP-07 plan, section 1,
// from each script answer to each route's response.
func TestScriptOutcomesBecomeResponses(t *testing.T) {
	end := windowEnd
	base := script.Outcome{Transition: script.Current, Balance: 40, Limit: 1000, Bonus: 5, Index: 2, Start: windowStart, End: &end}
	with := func(d script.Decision) script.Outcome { o := base; o.Decision = d; return o }

	type want struct {
		status int
		code   string
		replay bool
	}
	cases := []struct {
		name  string
		out   script.Outcome
		err   error
		route string
		want  want
	}{
		{"consume applied", with(script.Applied), nil, "/v1/consume", want{200, "", false}},
		{"consume replayed", with(script.Replayed), nil, "/v1/consume", want{200, "", true}},
		{"consume denied", with(script.Denied), nil, "/v1/consume", want{429, "quota_exceeded", false}},
		{"refund ceiling", with(script.RefusedCeiling), nil, "/v1/refund", want{409, "refund_exceeds_grant", false}},
		{"reuse", script.Outcome{}, script.ErrIdempotencyKeyReuse, "/v1/consume", want{409, "idempotency_key_reuse", false}},
		{"missing hash", script.Outcome{}, script.ErrStateMissing, "/v1/check", want{503, "service_unavailable", false}},
		{"store timeout", script.Outcome{}, errors.New("i/o timeout"), "/v1/refund", want{503, "service_unavailable", false}},
		{"invalid", script.Outcome{}, script.ErrInvalidArgument, "/v1/consume", want{500, "internal_error", false}},
		{"bad key part", script.Outcome{}, store.ErrBadKeyPart, "/v1/consume", want{503, "service_unavailable", false}},
		{"check denied", with(script.Denied), nil, "/v1/check", want{200, "", false}},
	}
	for _, c := range cases {
		h := newHarness(t)
		h.scripts.out, h.scripts.err = c.out, c.err
		r := h.do(http.MethodPost, c.route, h.runtime, meteredBody, "Idempotency-Key", "01JCQ9W3K2M9XQ3F8V6T0B5NDE")
		if r.Status != c.want.status {
			t.Errorf("%s: %d %s, want %d", c.name, r.Status, r.Body, c.want.status)
			continue
		}
		if (r.Header.Get("Idempotent-Replay") == "true") != c.want.replay {
			t.Errorf("%s: Idempotent-Replay = %q", c.name, r.Header.Get("Idempotent-Replay"))
		}
		if c.want.code == "" {
			continue
		}
		code, _, details, retry := r.envelope(t)
		if code != c.want.code {
			t.Errorf("%s: code %s, want %s", c.name, code, c.want.code)
		}
		if (r.Status == 503) != (retry != nil) {
			t.Errorf("%s: retry_after_ms = %v; a 503 carries it and nothing else here does", c.name, retry)
		}
		if code == "quota_exceeded" {
			if details["cycle_end"] != "2026-11-01T00:00:00Z" || details["balance"] != 40.0 || details["requested"] != 100.0 ||
				details["limit"] != 1000.0 || details["bonus"] != 5.0 || details["feature_key"] != "llm.tokens.output" {
				t.Errorf("quota_exceeded details = %v", details)
			}
			if r.Header.Get("Retry-After") != "" {
				t.Error("quota_exceeded carries Retry-After")
			}
		}
	}

	// The 200 bodies are the contract's types, and a replay differs from the
	// original only in replayed.
	h := newHarness(t)
	h.scripts.out = with(script.Applied)
	first := h.consume(meteredBody)
	h.scripts.out = with(script.Replayed)
	second := h.consume(meteredBody)
	var a, b contract.MeteredResponse
	if err := a.UnmarshalJSON(first.Body); err != nil {
		t.Fatal(err)
	}
	if err := b.UnmarshalJSON(second.Body); err != nil {
		t.Fatal(err)
	}
	if a.Replayed || !b.Replayed {
		t.Errorf("replayed = %v then %v, want false then true", a.Replayed, b.Replayed)
	}
	b.Replayed = false
	ja, _ := a.MarshalJSON()
	jb, _ := b.MarshalJSON()
	if string(ja) != string(jb) {
		t.Errorf("a replay differs beyond replayed:\n%s\n%s", ja, jb)
	}
	if a.Consumed != 100 || a.Balance != 40 || time.Time(a.CycleEnd.Value) != windowEnd {
		t.Errorf("consume body = %+v", a)
	}

	// A check's request carries the window the cycle engine computed for the
	// instant of acceptance, and no key.
	req := h.scripts.calls[0]
	if req.TenantID != tenantID || req.Limit != 1000 || req.Window.Start != windowStart || req.IdempotencyKey == "" {
		t.Errorf("the script was asked %+v", req)
	}
}

// TestAPanicIsA500WithTheRequestId: a defect inside a handler answers 500
// internal_error in the envelope with the request id, and is logged with it.
func TestAPanicIsA500WithTheRequestId(t *testing.T) {
	h := newHarness(t)
	rt := &router{log: slog.New(slog.NewJSONHandler(syncWriter{mu: h.logMu, buf: h.logs}, nil)),
		now: time.Now, dataPlane: map[string]string{}}
	h.handler = rt.accept(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("defect") }))
	r := h.do(http.MethodGet, "/x", "", "")
	code, id, _, _ := r.envelope(t)
	if r.Status != 500 || code != "internal_error" || id != r.Header.Get("X-Request-Id") {
		t.Errorf("panic = %d %s %s", r.Status, code, id)
	}
	found := false
	for _, l := range h.logLines() {
		if l["msg"] == "panic" && l["request_id"] == id {
			found = true
		}
	}
	if !found {
		t.Error("the panic was not logged with the request id")
	}
}

// TestRateLimitIsPerKeyAndDistinct (DR-040): past the burst a key is refused
// with 429 rate_limited, Retry-After, retry_after_ms and the limit, and no
// cycle_end; another key is unaffected; nothing refused reaches the script;
// and the refusal is counted by key prefix.
func TestRateLimitIsPerKeyAndDistinct(t *testing.T) {
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, func(o *RouterOptions) {
		o.Now = func() time.Time { return at }
		o.RateLimit = dataplane.NewRateLimiter(2, 2, 10)
	})
	for i := 0; i < 2; i++ {
		if r := h.do(http.MethodPost, "/v1/check", h.runtime, meteredBody); r.Status != 200 {
			t.Fatalf("request %d inside the burst = %d", i, r.Status)
		}
	}
	calls := len(h.scripts.calls)
	r := h.do(http.MethodPost, "/v1/check", h.runtime, meteredBody)
	code, _, details, retry := r.envelope(t)
	if r.Status != 429 || code != "rate_limited" {
		t.Fatalf("past the burst = %d %s, want 429 rate_limited", r.Status, code)
	}
	if r.Header.Get("Retry-After") != "1" || retry == nil || *retry != 500 || details["limit_per_second"] != 2.0 {
		t.Errorf("Retry-After %q, retry_after_ms %v, details %v; want 1, 500 and the limit", r.Header.Get("Retry-After"), retry, details)
	}
	if _, has := details["cycle_end"]; has {
		t.Error("rate_limited carries cycle_end")
	}
	if len(h.scripts.calls) != calls {
		t.Error("a rate-limited request reached the script")
	}
	if r := h.do(http.MethodPost, "/v1/check", h.admin, meteredBody); r.Status != 200 {
		t.Errorf("another key = %d, want 200", r.Status)
	}
	reg := prometheus.NewRegistry()
	if err := h.plane.Register(reg); err != nil {
		t.Fatal(err)
	}
	prefix := strings.TrimPrefix(strings.Split(h.runtime, ".")[0], "qc_runtime_")
	if v := counterValue(t, reg, "quotacore_rate_limited_total", prefix); v != 1 {
		t.Errorf("rate_limited_total{%s} = %v, want 1", prefix, v)
	}
}

// TestBalanceReportsTheStoredValues: the balance route reports each stored hash
// verbatim with consumed derived, in the tenant's zone, a never entitlement
// with both ends null, an archived feature's missing hash omitted, an active
// feature's missing hash a 503 for the whole answer, and an unknown feature
// 404 feature_not_found.
func TestBalanceReportsTheStoredValues(t *testing.T) {
	h := newHarness(t)
	end := windowEnd
	h.balances.stored["llm.tokens.output"] = store.Stored{Present: true, Balance: 700, Limit: 1000, Bonus: 50,
		Index: 3, Start: windowStart, End: &end}
	h.balances.stored["api.requests"] = store.Stored{Present: true, Balance: 50, Limit: 50, Start: windowStart}
	r := h.do(http.MethodGet, "/v1/balance?tenant_external_id=acme-prod-01", h.runtime, "")
	var got contract.BalanceResponse
	if err := got.UnmarshalJSON(r.Body); err != nil || r.Status != 200 {
		t.Fatalf("%d %s: %v", r.Status, r.Body, err)
	}
	if len(got.Entries) != 2 || got.Entries[0].FeatureKey != "api.requests" || got.Entries[1].FeatureKey != "llm.tokens.output" {
		t.Fatalf("entries = %+v", got.Entries)
	}
	never, monthly := got.Entries[0], got.Entries[1]
	if !never.CycleStart.Null || !never.CycleEnd.Null || never.Consumed != 0 {
		t.Errorf("never entry = %+v, want both ends null", never)
	}
	if monthly.Consumed != 350 || monthly.Balance != 700 || monthly.Bonus != 50 {
		t.Errorf("monthly entry = %+v, want consumed = 1000 + 50 - 700", monthly)
	}
	if !strings.Contains(string(r.Body), `"cycle_end":"2026-11-01T00:00:00Z"`) {
		t.Errorf("body %s does not render the end with an offset", r.Body)
	}

	one := h.do(http.MethodGet, "/v1/balance?tenant_external_id=acme-prod-01&feature_key=old.feature", h.runtime, "")
	if !strings.Contains(string(one.Body), `"entries":[]`) || one.Status != 200 {
		t.Errorf("an archived feature whose hash expired = %d %s, want 200 with no entry", one.Status, one.Body)
	}
	nf := h.do(http.MethodGet, "/v1/balance?tenant_external_id=acme-prod-01&feature_key=img.generations", h.runtime, "")
	if code, _, _, _ := nf.envelope(t); nf.Status != 404 || code != "feature_not_found" {
		t.Errorf("a feature outside the plan = %d %s, want 404 feature_not_found", nf.Status, code)
	}
	if r := h.do(http.MethodGet, "/v1/balance?tenant_external_id=paused", h.runtime, ""); r.Status == 403 {
		t.Error("a suspended tenant's balance was refused; it is readable (DR-016)")
	}

	reg := prometheus.NewRegistry()
	if err := h.plane.Register(reg); err != nil {
		t.Fatal(err)
	}
	delete(h.balances.stored, "api.requests")
	missing := h.do(http.MethodGet, "/v1/balance?tenant_external_id=acme-prod-01", h.runtime, "")
	if code, _, _, _ := missing.envelope(t); missing.Status != 503 || code != "service_unavailable" {
		t.Errorf("a missing active hash = %d %s, want 503", missing.Status, code)
	}
	if v := counterValue(t, reg, "quotacore_balance_key_missing_total"); v != 1 {
		t.Errorf("balance_key_missing_total = %v, want 1", v)
	}
}
