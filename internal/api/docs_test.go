package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	contract "github.com/quotacore/quotacore/api"
)

// These tests are about the two documentation routes only. The probes are covered in
// router_test.go, and the product routes are deliberately absent, which router_test.go also
// asserts.

// TestRouterServesTheOperationalRoutes is the table that says what the current phase serves.
func TestRouterServesTheOperationalRoutes(t *testing.T) {
	h := NewRouter(RouterOptions{Ready: func() bool { return true }})
	cases := []struct {
		method      string
		path        string
		want        int
		contentType string
	}{
		{http.MethodGet, "/openapi.json", http.StatusOK, "application/json"},
		{http.MethodGet, "/docs", http.StatusOK, "text/html"},
		{http.MethodPost, "/openapi.json", http.StatusMethodNotAllowed, ""},
		{http.MethodPost, "/docs", http.StatusMethodNotAllowed, ""},
		// The contract declares GET for both routes and nothing else, so HEAD is a 405 rather
		// than a silent alias. A head request that worked but was not in the contract would be a
		// route the document does not describe.
		{http.MethodHead, "/docs", http.StatusMethodNotAllowed, ""},
		{http.MethodGet, "/docs/", http.StatusNotFound, ""},
		{http.MethodGet, "/openapi.yaml", http.StatusNotFound, ""},
		{http.MethodGet, "/api/openapi.json", http.StatusNotFound, ""},
		{http.MethodGet, "/swagger.json", http.StatusNotFound, ""},
		{http.MethodGet, "/v1/consume", http.StatusNotFound, ""},
		{http.MethodGet, "/v1/check", http.StatusNotFound, ""},
		{http.MethodGet, "/v1/refund", http.StatusNotFound, ""},
		{http.MethodGet, "/v1/balance", http.StatusNotFound, ""},
		{http.MethodGet, "/v1/admin/tenants", http.StatusNotFound, ""},
		{http.MethodGet, "/v1/admin/keys", http.StatusNotFound, ""},
		{http.MethodGet, "/v1/admin/audit", http.StatusNotFound, ""},
		{http.MethodGet, "/v1/admin/status", http.StatusNotFound, ""},
		{http.MethodDelete, "/v1/admin/keys/qk_1", http.StatusNotFound, ""},
		{http.MethodPost, "/v1/admin/tenants/tn_1/overrides", http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
			continue
		}
		if tc.contentType == "" {
			continue
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.contentType) {
			t.Errorf("%s %s Content-Type = %q, want it to start with %q",
				tc.method, tc.path, got, tc.contentType)
		}
	}
}

// TestServedContractIsTheContractTheBinaryWasBuiltFrom is the DoD item: the document a client
// fetches must be the one in this binary. It compares the served bytes against the contract, and
// checks the served document parses and names every path the contract declares, so a conversion
// that silently dropped the paths tree would fail rather than serve a smaller document with a
// correct content type.
func TestServedContractIsTheContractTheBinaryWasBuiltFrom(t *testing.T) {
	h := NewRouter(RouterOptions{Ready: func() bool { return true }})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d, want 200", rec.Code)
	}

	var served struct {
		OpenAPI string                     `json:"openapi"`
		Paths   map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &served); err != nil {
		t.Fatalf("the served document is not a JSON object: %v", err)
	}
	if !strings.HasPrefix(served.OpenAPI, "3.1") {
		t.Errorf("openapi = %q, want a 3.1 version", served.OpenAPI)
	}
	for _, path := range []string{
		"/v1/consume", "/v1/refund", "/v1/check", "/v1/balance",
		"/v1/admin/features", "/v1/admin/plans", "/v1/admin/tenants",
		"/v1/admin/keys", "/v1/admin/audit", "/v1/admin/status",
		"/healthz", "/readyz", "/metrics", "/openapi.json", "/docs",
	} {
		if _, ok := served.Paths[path]; !ok {
			t.Errorf("the served document has no path %s", path)
		}
	}
	if len(served.Paths) < 15 {
		t.Errorf("the served document declares %d paths, which is fewer than the contract's",
			len(served.Paths))
	}
}

// TestServedContractMatchesTheEmbeddedBytes is the identity half of the previous test: the
// embedded YAML and the served JSON are the same document. RenderJSON is the only thing between
// them, and this is the test that says it did not lose anything on the way.
func TestServedContractMatchesTheEmbeddedBytes(t *testing.T) {
	h := NewRouter(RouterOptions{Ready: func() bool { return true }})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	rendered, err := contract.RenderJSON(contract.Document)
	if err != nil {
		t.Fatalf("render the contract: %v", err)
	}
	if rec.Body.String() != string(rendered) {
		t.Error("the body served is not the JSON rendering of the embedded contract")
	}
}

// TestServedDocsIsTheCommittedPage checks the same identity for the reference page.
func TestServedDocsIsTheCommittedPage(t *testing.T) {
	h := NewRouter(RouterOptions{Ready: func() bool { return true }})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if rec.Body.String() != string(contract.DocsPage) {
		t.Error("the page served is not the committed api/docs.html")
	}
	if !strings.HasPrefix(rec.Body.String(), "<!DOCTYPE html>") {
		t.Error("the served page is not an HTML document")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store so a stale reference cannot outlive the binary", got)
	}
}

// TestDocumentationRoutesNeedNoCredential is the property that makes them usable when something
// is broken. A reference you must authenticate to read is no use at 02:00 with a bad key.
func TestDocumentationRoutesNeedNoCredential(t *testing.T) {
	h := NewRouter(RouterOptions{Ready: func() bool { return true }})
	for _, path := range []string{"/openapi.json", "/docs"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("GET %s = %d; the documentation routes are declared as taking no credential",
				path, rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "" {
			t.Errorf("GET %s sent WWW-Authenticate: %q, which advertises a credential it does not check",
				path, got)
		}
	}
}

// TestDocumentationRoutesIgnoreQueryStrings keeps a cache-buster or a stray parameter from turning
// a documentation route into a 404, which is a bad first impression of the API.
func TestDocumentationRoutesIgnoreQueryStrings(t *testing.T) {
	h := NewRouter(RouterOptions{Ready: func() bool { return true }})
	for _, target := range []string{"/docs?pretty=1", "/openapi.json?v=2"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", target, rec.Code)
		}
	}
}
