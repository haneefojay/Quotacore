package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/quotacore/quotacore/internal/store"
)

// The admin tests drive IP-09's routes through the assembled stack, the same
// wiring run serves. Resources are created through the API wherever a route
// exists, so what is proved is the route and not a seed.

// adminPost sends a JSON body with the stack's admin key.
func (s *stack) adminDo(method, path, body string) response {
	s.t.Helper()
	return s.do(method, path, s.admin, body)
}

// mustStatus fails the test unless the response has the status.
func mustStatus(t *testing.T, r response, want int) map[string]any {
	t.Helper()
	if r.Status != want {
		t.Fatalf("got %d, want %d: %s", r.Status, want, r.Body)
	}
	if len(r.Body) == 0 {
		return nil
	}
	return r.json(t)
}

func num(t *testing.T, m map[string]any, field string) int64 {
	t.Helper()
	f, ok := m[field].(float64)
	if !ok {
		t.Fatalf("%s is not a number in %v", field, m)
	}
	return int64(f)
}

func str(m map[string]any, field string) string {
	s, _ := m[field].(string)
	return s
}

// createFeature creates a metered feature through the API.
func (s *stack) createFeature(key string) map[string]any {
	s.t.Helper()
	return mustStatus(s.t, s.adminDo(http.MethodPost, "/v1/admin/features",
		`{"feature_key":"`+key+`","name":"F","unit":"token"}`), http.StatusCreated)
}

// createBooleanFeature creates a boolean feature through the admin API.
func (s *stack) createBooleanFeature(key string) map[string]any {
	s.t.Helper()
	return mustStatus(s.t, s.adminDo(http.MethodPost, "/v1/admin/features",
		`{"feature_key":"`+key+`","name":"F","unit":"flag","kind":"boolean"}`), http.StatusCreated)
}

// createPlan creates a plan with one entitlement per feature at limit.
func (s *stack) createPlan(key string, limit int64, interval string, features ...string) map[string]any {
	s.t.Helper()
	var ents []string
	for _, f := range features {
		ents = append(ents, fmt.Sprintf(`{"feature_key":%q,"limit":%d,"reset_interval":%q}`, f, limit, interval))
	}
	return mustStatus(s.t, s.adminDo(http.MethodPost, "/v1/admin/plans",
		`{"plan_key":"`+key+`","name":"P","entitlements":[`+strings.Join(ents, ",")+`]}`), http.StatusCreated)
}

func (s *stack) createTenant(external, plan, zone string) map[string]any {
	s.t.Helper()
	m := mustStatus(s.t, s.adminDo(http.MethodPost, "/v1/admin/tenants",
		`{"external_id":"`+external+`","display_name":"T","plan_key":"`+plan+`","timezone":"`+zone+`"}`), http.StatusCreated)
	s.t.Cleanup(func() { s.rdb.HDel(context.Background(), store.ConfigTenantsKey(), str(m, "id")) })
	return m
}

func uniq(prefix string) string { return strings.ToLower(externalID(prefix)) }

// TestTheAdminSurfaceProvisionsWhatTheDataPlaneEnforces walks the resource
// surface end to end: a feature, a plan and a tenant created over the API are
// enforced on the data plane at once, with the six-field hash the create
// wrote, and every list pages with a cursor.
func TestTheAdminSurfaceProvisionsWhatTheDataPlaneEnforces(t *testing.T) {
	s := newStack(t)
	f := uniq("tok")
	s.createFeature(f)
	plan := uniq("starter")
	p := s.createPlan(plan, 100, "monthly", f)
	if ents, _ := p["entitlements"].([]any); len(ents) != 1 {
		t.Fatalf("plan entitlements = %v", p["entitlements"])
	}
	ext := externalID("acme")
	tn := s.createTenant(ext, plan, "Europe/Berlin")
	if str(tn, "state") != "active" || num(t, tn, "version") != 1 {
		t.Fatalf("tenant = %v", tn)
	}
	hash, err := s.rdb.HGetAll(context.Background(), "qc:{t:"+str(tn, "id")+"}:bal:"+f).Result()
	if err != nil || len(hash) != 6 || hash["balance"] != "100" || hash["limit"] != "100" || hash["cycle_index"] != "0" {
		t.Fatalf("the opening hash = %v, %v", hash, err)
	}
	if r := s.consume(s.runtime, f, 30, ext, newKey()); r.Status != http.StatusOK {
		t.Fatalf("consume on a tenant created over the API: %d %s", r.Status, r.Body)
	}

	// A second of each, so every list has two pages at limit=1.
	f2 := uniq("tok")
	s.createFeature(f2)
	plan2 := uniq("starter")
	s.createPlan(plan2, 10, "never", f2)
	s.createTenant(externalID("acme"), plan2, "UTC")
	for _, path := range []string{"/v1/admin/features", "/v1/admin/plans", "/v1/admin/tenants", "/v1/admin/keys"} {
		r := s.adminDo(http.MethodGet, path+"?limit=1", "")
		m := mustStatus(t, r, http.StatusOK)
		next, _ := m["next_cursor"].(string)
		if next == "" {
			t.Fatalf("%s?limit=1 has no next cursor: %s", path, r.Body)
		}
		r2 := s.adminDo(http.MethodGet, path+"?limit=1&cursor="+next, "")
		mustStatus(t, r2, http.StatusOK)
		if string(r2.Body) == string(r.Body) {
			t.Errorf("%s: the second page repeats the first", path)
		}
		bad := s.adminDo(http.MethodGet, path+"?cursor="+next[:len(next)-2]+"xx", "")
		if bad.code(t) != "validation_failed" {
			t.Errorf("%s with a tampered cursor: %d %s", path, bad.Status, bad.Body)
		}
	}
	audit := mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/audit?tenant_external_id="+ext, ""), http.StatusOK)
	entries, _ := audit["entries"].([]any)
	if len(entries) != 1 || str(entries[0].(map[string]any), "action") != "tenant.create" {
		t.Fatalf("the tenant's audit = %s", mustJSON(audit))
	}
	st := mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/status", ""), http.StatusOK)
	if str(st, "status") != "ok" || str(st, "worker") != "running" {
		t.Errorf("status = %s", mustJSON(st))
	}
}

func newTestLogger(buf *syncBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
