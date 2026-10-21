package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func probe(t *testing.T, h http.Handler, path string) (int, string, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	body, err := io.ReadAll(w.Result().Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return w.Code, w.Header().Get("Content-Type"), string(body)
}

// TestTheProbesAnswerTheContract (IP-13 DoD 5): HealthzOK and ReadyzOK are
// JSON bodies and ReadyzNotReady is a bare 503 (api/openapi.yaml).
func TestTheProbesAnswerTheContract(t *testing.T) {
	for _, ready := range []bool{true, false} {
		liveness, _ := Health(func() bool { return ready })
		code, ctype, body := probe(t, liveness, "/healthz")
		if code != http.StatusOK || ctype != "application/json" || body != `{"status":"ok"}` {
			t.Errorf("ready=%v: liveness = %d %q %s, want 200 application/json {\"status\":\"ok\"}", ready, code, ctype, body)
		}
	}

	_, readiness := Health(func() bool { return true })
	code, ctype, body := probe(t, readiness, "/readyz")
	if code != http.StatusOK || ctype != "application/json" || body != `{"status":"ready"}` {
		t.Errorf("ready: readiness = %d %q %s, want 200 application/json {\"status\":\"ready\"}", code, ctype, body)
	}

	for name, ready := range map[string]Readiness{
		"reports not ready":   func() bool { return false },
		"no readiness source": nil,
	} {
		_, readiness := Health(ready)
		code, _, body := probe(t, readiness, "/readyz")
		if code != http.StatusServiceUnavailable || body != "" {
			t.Errorf("%s: readiness = %d %q, want a bare 503", name, code, body)
		}
	}
}
