package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLivenessIsAlwaysOKAndCarriesNoBody(t *testing.T) {
	liveness, _ := Health(func() bool { return false })

	w := httptest.NewRecorder()
	liveness.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("liveness status = %d, want %d", w.Code, http.StatusOK)
	}
	body, err := io.ReadAll(w.Result().Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("liveness body = %q, want empty", body)
	}
}

func TestReadinessIsServiceUnavailableWhenNotReady(t *testing.T) {
	cases := map[string]Readiness{
		"reports not ready":   func() bool { return false },
		"no readiness source": nil,
	}
	for name, ready := range cases {
		t.Run(name, func(t *testing.T) {
			_, readiness := Health(ready)

			w := httptest.NewRecorder()
			readiness.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("readiness status = %d, want %d", w.Code, http.StatusServiceUnavailable)
			}
			if body, _ := io.ReadAll(w.Result().Body); len(body) != 0 {
				t.Fatalf("readiness body = %q, want empty", body)
			}
		})
	}
}

func TestReadinessIsOKWhenReady(t *testing.T) {
	_, readiness := Health(func() bool { return true })

	w := httptest.NewRecorder()
	readiness.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("readiness status = %d, want %d", w.Code, http.StatusOK)
	}
}
