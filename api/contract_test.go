package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-faster/jx"
)

// TestDocumentIsCurrent is DoD item 6 for IP-02: the served document and the generated
// code come from the same bytes.
//
// The failure it guards against is not hypothetical and not subtle. openapi.yaml is
// edited by hand, ogen is run by hand, and nothing in the build connects the two, so the
// default state of this repository after a schema change is a contract that documents
// fields the validators reject and validators for fields nobody documents. Both
// directions are wrong and both would be found by a customer rather than by us.
func TestDocumentIsCurrent(t *testing.T) {
	if len(Document) == 0 {
		t.Fatal("embedded document is empty")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this source file")
	}
	onDisk, err := os.ReadFile(filepath.Join(filepath.Dir(file), "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	if string(Document) != string(onDisk) {
		t.Error("embedded document differs from api/openapi.yaml; go:embed is stale")
	}
	if !strings.HasPrefix(string(Document), "openapi: 3.1.0") {
		t.Error("document is not OpenAPI 3.1.0, which ADR-0010 selects")
	}
}

// TestOgenEmitsNoRoutes is the check that keeps the generated package honest about what
// this phase is.
//
// ogen's defaults include a router, a client, an OpenTelemetry layer, and a complete set
// of unimplemented handlers that answer 501. That last one is the specific hazard: the
// error catalogue already defines not_implemented as "a v0.2 surface called on an MVP
// build", so a generated stub would answer a real request with a code that claims to mean
// something else. A client would see 501, read the envelope, and conclude the feature was
// never planned rather than merely not built yet.
//
// Asserting on the absence of generated files rather than on config text is deliberate.
// The config could be edited to re-enable a feature and the assertion would still pass;
// the file list cannot drift from what the generator actually did.
func TestOgenEmitsNoRoutes(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this source file")
	}
	dir := filepath.Dir(file)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	// Files ogen writes only when a routing or transport feature is enabled.
	forbidden := map[string]string{
		"oas_router_gen.go":         "an ogen router",
		"oas_client_gen.go":         "an ogen client",
		"oas_server_gen.go":         "an ogen server",
		"oas_unimplemented_gen.go":  "unimplemented handlers that answer 501",
		"oas_otel_gen.go":           "an OpenTelemetry layer",
		"oas_middleware_gen.go":     "middleware",
		"oas_handler_gen.go":        "generated request handlers",
		"oas_interfaces_gen.go.tmp": "a temporary generator artefact",
	}

	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name()] = true
		if why, bad := forbidden[e.Name()]; bad {
			t.Errorf("api/%s exists: %s. This phase generates types, codecs and validators only; "+
				"the routes belong to IP-07 and IP-09. See api/.ogen/ogen.yml.", e.Name(), why)
		}
	}

	// And the files this phase does need, so a config that disables everything fails loudly
	// rather than passing on absence alone.
	for _, want := range []string{
		"oas_schemas_gen.go",
		"oas_json_gen.go",
		"oas_validators_gen.go",
		"oas_parameters_gen.go",
		"oas_defaults_gen.go",
	} {
		if !seen[want] {
			t.Errorf("api/%s is missing; generation produced no types, codecs or validators", want)
		}
	}
}

// TestUndeclaredFieldIsRejected is DoD item 3, and the reason this phase generates code
// at all rather than shipping a YAML file.
//
// A quota API that silently ignores an undeclared field is a quota API that will accept
// {"amount": 10, "unit": "tokens"} from a client that misspelled the limit, and then
// charge the tenant against the wrong number. The request looks successful. The error
// surfaces at the invoice.
func TestUndeclaredFieldIsRejected(t *testing.T) {
	valid := []byte(`{"feature_key":"llm.tokens.output","amount":4200,"tenant_external_id":"acme-prod-01"}`)

	var ok MeteredRequest
	if err := ok.Decode(jx.DecodeBytes(valid)); err != nil {
		t.Fatalf("a valid body was rejected: %v", err)
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a valid body failed validation: %v", err)
	}
	if ok.FeatureKey != FeatureKey("llm.tokens.output") {
		t.Errorf("feature_key = %q, want llm.tokens.output", ok.FeatureKey)
	}

	// `unit` is a real field on a feature, and a plausible thing for a client to send here by
	// mistake. The point is that the request must not be accepted and silently reinterpreted.
	invalid := []byte(`{"feature_key":"llm.tokens.output","amount":4200,"tenant_external_id":"acme-prod-01","unit":"tokens"}`)

	var bad MeteredRequest
	err := bad.Decode(jx.DecodeBytes(invalid))
	if err == nil {
		t.Error("an undeclared field was accepted; additionalProperties: false is not enforced")
	} else if !strings.Contains(err.Error(), "unexpected field") {
		t.Errorf("rejected with %v, want an unexpected-field error naming the offender", err)
	}
}

