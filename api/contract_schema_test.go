package api

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// These are api-conventions.md section 13 items 3, 7 and 8. They are here because that section
// says CI enforces them, and a document that claims a check which does not exist is worse than one
// that admits the gap: the reader stops looking.

// TestEverySchemaFieldHasATypeAndADescription is item 3.
//
// A field with a type and no description is a decision nobody wrote down, and a field with a
// description and no type is a sentence about a type that does not exist. Either one reaches a
// client as a guess.
//
// A description may be inherited. A property that is a bare `$ref`, or an `allOf` of one, says what
// it is by pointing at a component that already has prose, and demanding a second copy of that
// prose at every use would be a rule that rewards duplication.
func TestEverySchemaFieldHasATypeAndADescription(t *testing.T) {
	t.Parallel()

	m := parseDocModel(t, Document)
	checked := 0
	forEachProperty(t, &m, func(owner, prop string, node *yaml.Node) {
		checked++
		if !declaresAType(node) {
			t.Errorf("%s.%s declares no type, no $ref and no composition", owner, prop)
		}
		if !hasADescription(t, &m, node, map[string]bool{}) {
			t.Errorf("%s.%s has no description, and its referenced schemas have none either", owner, prop)
		}
	})
	if checked == 0 {
		t.Fatal("no schema property was checked, so this test would pass vacuously")
	}
}

// declaresAType reports whether a node says what type it is, directly or by composition.
func declaresAType(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "type", "$ref", "enum", "const", "allOf", "oneOf", "anyOf":
			return true
		}
	}
	return false
}

// hasADescription reports whether a node carries prose of its own, or delegates to a component
// that does. The visited set stops a cycle of mutually referencing schemas from recursing forever,
// which a document with two schemas referring to each other would otherwise turn into a hang
// rather than a test failure.
func hasADescription(t *testing.T, m *docModel, node *yaml.Node, visited map[string]bool) bool {
	t.Helper()
	if node == nil {
		return false
	}
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		val := node.Content[i+1]
		switch key {
		case "description":
			if strings.TrimSpace(val.Value) != "" {
				return true
			}
		case "$ref":
			target := componentByPointer(t, m, val.Value)
			if target == nil {
				return false
			}
			if visited[val.Value] {
				continue
			}
			visited[val.Value] = true
			if hasADescription(t, m, target, visited) {
				return true
			}
		case "allOf", "oneOf", "anyOf":
			for _, member := range val.Content {
				if hasADescription(t, m, member, visited) {
					return true
				}
			}
		}
	}
	return false
}

// TestEverySchemaHasADescription is the same rule one level up, for the same reason: a named type
// with no prose is a name.
func TestEverySchemaHasADescription(t *testing.T) {
	t.Parallel()

	m := parseDocModel(t, Document)
	checked := 0
	for i := 0; i+1 < len(m.Components.Schemas.Content); i += 2 {
		name := m.Components.Schemas.Content[i].Value
		schema := readSchema(m.Components.Schemas.Content[i+1], name)
		checked++
		if strings.TrimSpace(schema.Description) == "" {
			t.Errorf("schema %s has no description", name)
		}
	}
	if checked == 0 {
		t.Fatal("no schema was checked, so this test would pass vacuously")
	}
}

// TestNoRefIsUnresolvedAndNoComponentIsDead is item 7.
//
// An unresolved `$ref` is a schema that does not exist as far as any client is concerned, and it
// fails at their end rather than ours. A component nothing references is a draft or a mistake, and
// neither should survive review. Both halves are checked here, and the second half is the one that
// catches the schema somebody renamed.
func TestNoRefIsUnresolvedAndNoComponentIsDead(t *testing.T) {
	t.Parallel()

	m := parseDocModel(t, Document)
	defined := map[string]bool{}
	used := map[string]bool{}
	for _, section := range []struct {
		name string
		node yaml.Node
	}{
		{"schemas", m.Components.Schemas},
		{"parameters", m.Components.Parameters},
		{"responses", m.Components.Responses},
		{"requestBodies", m.Components.RequestBodies},
		{"headers", m.Components.Headers},
		{"examples", m.Components.Examples},
	} {
		for i := 0; i+1 < len(section.node.Content); i += 2 {
			defined["components/"+section.name+"/"+section.node.Content[i].Value] = true
		}
	}
	if len(defined) == 0 {
		t.Fatal("the document has no components, so this test would pass vacuously")
	}

	for _, pointer := range collectRefs(t, &m) {
		target := strings.TrimPrefix(pointer, "#/")
		if !defined[target] {
			t.Errorf("the document references %s, which is not defined", pointer)
			continue
		}
		used[target] = true
	}
	for target := range defined {
		if !used[target] {
			t.Errorf("%s is defined and never referenced", target)
		}
	}
}

