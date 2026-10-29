package api

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The key scope a route requires is a rule, not a decoration: `runtime` on the data plane, `admin`
// on `/v1/admin/*`, and none at all on the operational routes. It is stated once, in
// api-conventions.md (DR-043, NFR-O5). These tests exist because the contract had no way to state
// it that a validator would accept, and the wrong way round is worse than no way.

// TestSecurityRequirementsCarryNoScopes asserts the requirement arrays are empty.
//
// OpenAPI says a Security Requirement Object's value is a list of scope names only for `oauth2`
// and `openIdConnect`, and that for every other scheme type the array must be empty. `bearerAuth`
// is `type: http`, so `bearerAuth: [admin]` is not a scope declaration, it is a malformed document
// that a strict linter rejects and a lenient generator silently ignores — which is how a rule
// about authorisation ends up present in prose and absent from the artefact clients read.
func TestSecurityRequirementsCarryNoScopes(t *testing.T) {
	t.Parallel()

	m := parseDocModel(t, Document)
	assertEmptyScopes(t, "the document-level security", &m.Security)

	for _, op := range readOperations(&m.Paths, newDocRefs(&m), &m.Security) {
		name := strings.ToUpper(op.Method) + " " + op.Path
		for _, scheme := range declaredSchemes(t, &m, op.Method, op.Path) {
			for _, scope := range scheme.scopes {
				t.Errorf("%s requires %s with the scope %q; an HTTP bearer scheme has no scopes, "+
					"so the scope belongs in x-required-scope (see the scheme description)",
					name, scheme.name, scope)
			}
		}
	}
}

// TestEveryOperationDeclaresItsRequiredScope asserts the extension is present wherever a
// credential is required, and absent where none is. An operation with a credential requirement and
// no recorded scope is an operation nobody has checked.
func TestEveryOperationDeclaresItsRequiredScope(t *testing.T) {
	t.Parallel()

	m := parseDocModel(t, Document)
	for _, op := range readOperations(&m.Paths, newDocRefs(&m), &m.Security) {
		name := strings.ToUpper(op.Method) + " " + op.Path
		want := "runtime"
		switch {
		case len(op.Security) == 0:
			want = ""
		case strings.HasPrefix(op.Path, "/v1/admin/"):
			want = "admin"
		}
		if op.RequiredScope != want {
			if want == "" {
				t.Errorf("%s takes no credential, so it must declare no x-required-scope, but declares %q",
					name, op.RequiredScope)
				continue
			}
			t.Errorf("%s declares x-required-scope %q, expected %q", name, op.RequiredScope, want)
		}
	}
}

// TestOperationalRoutesTakeNoCredential pins the five routes that are reachable without a key. A
// key requirement added to a probe is a deployment that restarts healthy processes, and a probe
// left reachable is a page of the contract that does not describe the service.
func TestOperationalRoutesTakeNoCredential(t *testing.T) {
	t.Parallel()

	m := parseDocModel(t, Document)
	open := map[string]bool{}
	for _, op := range readOperations(&m.Paths, newDocRefs(&m), &m.Security) {
		if len(op.Security) == 0 {
			open[op.Path] = true
		}
	}
	for _, path := range []string{"/healthz", "/readyz", "/metrics", "/openapi.json", "/docs"} {
		if !open[path] {
			t.Errorf("%s requires a credential; it is an operational route (NFR-S9)", path)
		}
		delete(open, path)
	}
	for path := range open {
		t.Errorf("%s requires no credential but is not an operational route", path)
	}
}

// docScheme is one entry of a security requirement, with the scope array it carried.
type docScheme struct {
	name   string
	scopes []string
}

// declaredSchemes re-reads one operation's `security` value from the raw node, because
// readOperations reduces it to names and the test is about what the document actually says.
func declaredSchemes(t *testing.T, m *docModel, method, path string) []docScheme {
	t.Helper()
	for i := 0; i+1 < len(m.Paths.Content); i += 2 {
		if m.Paths.Content[i].Value != path {
			continue
		}
		item := m.Paths.Content[i+1]
		for j := 0; j+1 < len(item.Content); j += 2 {
			if !strings.EqualFold(item.Content[j].Value, method) {
				continue
			}
			var out []docScheme
			for k := 0; k+1 < len(item.Content[j+1].Content); k += 2 {
				if item.Content[j+1].Content[k].Value != "security" {
					continue
				}
				for _, entry := range item.Content[j+1].Content[k+1].Content {
					for e := 0; e+1 < len(entry.Content); e += 2 {
						scheme := docScheme{name: entry.Content[e].Value}
						for _, scope := range entry.Content[e+1].Content {
							scheme.scopes = append(scheme.scopes, scope.Value)
						}
						out = append(out, scheme)
					}
				}
			}
			return out
		}
	}
	t.Fatalf("the contract has no %s %s, so the test above would pass vacuously",
		strings.ToUpper(method), path)
	return nil
}

func assertEmptyScopes(t *testing.T, where string, node *yaml.Node) {
	t.Helper()
	if node.Kind == 0 {
		t.Errorf("%s is missing entirely, so every operation inherits nothing and is unauthenticated",
			where)
		return
	}
	for _, entry := range node.Content {
		for i := 0; i+1 < len(entry.Content); i += 2 {
			if len(entry.Content[i+1].Content) > 0 {
				t.Errorf("%s requires %s with %d scope name(s); an HTTP bearer scheme has no scopes",
					where, entry.Content[i].Value, len(entry.Content[i+1].Content))
			}
		}
	}
}