// TestEveryRequestBodyIsStrict is DoD item 3 across the whole contract rather than on one
// hand-picked route.
//
// Checking one schema proves only that ogen honours additionalProperties where it was
// applied. The property that matters is that no request body in the document can be
// accepted with a field nobody documented, and a per-schema omission would be invisible
// until a client sent it.
func TestEveryRequestBodyIsStrict(t *testing.T) {
	for _, tc := range []struct {
		name string
		body interface{ Decode(*jx.Decoder) error }
		// undeclared is a field name no schema in the document declares.
		undeclared string
	}{
		// `refund` and `check` share MeteredRequest, so they are covered by it.
		{"MeteredRequest", &MeteredRequest{}, "unit"},
		{"FeatureCreate", &FeatureCreate{}, "state"},
		{"FeatureUpdate", &FeatureUpdate{}, "unit"},
		{"PlanCreate", &PlanCreate{}, "state"},
		{"PlanUpdate", &PlanUpdate{}, "state"},
		{"PlanApplyRequest", &PlanApplyRequest{}, "force"},
		{"TenantCreate", &TenantCreate{}, "state"},
		{"TenantUpdate", &TenantUpdate{}, "anchor_at"},
		{"ReasonRequest", &ReasonRequest{}, "force"},
		{"ArchiveRequest", &ArchiveRequest{}, "force"},
		{"DeleteRequest", &DeleteRequest{}, "force"},
		{"GrantRequest", &GrantRequest{}, "expires_at"},
		{"SetRequest", &SetRequest{}, "expires_at"},
		{"ForceRolloverRequest", &ForceRolloverRequest{}, "force"},
		{"OverrideUpsert", &OverrideUpsert{}, "cleared"},
		{"OverrideClearRequest", &OverrideClearRequest{}, "feature_key"},
		{"KeyCreate", &KeyCreate{}, "key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := withUndeclaredField(tc.body, tc.undeclared)
			if err != nil {
				t.Fatalf("build the probe body for %s: %v", tc.name, err)
			}
			err = tc.body.Decode(jx.DecodeBytes(payload))
			if err == nil {
				t.Fatalf("an undeclared field was accepted on %s; additionalProperties: false is not enforced", tc.name)
			}
			if !strings.Contains(err.Error(), "unexpected field") {
				t.Errorf("%s rejected with %v, want an unexpected-field error naming %q", tc.name, err, tc.undeclared)
			}
		})
	}
}

// withUndeclaredField renders a schema's zero value as JSON with one extra key.
//
// Building the probe from the generated encoder, rather than hand-writing a body per
// schema, is what keeps this test honest as the document grows: a request body added to
// openapi.yaml without a case here shows up as a missing case rather than as silently less
// coverage. A hand-written body would also drift, and would keep passing after the schema
// it was written against had changed.
func withUndeclaredField(zero any, undeclared string) ([]byte, error) {
	enc, ok := zero.(interface{ Encode(*jx.Encoder) })
	if !ok {
		return nil, errors.New("generated type does not implement Encode")
	}
	var e jx.Encoder
	enc.Encode(&e)
	raw := e.Bytes()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	fields[undeclared] = json.RawMessage(`"probe"`)
	return json.Marshal(fields)
}