// TestUnexpressibleConstraintsNameTheirMechanism is item 8.
//
// One constraint today cannot be expressed in JSON Schema: an override must set at least one of
// `limit_value` or `interval`, which is a rule about the pair rather than about either field. The
// rule is that a gap like this is stated in the schema with the mechanism that does enforce it, and
// that a silent gap fails the build. This test is the second half of that rule.
func TestUnexpressibleConstraintsNameTheirMechanism(t *testing.T) {
	t.Parallel()

	document := string(Document)
	for _, want := range []string{
		"tenant_overrides_not_empty",
	} {
		if !strings.Contains(document, want) {
			t.Errorf("the contract does not name %s, so the constraint it enforces is not stated "+
				"where a client will read it", want)
		}
	}
	// The prose has to be on the schema, not merely somewhere in the file, or a reader looking at
	// the type will not find it.
	m := parseDocModel(t, Document)
	for i := 0; i+1 < len(m.Components.Schemas.Content); i += 2 {
		if m.Components.Schemas.Content[i].Value != "OverrideUpsert" {
			continue
		}
		description := readSchema(m.Components.Schemas.Content[i+1], "OverrideUpsert").Description
		if !strings.Contains(description, "tenant_overrides_not_empty") {
			t.Errorf("the OverrideUpsert description does not name the constraint the store "+
				"enforces; it reads: %q", description)
		}
		return
	}
	t.Error("the contract has no OverrideUpsert schema")
}

// forEachProperty walks every property of every schema, including the properties of schemas
// reached only through another schema, and hands the caller the raw node so the test can apply its
// own rules about inheritance.
func forEachProperty(t *testing.T, m *docModel, visit func(owner, prop string, node *yaml.Node)) {
	t.Helper()
	if len(m.Components.Schemas.Content) == 0 {
		t.Fatal("the document has no schemas")
	}
	for i := 0; i+1 < len(m.Components.Schemas.Content); i += 2 {
		name := m.Components.Schemas.Content[i].Value
		schema := m.Components.Schemas.Content[i+1]
		if schema.Kind == yaml.AliasNode {
			schema = schema.Alias
		}
		for k := 0; k+1 < len(schema.Content); k += 2 {
			if schema.Content[k].Value != "properties" {
				continue
			}
			for p := 0; p+1 < len(schema.Content[k+1].Content); p += 2 {
				visit(name, schema.Content[k+1].Content[p].Value, schema.Content[k+1].Content[p+1])
			}
		}
	}
}

// componentByPointer resolves a local JSON pointer into the components the document defines. It
// understands the four sections the contract uses and returns nil for anything else, so a pointer
// into a section this helper does not walk is reported as unresolvable rather than silently
// treated as fine.
func componentByPointer(t *testing.T, m *docModel, pointer string) *yaml.Node {
	t.Helper()
	const prefix = "#/components/"
	if !strings.HasPrefix(pointer, prefix) {
		return nil
	}
	rest := strings.TrimPrefix(pointer, prefix)
	section, name, ok := strings.Cut(rest, "/")
	if !ok || name == "" {
		return nil
	}
	var section_node *yaml.Node
	switch section {
	case "schemas":
		section_node = &m.Components.Schemas
	case "parameters":
		section_node = &m.Components.Parameters
	case "responses":
		section_node = &m.Components.Responses
	case "requestBodies":
		section_node = &m.Components.RequestBodies
	default:
		return nil
	}
	for i := 0; i+1 < len(section_node.Content); i += 2 {
		if section_node.Content[i].Value == name {
			return section_node.Content[i+1]
		}
	}
	return nil
}

// collectRefs returns every `$ref` in the document as a document-relative pointer.
func collectRefs(t *testing.T, m *docModel) []string {
	t.Helper()
	var out []string
	var walk func(node *yaml.Node)
	walk = func(node *yaml.Node) {
		if node == nil {
			return
		}
		if node.Kind == yaml.AliasNode {
			node = node.Alias
		}
		switch node.Kind {
		case yaml.SequenceNode:
			// A sequence's children are values, not key/value pairs. Treating them as pairs skips
			// every single-element list, which is exactly the shape a lone `$ref` takes.
			for _, item := range node.Content {
				walk(item)
			}
			return
		case yaml.MappingNode:
			for i := 0; i+1 < len(node.Content); i += 2 {
				if node.Content[i].Value == "$ref" {
					out = append(out, node.Content[i+1].Value)
				}
				walk(node.Content[i])
				walk(node.Content[i+1])
			}
		}
	}
	walk(&m.Paths)
	walk(&m.Components.Schemas)
	walk(&m.Components.Parameters)
	walk(&m.Components.Responses)
	walk(&m.Components.RequestBodies)
	walk(&m.Components.Headers)
	walk(&m.Components.Examples)
	if len(out) == 0 {
		t.Fatal("the document has no $ref at all, so this test would pass vacuously")
	}
	return out
}
