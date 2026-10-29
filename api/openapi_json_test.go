package api

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// The contract is YAML on disk and JSON on the wire. Everything in this file is about the gap
// between those two statements, because that gap is where a contract quietly loses a field.

func TestDocumentJSONIsValidJSON(t *testing.T) {
	t.Parallel()

	document, err := DocumentJSON()
	if err != nil {
		t.Fatalf("render the contract as JSON: %v", err)
	}
	if !json.Valid(document) {
		t.Fatal("the served document is not parseable JSON")
	}
	var parsed any
	if err := json.Unmarshal(document, &parsed); err != nil {
		t.Fatalf("json.Valid and json.Unmarshal disagree about the same bytes: %v", err)
	}
}

// TestServedContractMatchesTheEmbeddedDocument is the test that makes the `/openapi.json`
// description true. It compares by value, not by bytes, because the two representations
// legitimately differ: YAML has a timestamp scalar where JSON has only a string, and key order is
// not part of either format's meaning. A byte comparison would be satisfied by two documents that
// mean different things, and would fail on a difference that means nothing.
func TestServedContractMatchesTheEmbeddedDocument(t *testing.T) {
	t.Parallel()

	served, err := DocumentJSON()
	if err != nil {
		t.Fatalf("render the contract as JSON: %v", err)
	}
	var fromJSON any
	if err := json.Unmarshal(served, &fromJSON); err != nil {
		t.Fatalf("parse the served document: %v", err)
	}
	var fromYAML any
	if err := yaml.Unmarshal(Document, &fromYAML); err != nil {
		t.Fatalf("parse the embedded document: %v", err)
	}
	if !reflect.DeepEqual(normalise(fromYAML), normalise(fromJSON)) {
		t.Error("the JSON served at /openapi.json is not the same document as the embedded YAML")
	}
}

// TestDocumentJSONPreservesMappingOrder checks the property a Go map would destroy, and the
// reason the conversion walks the node tree instead of decoding into a map and re-encoding.
//
// Order is not a semantic property of JSON, so a reader could argue it does not matter. It
// matters here for two concrete reasons: the contract groups related routes together
// deliberately, and a reordering on every regeneration buries a real edit inside a large
// mechanical one.
func TestDocumentJSONPreservesMappingOrder(t *testing.T) {
	t.Parallel()

	served, err := DocumentJSON()
	if err != nil {
		t.Fatalf("render the contract as JSON: %v", err)
	}
	// Both lists are in this order in the document, so the served bytes must show them in this
	// order too.
	for _, group := range [][]string{
		{"data-plane", "admin-features", "admin-plans"},
		{`"/v1/consume"`, `"/v1/refund"`, `"/v1/check"`, `"/v1/balance"`},
		{`"/v1/admin/features"`, `"/v1/admin/plans"`, `"/v1/admin/tenants"`,
			`"/v1/admin/keys"`, `"/v1/admin/audit"`, `"/v1/admin/status"`},
		{`"/healthz"`, `"/readyz"`, `"/metrics"`, `"/openapi.json"`, `"/docs"`},
	} {
		previous := -1
		for _, member := range group {
			at := bytes.Index(served, []byte(member))
			if at < 0 {
				t.Fatalf("%q is not in the served document", member)
			}
			if at < previous {
				t.Errorf("%q appears before an earlier member of %v; mapping order was not preserved",
					member, group)
			}
			previous = at
		}
	}
}

// TestDocumentJSONIsCached asserts the conversion happens once. A route that re-parsed the whole
// contract per request would be measurable, and it would be measurable only under load, which is
// the worst time to find out.
func TestDocumentJSONIsCached(t *testing.T) {
	t.Parallel()

	first, err := DocumentJSON()
	if err != nil {
		t.Fatalf("render the contract as JSON: %v", err)
	}
	second, err := DocumentJSON()
	if err != nil {
		t.Fatalf("render the contract as JSON again: %v", err)
	}
	if &first[0] != &second[0] {
		t.Error("two calls returned different backing arrays, so the result is not cached")
	}
}

