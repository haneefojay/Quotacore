package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/quotacore/quotacore/internal/envelope"
)

// TestRouterServesTheContractsRoutes: the router serves the operational
// routes, the four data-plane routes and every admin route of the contract,
// and nothing else. An unknown path is a bare 404 and a wrong method a bare
// 405 with Allow, neither asking for a credential. /metrics stays absent
// until IP-13.
func TestRouterServesTheContractsRoutes(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		method, path, token string
		want                int
	}{
		{http.MethodGet, "/healthz", "", http.StatusOK},
		{http.MethodGet, "/readyz", "", http.StatusOK},
		{http.MethodGet, "/openapi.json", "", http.StatusOK},
		{http.MethodGet, "/docs", "", http.StatusOK},
		{http.MethodGet, "/readyz/", "", http.StatusNotFound},
		{http.MethodGet, "/", "", http.StatusNotFound},
		{http.MethodGet, "/metrics", "", http.StatusNotFound},
		{http.MethodGet, "/v1/nothing", "", http.StatusNotFound},
		{http.MethodPost, "/healthz", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/consume", "", http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/balance", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/balance", "", http.StatusUnauthorized},
		{http.MethodPost, "/v1/consume", "", http.StatusUnauthorized},
		{http.MethodPost, "/v1/admin/tenants", "", http.StatusUnauthorized},
		{http.MethodGet, "/v1/admin/status", "", http.StatusUnauthorized},
		// An admin key passes the gate on a route whose handler is IP-09's.
		{http.MethodGet, "/v1/admin/tenants", h.admin, http.StatusNotFound},
	}
	for _, tc := range cases {
		r := h.do(tc.method, tc.path, tc.token, "")
		if r.Status != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, r.Status, tc.want)
		}
		if r.Status == http.StatusNotFound || r.Status == http.StatusMethodNotAllowed {
			if len(r.Body) != 0 {
				t.Errorf("%s %s: a %d carries a body %q", tc.method, tc.path, r.Status, r.Body)
			}
		}
		if r.Status == http.StatusMethodNotAllowed && r.Header.Get("Allow") == "" {
			t.Errorf("%s %s: 405 without Allow", tc.method, tc.path)
		}
	}
	if allow := h.do(http.MethodGet, "/v1/consume", "", "").Header.Values("Allow"); !strings.Contains(strings.Join(allow, ","), "POST") {
		t.Errorf("GET /v1/consume Allow = %v, want POST", allow)
	}

	// Every contract route is registered: none is a 404 for want of a route
	// (an admin route answers 401 without a credential, a probe answers).
	for _, op := range envelope.MustOperations() {
		if op.ID == "metrics" {
			continue
		}
		path := strings.NewReplacer("{feature_key}", "llm.tokens", "{id}", "x").Replace(op.Path)
		if r := h.do(op.Method, path, "", ""); r.Status == http.StatusNotFound || r.Status == http.StatusMethodNotAllowed {
			t.Errorf("%s is not routed: %d", op.Endpoint(), r.Status)
		}
	}
}

// TestRouterWithoutTheDataPlaneAnswersAsBefore: a router built with only
// probes, as the readiness tests build it, answers a /v1 route with a bare
// 404, which is what a build before IP-07 answered.
func TestRouterWithoutTheDataPlaneAnswersAsBefore(t *testing.T) {
	h := NewRouter(RouterOptions{Ready: func() bool { return false }})
	for _, path := range []string{"/v1/balance", "/v1/admin/status"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz = %d, want 503", rec.Code)
	}
}

func TestRouterProbesCarryNoBody(t *testing.T) {
	liveness, readiness := Health(func() bool { return true })
	rec := httptest.NewRecorder()
	liveness.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Body.Len() != 0 {
		t.Errorf("liveness body = %q, want empty", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	readiness.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Body.Len() != 0 {
		t.Errorf("readiness body = %q, want empty", rec.Body.String())
	}
}
