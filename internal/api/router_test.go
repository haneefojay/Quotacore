package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouterServesOnlyTheTwoHealthProbes(t *testing.T) {
	readyz := http.StatusServiceUnavailable
	cases := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/readyz", readyz},
		{http.MethodGet, "/readyz/", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/v1/balance", http.StatusNotFound},
		{http.MethodGet, "/v1/admin/status", http.StatusNotFound},
		{http.MethodGet, "/metrics", http.StatusNotFound},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodPost, "/readyz", http.StatusMethodNotAllowed},
	}
	for _, ready := range []bool{false, true} {
		want := readyz
		if ready {
			want = http.StatusOK
		}
		h := NewRouter(RouterOptions{Ready: func() bool { return ready }})
		for _, tc := range cases {
			if tc.method == http.MethodGet && tc.path == "/readyz" {
				tc.want = want
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.want {
				t.Errorf("ready=%v %s %s = %d, want %d", ready, tc.method, tc.path, rec.Code, tc.want)
			}
		}
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
