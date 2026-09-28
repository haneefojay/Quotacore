package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-faster/jx"
	"go.yaml.in/yaml/v3"
)

// The catalogue is the authority on which codes exist and what status each one carries;
// this package is the authority on what the service can actually return. This file exists
// because those two facts are written down in two files, in two formats, by hand, and
// nothing in the build otherwise connects them.
//
// The failure this guards against is specific and has a cost. A code in the catalogue that
// no route returns is documentation a client cannot act on: the client writes a branch for
// it, never sees it, and either keeps the branch forever or removes it and is wrong when
// the route starts returning it. A code in the contract that the catalogue does not define
// is worse, because a client that trusts the contract has been told a code is stable and
// permanent when nobody ever decided that.

// contractDoc is the shape of api/openapi.yaml this file needs. It is deliberately partial:
// reading the whole document into a struct would mean every added extension is a code change
// here, and the checks below are about codes, statuses, headers and examples.
type contractDoc struct {
	Components struct {
		Responses map[string]struct {
			XStatus *int     `yaml:"x-status"`
			XCodes  []string `yaml:"x-codes"`
			Headers map[string]struct {
				Ref string `yaml:"$ref"`
			} `yaml:"headers"`
		} `yaml:"responses"`
		Schemas map[string]struct {
			Examples map[string]example `yaml:"examples"`
		} `yaml:"schemas"`
		Examples map[string]example `yaml:"examples"`
	} `yaml:"components"`
	// Each path maps to its methods, and also to a `parameters` key holding a sequence. The
	// value is a raw node rather than a typed struct because the two shapes differ: a method is
	// a mapping and a `parameters` list is a sequence, and one struct type for both would fail
	// to unmarshal. TestMatrixAndContractAgree decodes only the method keys.
	Paths map[string]map[string]yaml.Node `yaml:"paths"`
}

type example struct {
	Value yaml.Node `yaml:"value"`
}

var (
	// A catalogue row: | `code` | 409 | ...
	catalogueRow = regexp.MustCompile("(?m)^\\| `(?<c>[a-z][a-z0-9_]*)` \\| (?<s>\\d{3}) \\|")
	// A matrix row: | `POST /v1/consume` | `a`, `b` | ...
	matrixRow = regexp.MustCompile("^\\| (?<ep>.+?) \\| (?<codes>.+?) \\|$")
	// A backticked token, which is how both the matrix and the docs name a code.
	backticked = regexp.MustCompile("`([a-z][a-z0-9_]*)`")
	// The matrix abbreviates cells with endpoint names rather than codes.
	endpointShorthand = map[string]bool{
		"consume": true, "refund": true, "check": true, "balance": true,
	}
)

// loadContract parses the embedded document. Every check in this file goes through it, so
// the tests assert against the bytes the binary serves rather than a second copy on disk
// that could be edited without the embed being refreshed.
func loadContract(t *testing.T) contractDoc {
	t.Helper()
	var doc contractDoc
	if err := yaml.Unmarshal(Document, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	if len(doc.Components.Responses) == 0 {
		t.Fatal("no response components parsed; the document structure changed under this check")
	}
	return doc
}

func readCatalogue(t *testing.T) (codes map[string]string, matrix []matrixCell) {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "docs", "product", "error-catalog.md"))
	if err != nil {
		t.Fatalf("resolve the catalogue path: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read error-catalog.md: %v", err)
	}
	text := string(raw)

	// The catalogue table is section 2 and the matrix is section 3. Slicing between the
	// headings rather than over the whole file is what stops a backticked code appearing in
	// the failure scenarios from being mistaken for a catalogue entry.
	tableAt := strings.Index(text, "\n## 2. ")
	matrixAt := strings.Index(text, "\n## 3. ")
	scenariosAt := strings.Index(text, "\n## 4. ")
	if tableAt < 0 || matrixAt < 0 || scenariosAt < 0 || !(tableAt < matrixAt && matrixAt < scenariosAt) {
		t.Fatal("error-catalog.md no longer has the numbered sections this test parses (2, 3 and 4)")
	}

	codes = map[string]string{}
	for _, m := range catalogueRow.FindAllStringSubmatch(text[tableAt:matrixAt], -1) {
		codes[m[1]] = m[2]
	}
	if len(codes) == 0 {
		t.Fatal("no catalogue rows parsed from section 2")
	}

	for _, line := range strings.Split(text[matrixAt:scenariosAt], "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		mm := matrixRow.FindStringSubmatch(line)
		if mm == nil {
			continue
		}
		// Skip the table's own header and separator rows. The separator is all dashes and the
		// header names the columns, and neither is a route.
		if strings.HasPrefix(mm[1], "---") || mm[1] == "Endpoint" {
			continue
		}
		cell := matrixCell{routes: splitRoutes(mm[1]), codes: map[string]bool{}}
		for _, tok := range backticked.FindAllStringSubmatch(mm[2], -1) {
			if !endpointShorthand[tok[1]] {
				cell.codes[tok[1]] = true
			}
		}
		matrix = append(matrix, cell)
	}
	if len(matrix) == 0 {
		t.Fatal("no endpoint matrix rows parsed from section 3")
	}
	return codes, matrix
}