// TestRenderJSONIsIdempotent guards the renderer itself: the same input must produce the same
// bytes every time, or nothing above can be compared to anything.
func TestRenderJSONIsIdempotent(t *testing.T) {
	t.Parallel()

	first, err := RenderJSON(Document)
	if err != nil {
		t.Fatalf("render the contract as JSON: %v", err)
	}
	second, err := RenderJSON(Document)
	if err != nil {
		t.Fatalf("render the contract as JSON again: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("two renderings of the same document differ")
	}
}

// TestDocumentJSONIsCompact checks the served form is not pretty-printed, and bounds how far it
// may grow. There is no strong reason either way, but compact is the smaller response and a
// debugging route is a place to spend bytes carefully. The upper bound is the part that earns its
// keep: it fails if someone reintroduces indentation, which would multiply the size for no gain.
func TestDocumentJSONIsCompact(t *testing.T) {
	t.Parallel()

	document, err := DocumentJSON()
	if err != nil {
		t.Fatalf("render the contract as JSON: %v", err)
	}
	if bytes.Contains(document, []byte("\n  ")) {
		t.Error("the served JSON is indented, so it is not the compact form the handler intends")
	}
	if limit := 2 * len(Document); len(document) > limit {
		t.Errorf("the JSON rendering is %d bytes against %d for the YAML source, over the %d limit",
			len(document), len(Document), limit)
	}
}

// TestDocumentJSONRejectsAnUnrenderableDocument proves the error path is reachable. Without this
// the 500 branch in the handler is code that has never run, and untested error handling is
// usually broken error handling.
func TestDocumentJSONRejectsAnUnrenderableDocument(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"not yaml at all":        "\tthis: [is not",
		"an empty document":      "",
		"whitespace only":        "   \n\n",
		"a scalar at the root":   "just a string",
		"a sequence at the root": "- one\n- two\n",
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := RenderJSON([]byte(document)); err == nil {
				t.Error("rendering succeeded, so the handler's failure branch is unreachable")
			}
		})
	}
}

// TestDocumentJSONDeclaresItsVersion guards the one piece of metadata a client checks before
// anything else.
func TestDocumentJSONDeclaresItsVersion(t *testing.T) {
	t.Parallel()

	document, err := DocumentJSON()
	if err != nil {
		t.Fatalf("render the contract as JSON: %v", err)
	}
	if !bytes.Contains(document, []byte(`"openapi":"3.1`)) {
		t.Error("the served document does not declare OpenAPI 3.1")
	}
	version := regexp.MustCompile(`"version":"([^"]+)"`).FindSubmatch(document)
	if version == nil {
		t.Fatal("the served document has no info.version")
	}
	if string(version[1]) == "" {
		t.Error("info.version is empty")
	}
}

// normalise turns a decoded document into a form that can be compared across the two decoders.
//
// The only difference worth normalising is numeric width and the timestamp type. The JSON decoder
// produces float64 for every number, so an integer limit of 100 arrives as 100.0 on one side and
// as int on the other, and the YAML decoder returns a time.Time where the JSON one returns the
// string it was written as. Comparing either directly would report a difference that means nothing
// and hide one that does.
func normalise(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = normalise(item)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[asString(k)] = normalise(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = normalise(item)
		}
		return out
	case int:
		return float64(v)
	case int32:
		return float64(v)
	case int64:
		return float64(v)
	case uint64:
		return float64(v)
	case float32:
		return float64(v)
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	default:
		return value
	}
}

// asString renders a mapping key. A key that is not a string has no JSON equivalent, so an empty
// string is returned to make the comparison fail and name the difference rather than to hide it.
func asString(key any) string {
	if s, ok := key.(string); ok {
		return s
	}
	return "\x00non-string-key\x00"
}
