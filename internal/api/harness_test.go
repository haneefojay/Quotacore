package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/auth"
	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/observability"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/snapshot"
	"github.com/quotacore/quotacore/internal/store"
)

// The unit harness is the real router, the real auth and the real snapshot,
// with the two stores replaced: api_keys and the tenant tables are maps, and
// the scripts answer what a test tells them to. It proves what the HTTP layer
// does with each answer; the integration tests in cmd/quotacore prove the
// answers themselves against a real datastore and a real Postgres.

const tenantID = "0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b"

type memKeys struct {
	mu   sync.Mutex
	rows map[string]auth.Key
}

func (m *memKeys) LoadKey(_ context.Context, prefix string) (auth.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.rows[prefix]
	if !ok {
		return auth.Key{}, auth.ErrKeyUnknown
	}
	return k, nil
}

func (m *memKeys) ReconcileKeys(_ context.Context, prefixes []string) (map[string]auth.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]auth.Key{}
	for _, p := range prefixes {
		if k, ok := m.rows[p]; ok {
			out[p] = k
		}
	}
	return out, nil
}

func (m *memKeys) TouchLastUsed(context.Context, map[string]time.Time) error { return nil }

func (m *memKeys) issue(t *testing.T, scope auth.Scope) string {
	t.Helper()
	is, err := auth.IssueToken(scope)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.rows[is.Prefix] = auth.Key{ID: "id-" + is.Prefix, Prefix: is.Prefix, Scope: scope, State: "active", Hash: is.Hash}
	m.mu.Unlock()
	return is.Token
}

type memTenants struct {
	byExternal map[string]*snapshot.LoadedTenant
}

func (m memTenants) TenantIDByExternalID(_ context.Context, ext string) (string, error) {
	if t, ok := m.byExternal[ext]; ok {
		return t.ID, nil
	}
	return "", snapshot.ErrTenantUnknown
}

func (m memTenants) Load(_ context.Context, id string) (*snapshot.LoadedTenant, error) {
	for _, t := range m.byExternal {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, snapshot.ErrTenantUnknown
}

// scripted answers every script call with out and err, and records the calls.
type scripted struct {
	mu    sync.Mutex
	out   script.Outcome
	err   error
	calls []script.Request
	ops   []string
}

func (s *scripted) call(op string, req script.Request) (script.Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	s.ops = append(s.ops, op)
	return s.out, s.err
}

func (s *scripted) Consume(_ context.Context, r script.Request) (script.Outcome, error) {
	return s.call("consume", r)
}
func (s *scripted) Refund(_ context.Context, r script.Request) (script.Outcome, error) {
	return s.call("refund", r)
}
func (s *scripted) Check(_ context.Context, r script.Request) (script.Outcome, error) {
	return s.call("check", r)
}

type memBalances struct {
	stored map[string]store.Stored
	err    error
}

func (m memBalances) ReadBalances(_ context.Context, _ string, features []string) ([]store.Stored, error) {
	if m.err != nil {
		return nil, m.err
	}
	out := make([]store.Stored, len(features))
	for i, f := range features {
		out[i] = m.stored[f]
	}
	return out, nil
}

type harness struct {
	t        *testing.T
	handler  http.Handler
	keys     *memKeys
	scripts  *scripted
	balances *memBalances
	plane    *observability.DataPlane
	logs     *bytes.Buffer
	logMu    *sync.Mutex
	runtime  string
	admin    string
}

type syncWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

var (
	windowStart = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	windowEnd   = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
)

func newHarness(t *testing.T, opts ...func(*RouterOptions)) *harness {
	t.Helper()
	tenants := memTenants{byExternal: map[string]*snapshot.LoadedTenant{
		"acme-prod-01": {
			ID: tenantID, ExternalID: "acme-prod-01", State: "active", Timezone: "UTC",
			Anchor: windowStart, Version: 1,
			Plan: &snapshot.LoadedPlan{ID: "plan-pro", Key: "pro", Entitlements: map[string]snapshot.Entitlement{
				"llm.tokens.output": {Limit: 1000, Interval: cycle.Monthly},
				"api.requests":      {Limit: 50, Interval: cycle.Never},
				"old.feature":       {Limit: 10, Interval: cycle.Monthly, Archived: true},
			}},
		},
		"paused": {
			ID: "1192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b", ExternalID: "paused", State: "suspended", Timezone: "UTC",
			Anchor: windowStart, Version: 1,
			Plan: &snapshot.LoadedPlan{ID: "plan-basic", Key: "basic", Entitlements: map[string]snapshot.Entitlement{
				"llm.tokens.output": {Limit: 1000, Interval: cycle.Monthly},
			}},
		},
	}}
	snap := snapshot.New(snapshot.Options{Loader: tenants, MissRate: 1000})
	keys := &memKeys{rows: map[string]auth.Key{}}
	end := windowEnd
	scripts := &scripted{out: script.Outcome{Decision: script.Applied, Transition: script.Current,
		Balance: 900, Limit: 1000, Index: 0, Start: windowStart, End: &end}}
	balances := &memBalances{stored: map[string]store.Stored{}}
	var cells [][2]string
	for _, op := range envelope.MustOperations() {
		for _, c := range op.Codes {
			cells = append(cells, [2]string{string(c), op.Endpoint()})
		}
	}
	plane := observability.NewDataPlane(cells)
	buf, mu := &bytes.Buffer{}, &sync.Mutex{}
	logger := slog.New(slog.NewJSONHandler(syncWriter{mu: mu, buf: buf}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ro := RouterOptions{
		Ready: func() bool { return true },
		DataPlane: dataplane.New(dataplane.Options{
			Tenants: snap, Scripts: scripts, Balances: balances, Metrics: plane,
		}),
		Auth:      auth.New(auth.Options{Loader: keys}),
		RateLimit: dataplane.NewRateLimiter(1000, 1000, 100),
		Metrics:   plane,
		Logger:    logger,
	}
	for _, o := range opts {
		o(&ro)
	}
	h := &harness{t: t, handler: NewRouter(ro), keys: keys, scripts: scripts, balances: balances,
		plane: plane, logs: buf, logMu: mu}
	h.runtime = keys.issue(t, auth.Runtime)
	h.admin = keys.issue(t, auth.Admin)
	return h
}

type reply struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r reply) envelope(t *testing.T) (code, requestID string, details map[string]any, retryMs *int64) {
	t.Helper()
	var e struct {
		Error struct {
			Code         string         `json:"code"`
			RequestID    string         `json:"request_id"`
			RetryAfterMs *int64         `json:"retry_after_ms"`
			Details      map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.Body, &e); err != nil {
		t.Fatalf("not an envelope (%d): %s", r.Status, r.Body)
	}
	return e.Error.Code, e.Error.RequestID, e.Error.Details, e.Error.RetryAfterMs
}

func (h *harness) do(method, path, token, body string, headers ...string) reply {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
			continue
		}
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return reply{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()}
}

const meteredBody = `{"feature_key":"llm.tokens.output","amount":100,"tenant_external_id":"acme-prod-01"}`

func (h *harness) consume(body string, headers ...string) reply {
	return h.do(http.MethodPost, "/v1/consume", h.runtime, body, append([]string{"Idempotency-Key", "01JCQ9W3K2M9XQ3F8V6T0B5NDE"}, headers...)...)
}

func (h *harness) logLines() []map[string]any {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			h.t.Fatalf("a log line is not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}
