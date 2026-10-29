package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/observability"
)

// TestRouterServesTheContractsRoutes: the router serves the operational
// routes, the four data-plane routes and every admin route of the contract,
// and nothing else. An unknown path is a bare 404 and a wrong method a bare
// 405 with Allow, neither asking for a credential. /metrics is served on its
// own listener (IP-13 D8), so this one answers it 404.
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

// TestTheAPIListenerDoesNotServeMetrics (IP-13 D8): /metrics is on its own
// listener, so the API router answers it 404 and the metrics router answers
// nothing else.
func TestTheAPIListenerDoesNotServeMetrics(t *testing.T) {
	served := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("series")) })
	api := NewRouter(RouterOptions{Ready: func() bool { return true }})
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /metrics on the API router = %d, want 404", rec.Code)
	}
	metrics := NewMetricsRouter(RouterOptions{}, served)
	for path, want := range map[string]int{"/metrics": 200, "/healthz": 404, "/v1/consume": 404} {
		rec := httptest.NewRecorder()
		metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s on the metrics router = %d, want %d", path, rec.Code, want)
		}
		if rec.Header().Get("X-Request-Id") == "" {
			t.Errorf("GET %s on the metrics router carries no X-Request-Id", path)
		}
	}
}

// TestAnUnknownMethodIsNotALabel (IP-13 DoD 3): a method no contract route
// uses is counted as other, so a caller cannot mint a series by inventing
// methods.
func TestAnUnknownMethodIsNotALabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := observability.NewHTTP()
	if err := h.Register(reg); err != nil {
		t.Fatal(err)
	}
	r := NewRouter(RouterOptions{Ready: func() bool { return true }, HTTP: h})
	for _, m := range []string{"FOO", "BREW", "GET"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(m, "/healthz", nil))
	}
	if got := testutil.ToFloat64(h.Requests.WithLabelValues("unmatched", observability.MethodOther, "405")); got != 2 {
		t.Errorf("requests with an invented method counted as other = %v, want 2", got)
	}
	if got := testutil.ToFloat64(h.Requests.WithLabelValues("GET /healthz", "GET", "200")); got != 1 {
		t.Errorf("GET /healthz counted %v times, want 1", got)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "method" && l.GetValue() != "GET" && l.GetValue() != observability.MethodOther {
					t.Errorf("%s has method=%q", f.GetName(), l.GetValue())
				}
			}
		}
	}
}