type matrixCell struct {
	routes []string
	codes  map[string]bool
}

// splitRoutes turns one matrix cell's endpoint column into the individual routes it names.
func splitRoutes(cell string) []string {
	var out []string
	for _, part := range strings.Split(cell, ",") {
		if r := strings.Trim(strings.TrimSpace(part), "`"); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// TestContractAndCatalogueAgree is DoD item 1 in both directions.
//
// It is a test rather than only a checker because the checker parses Markdown, and Markdown
// is not a format that can be asked "which statuses are declared for these codes" without
// reimplementing enough of a table parser to get it subtly wrong on a cell that has been
// reflowed. Here the two are structured data and the comparison is exact.
func TestContractAndCatalogueAgree(t *testing.T) {
	doc := loadContract(t)
	catalogue, _ := readCatalogue(t)

	// The contract's own statement of the code set is the ErrorCode enum, which is what a
	// client generator reads. It is read separately below, by re-parsing, because an enum is a
	// list of scalars and a struct that also carries the response components cannot hold both
	// shapes. Its presence is asserted here so a missing enum is reported once, clearly.
	if _, ok := doc.Components.Schemas["ErrorCode"]; !ok {
		t.Fatal("the contract has no ErrorCode enum; the code set has one authority and it moved")
	}

	// A code's status comes from the response components, because that is where a client
	// learns it. A code that appears in no component is unreachable no matter what the enum says.
	contractCodes := map[string]string{}
	for _, resp := range doc.Components.Responses {
		if resp.XStatus == nil || len(resp.XCodes) == 0 {
			continue
		}
		for _, c := range resp.XCodes {
			contractCodes[c] = fmt.Sprintf("%03d", *resp.XStatus)
		}
	}

	for _, code := range sortedKeys(catalogue) {
		want := catalogue[code]
		got, reachable := contractCodes[code]
		if !reachable {
			t.Errorf("%s is in the catalogue as %s but no response component declares it, so no route can "+
				"return it", code, want)
			continue
		}
		if got != want {
			t.Errorf("%s is %s in the catalogue and %s in the contract; one of them is wrong, and a client "+
				"branching on the status would be wrong for every such error", code, want, got)
		}
	}

	// The enum is what a generated client reads, so it has to be the same set.
	enumCodes := parseStringList(t, Document, "ErrorCode")

	for _, code := range enumCodes {
		if _, ok := catalogue[code]; !ok {
			t.Errorf("the contract's ErrorCode enum declares %s, which the catalogue does not define; a code "+
				"is permanent by contract, so this one was introduced without a decision", code)
		}
	}
	if len(enumCodes) != len(catalogue) {
		t.Errorf("the ErrorCode enum has %d entries and the catalogue has %d codes", len(enumCodes), len(catalogue))
	}
}

// TestMatrixAndContractAgree is DoD item 6.
//
// A cell listing several routes states the codes those routes *can* return, not that each
// route returns all of them, so the comparison is against the union over the cell. That rule
// is written into error-catalog.md section 3 and it is the only reading that does not require
// these documents to guess which route returns which code, which they deliberately do not say.
func TestMatrixAndContractAgree(t *testing.T) {
	doc := loadContract(t)
	_, matrix := readCatalogue(t)

	httpMethods := map[string]bool{
		"get": true, "put": true, "post": true, "delete": true,
		"options": true, "head": true, "patch": true, "trace": true,
	}

	routeCodes := map[string]map[string]bool{}
	for path, methods := range doc.Paths {
		for method, node := range methods {
			if !httpMethods[strings.ToLower(method)] {
				continue
			}
			var op struct {
				Responses map[string]struct {
					Ref string `yaml:"$ref"`
				} `yaml:"responses"`
			}
			if err := node.Decode(&op); err != nil {
				t.Errorf("the operation %s %s does not parse: %v", strings.ToUpper(method), path, err)
				continue
			}
			key := strings.ToUpper(method) + " " + path
			routeCodes[key] = map[string]bool{}
			for _, resp := range op.Responses {
				name := strings.TrimPrefix(resp.Ref, "#/components/responses/")
				comp, ok := doc.Components.Responses[name]
				if !ok {
					t.Errorf("the route %s references the response component %s, which does not exist", key, name)
					continue
				}
				for _, code := range comp.XCodes {
					routeCodes[key][code] = true
				}
			}
		}
	}
	if len(routeCodes) == 0 {
		t.Fatal("no routes parsed from the contract")
	}

	covered := map[string]bool{}
	for _, cell := range matrix {
		// The union the cell asserts, and the union the contract provides.
		var want, got map[string]bool
		want, got = map[string]bool{}, map[string]bool{}
		for code := range cell.codes {
			want[code] = true
		}
		for _, route := range cell.routes {
			covered[route] = true
			declared, known := routeCodes[route]
			if !known {
				t.Errorf("the matrix names %s, which is not a route in the contract", route)
				continue
			}
			for code := range declared {
				got[code] = true
			}
		}
		for _, code := range sortedKeys(want) {
			if !got[code] {
				t.Errorf("the matrix says the routes %s can return %s, but no response on them declares it",
					strings.Join(cell.routes, ", "), code)
			}
		}
		for _, code := range sortedKeys(got) {
			if !want[code] {
				t.Errorf("the routes %s declare %s, which no matrix cell names, so the code is undocumented",
					strings.Join(cell.routes, ", "), code)
			}
		}
	}

	// Every route in the contract has to appear in the matrix. A documented route is not the
	// same as a specified one, and this is where a route added for convenience would be caught.
	for _, key := range sortedKeys(routeCodes) {
		if !covered[key] {
			t.Errorf("the contract defines %s, which the endpoint matrix does not list", key)
		}
	}
}

// TestEveryResponseCarriesARequestID is DoD item 4.
//
// NFR-S10 is unconditional, and the easy case to forget is a 204 or an error response,
// because they "have no body" and it feels like there is nothing to correlate. A 204 on a
// delete is exactly the response an operator needs a request id from.
func TestEveryResponseCarriesARequestID(t *testing.T) {
	doc := loadContract(t)
	for name, resp := range doc.Components.Responses {
		if _, ok := resp.Headers["X-Request-Id"]; !ok {
			t.Errorf("the response component %s does not declare X-Request-Id", name)
			continue
		}
		if got := resp.Headers["X-Request-Id"].Ref; got != "#/components/headers/XRequestId" {
			t.Errorf("the response component %s declares X-Request-Id as %q, want the shared header", name, got)
		}
	}
}

// objectValidator is the shape every generated object type satisfies: decode from JSON, then
// check it. It is one interface rather than two because the interesting failure is a type that
// decodes and then fails validation, and a helper that only decoded would miss half of it.
//
// Scalar aliases are deliberately not in this interface. ogen generates `Validate` on them
// with a value receiver and no `Decode`, so they cannot satisfy a pointer-receiver interface,
// and forcing them through one would mean a hand-written shim per alias.
type objectValidator interface {
	Decode(*jx.Decoder) error
	Validate() error
}

// scalarValidator is a generated scalar alias, validated by the same generated rules.
type scalarValidator interface {
	Validate() error
}

// TestExamplesValidate is DoD item 2.
//
// An example that does not validate is worse than no example: it is a sample client that
// receives a 400 from the API it is documenting, and the reader reasonably concludes the
// service is broken rather than the document.
//
// Object examples are decoded and validated by the same generated code the service will use,
// so this cannot pass by agreeing with a second implementation of the schema. Scalar examples
// are validated against the generated alias rules, which is what enforces the patterns.
func TestExamplesValidate(t *testing.T) {
	doc := loadContract(t)

	// Example-bearing object schemas, and the generated type each one decodes into. Written
	// out rather than reflected on, because a reflective lookup would silently skip a schema
	// it could not resolve and report success for the examples it never checked.
	objectTypes := map[string]func() objectValidator{
		"MeteredRequest":       func() objectValidator { return new(MeteredRequest) },
		"CheckResponse":        func() objectValidator { return new(CheckResponse) },
		"MeteredResponse":      func() objectValidator { return new(MeteredResponse) },
		"BalanceResponse":      func() objectValidator { return new(BalanceResponse) },
		"FeatureCreate":        func() objectValidator { return new(FeatureCreate) },
		"FeatureUpdate":        func() objectValidator { return new(FeatureUpdate) },
		"Feature":              func() objectValidator { return new(Feature) },
		"PlanCreate":           func() objectValidator { return new(PlanCreate) },
		"PlanUpdate":           func() objectValidator { return new(PlanUpdate) },
		"Plan":                 func() objectValidator { return new(Plan) },
		"PlanEntitlement":      func() objectValidator { return new(PlanEntitlement) },
		"PlanApplyRequest":     func() objectValidator { return new(PlanApplyRequest) },
		"PlanApplied":          func() objectValidator { return new(PlanApplied) },
		"PlanImpact":           func() objectValidator { return new(PlanImpact) },
		"TenantCreate":         func() objectValidator { return new(TenantCreate) },
		"TenantUpdate":         func() objectValidator { return new(TenantUpdate) },
		"Tenant":               func() objectValidator { return new(Tenant) },
		"OverrideUpsert":       func() objectValidator { return new(OverrideUpsert) },
		"OverrideClearRequest": func() objectValidator { return new(OverrideClearRequest) },
		"Override":             func() objectValidator { return new(Override) },
		"GrantRequest":         func() objectValidator { return new(GrantRequest) },
		"SetRequest":           func() objectValidator { return new(SetRequest) },
		"ForceRolloverRequest": func() objectValidator { return new(ForceRolloverRequest) },
		"ReasonRequest":        func() objectValidator { return new(ReasonRequest) },
		"ArchiveRequest":       func() objectValidator { return new(ArchiveRequest) },
		"DeleteRequest":        func() objectValidator { return new(DeleteRequest) },
		"KeyCreate":            func() objectValidator { return new(KeyCreate) },
		"KeyCreated":           func() objectValidator { return new(KeyCreated) },
		"Key":                  func() objectValidator { return new(Key) },
		"Status":               func() objectValidator { return new(Status) },
		"DependencyHealth":     func() objectValidator { return new(DependencyHealth) },
		"UsageEvent":           func() objectValidator { return new(UsageEvent) },
		"AuditEntry":           func() objectValidator { return new(AuditEntry) },
		"HealthResponse":       func() objectValidator { return new(HealthResponse) },
	}
	// String aliases, whose examples are JSON strings.
	stringScalars := map[string]func(string) scalarValidator{
		"FeatureKey":      func(s string) scalarValidator { v := FeatureKey(s); return &v },
		"PlanKey":         func(s string) scalarValidator { v := PlanKey(s); return &v },
		"ExternalId":      func(s string) scalarValidator { v := ExternalId(s); return &v },
		"IdempotencyKey":  func(s string) scalarValidator { v := IdempotencyKey(s); return &v },
		"RequestId":       func(s string) scalarValidator { v := RequestId(s); return &v },
		"Cursor":          func(s string) scalarValidator { v := Cursor(s); return &v },
		"Unit":            func(s string) scalarValidator { v := Unit(s); return &v },
		"OverrideApplyAt": func(s string) scalarValidator { v := OverrideApplyAt(s); return &v },
		"ResetInterval":   func(s string) scalarValidator { v := ResetInterval(s); return &v },
	}
	// Integer aliases, whose examples are JSON numbers. They are separate from the string
	// aliases because `json.Unmarshal` into a `string` rejects a number outright, and folding
	// the two together would mean unmarshalling into `any` and re-deciding the type here, which
	// is the kind of second implementation of the schema this test exists to avoid.
	intScalars := map[string]func(int64) scalarValidator{
		"Amount":  func(n int64) scalarValidator { v := Amount(n); return &v },
		"Limit":   func(n int64) scalarValidator { v := Limit(n); return &v },
		"Version": func(n int64) scalarValidator { v := Version(n); return &v },
	}
	scalarTypes := map[string]bool{}
	for k := range stringScalars {
		scalarTypes[k] = true
	}
	for k := range intScalars {
		scalarTypes[k] = true
	}
	// Free-form maps and the timestamp alias, whose examples are checked structurally rather
	// than through a generated type: they are maps and a time.Time, and neither has the
	// generated validators this test relies on.
	structural := map[string]bool{
		"Metadata": true, "ErrorDetails": true, "Timestamp": true,
	}

	for schemaName, schema := range doc.Components.Schemas {
		if len(schema.Examples) == 0 {
			continue
		}
		for exName, ex := range schema.Examples {
			raw := exampleJSON(t, ex)
			switch {
			case objectTypes[schemaName] != nil:
				v := objectTypes[schemaName]()
				if err := v.Decode(jx.DecodeBytes(raw)); err != nil {
					t.Errorf("the example %s/%s does not decode as a %s: %v", schemaName, exName, schemaName, err)
					continue
				}
				if err := v.Validate(); err != nil {
					t.Errorf("the example %s/%s does not validate against %s: %v", schemaName, exName, schemaName, err)
				}
			case stringScalars[schemaName] != nil:
				var s string
				if err := json.Unmarshal(raw, &s); err != nil {
					t.Errorf("the example %s/%s is not a JSON string: %v", schemaName, exName, err)
					continue
				}
				if err := stringScalars[schemaName](s).Validate(); err != nil {
					t.Errorf("the example %s/%s does not validate against %s: %v", schemaName, exName, schemaName, err)
				}
			case intScalars[schemaName] != nil:
				var n int64
				if err := json.Unmarshal(raw, &n); err != nil {
					t.Errorf("the example %s/%s is not a JSON integer: %v", schemaName, exName, err)
					continue
				}
				if err := intScalars[schemaName](n).Validate(); err != nil {
					t.Errorf("the example %s/%s does not validate against %s: %v", schemaName, exName, schemaName, err)
				}
			case structural[schemaName]:
				// Checked below by TestStructuralExamplesAreWellFormed.
			default:
				t.Errorf("the schema %s has examples but this test has no generated type for it, so those "+
					"examples are unchecked; add it to one of the tables above", schemaName)
			}
		}
	}

	// Every table entry must name a schema that still exists, so the check cannot rot into
	// passing because a schema was renamed and the examples went with it.
	for name := range objectTypes {
		if _, ok := doc.Components.Schemas[name]; !ok {
			t.Errorf("this test maps %s to a generated type, but the contract no longer defines the schema", name)
		}
	}
	for name := range scalarTypes {
		if _, ok := doc.Components.Schemas[name]; !ok {
			t.Errorf("this test maps %s to a generated type, but the contract no longer defines the schema", name)
		}
	}
	for name := range structural {
		if _, ok := doc.Components.Schemas[name]; !ok {
			t.Errorf("this test treats %s as structural, but the contract no longer defines the schema", name)
		}
	}

	// Examples under components/examples are whole error envelopes, because that is the only
	// kind of reusable example in this document.
	catalogue, _ := readCatalogue(t)
	for exName, ex := range doc.Components.Examples {
		raw := exampleJSON(t, ex)
		var envelope ErrorResponse
		if err := envelope.Decode(jx.DecodeBytes(raw)); err != nil {
			t.Errorf("the example %s is not a decodable error envelope: %v", exName, err)
			continue
		}
		if err := envelope.Validate(); err != nil {
			t.Errorf("the example %s does not validate: %v", exName, err)
		}
		// The envelope decoding is not the whole check. An example is also a claim about which
		// code this situation produces, and an example showing a code the catalogue does not
		// define teaches a client to branch on a value nobody promised to keep.
		if code := string(envelope.Error.Code); catalogue[code] == "" {
			t.Errorf("the example %s shows code %s, which the catalogue does not define", exName, code)
		}
	}
	if len(doc.Components.Examples) == 0 {
		t.Error("no reusable error examples parsed from components/examples")
	}
}

// TestStructuralExamplesAreWellFormed checks the examples belonging to schemas that have no
// generated object type: the two free-form maps and the timestamp alias.
//
// The timestamp check is the one that matters. A response example with a naive timestamp
// would pass every other test in this file and would contradict api-conventions.md §3, which
// requires an explicit offset on every timestamp so no client has to branch on the zone.
func TestStructuralExamplesAreWellFormed(t *testing.T) {
	doc := loadContract(t)

	for _, name := range []string{"Timestamp", "Metadata", "ErrorDetails"} {
		schema, ok := doc.Components.Schemas[name]
		if !ok {
			t.Errorf("the contract no longer defines the schema %s", name)
			continue
		}
		for exName, ex := range schema.Examples {
			raw := exampleJSON(t, ex)
			if name == "Timestamp" {
				var s string
				if err := json.Unmarshal(raw, &s); err != nil {
					t.Errorf("the example %s/%s is not a JSON string: %v", name, exName, err)
					continue
				}
				ts, err := time.Parse(time.RFC3339, s)
				if err != nil {
					t.Errorf("the example %s/%s is not an RFC 3339 timestamp: %v", name, exName, err)
					continue
				}
				// A zero offset is UTC and `Z` is the whole value, which the rules allow. Any
				// other zone must be spelled out, which time.Parse already enforces.
				_ = ts
				if !strings.ContainsAny(s, "+-") && !strings.HasSuffix(s, "Z") {
					t.Errorf("the example %s/%s has no explicit offset: %q", name, exName, s)
				}
				continue
			}
			// A free-form map must still be a JSON object, not a scalar or a list.
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Errorf("the example %s/%s is not a JSON object: %v", name, exName, err)
			}
		}
	}
}

// exampleJSON renders an example's value as JSON.
//
// The conversion goes through an untyped value rather than re-marshalling the YAML node,
// because marshalling a node emits YAML flow style, not JSON: `yaml.Marshal` on a mapping
// node produces `key: value` lines, which is not a JSON object and would fail every decode
// with a confusing "expected {" at byte 0.
func exampleJSON(t *testing.T, ex example) []byte {
	t.Helper()
	var v any
	node := ex.Value
	if err := node.Decode(&v); err != nil {
		t.Fatalf("decode the example value: %v", err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("render the example as JSON: %v", err)
	}
	return raw
}

// parseStringList reads the `enum` of one schema out of the document as plain strings.
//
// It re-parses the document with a second struct rather than reusing contractDoc, because the
// enum is a list of scalars and a struct that also carries the response components cannot
// hold both shapes.
func parseStringList(t *testing.T, doc []byte, schema string) []string {
	t.Helper()
	var parsed struct {
		Components struct {
			Schemas map[string]struct {
				Enum []string `yaml:"enum"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("parse openapi.yaml for the %s enum: %v", schema, err)
	}
	enum := parsed.Components.Schemas[schema].Enum
	if len(enum) == 0 {
		t.Fatalf("the schema %s has no enum", schema)
	}
	return enum
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
