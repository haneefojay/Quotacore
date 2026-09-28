package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// This file turns the contract into the single self-contained HTML page that `GET /docs` serves.
//
// It is deliberately written without a documentation library and without any script tag. The
// page has to work on a host with no egress, which is the same constraint as the rest of this
// product, and a reference page that loads anything from a CDN fails the one property the page
// exists to provide: an operator debugging a request at 02:00 needs the contract, and if the
// contract is on a CDN they do not have it.
//
// The output is deterministic. Every iteration below is over a slice in document order or over
// an explicitly sorted list, and nothing prints a timestamp, a map address or a build number.
// That is what makes the committed page comparable byte for byte, which is the property the CI
// drift check and TestDocsPageIsUpToDate both depend on: a page that embedded a timestamp would
// differ on every run and the check would have to be weakened until it proved nothing.

// docModel is the subset of the contract the page renders.
type docModel struct {
	Info struct {
		Title       string `yaml:"title"`
		Version     string `yaml:"version"`
		Description string `yaml:"description"`
	} `yaml:"info"`
	Tags []struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	} `yaml:"tags"`
	// `paths` and `components.schemas` are held as raw nodes rather than as typed structs.
	// Typed structs would mean restating the contract's shape here, and a second statement of a
	// schema is a second thing to keep correct. The nodes are taken by value because that is the
	// only form go.yaml.in/yaml/v3 populates: given a `*yaml.Node` field it leaves it zero.
	Paths      yaml.Node `yaml:"paths"`
	Security   yaml.Node `yaml:"security"`
	Components struct {
		Schemas       yaml.Node `yaml:"schemas"`
		Parameters    yaml.Node `yaml:"parameters"`
		Responses     yaml.Node `yaml:"responses"`
		RequestBodies yaml.Node `yaml:"requestBodies"`
		Headers       yaml.Node `yaml:"headers"`
		Examples      yaml.Node `yaml:"examples"`
	} `yaml:"components"`
}

// docRefs indexes the reusable component sections so a `$ref` can be followed.
//
// Almost every response in this contract is a `$ref` into components/responses, and a renderer
// that does not follow them prints a status code with no description and no body type. That is
// a page that looks complete and is not: the reader sees a `403` and learns nothing about which
// code it is or what shape it has.
type docRefs struct {
	parameters    map[string]*yaml.Node
	responses     map[string]*yaml.Node
	requestBodies map[string]*yaml.Node
}

func newDocRefs(m *docModel) docRefs {
	return docRefs{
		parameters:    componentIndex(&m.Components.Parameters),
		responses:     componentIndex(&m.Components.Responses),
		requestBodies: componentIndex(&m.Components.RequestBodies),
	}
}

// componentIndex turns a components mapping into a name-to-node lookup.
func componentIndex(section *yaml.Node) map[string]*yaml.Node {
	out := map[string]*yaml.Node{}
	if section == nil {
		return out
	}
	for i := 0; i+1 < len(section.Content); i += 2 {
		out[section.Content[i].Value] = section.Content[i+1]
	}
	return out
}

// resolve follows a local `$ref` of the given component section, and returns the node unchanged
// when it is not a ref or the target is missing. Returning the node unchanged is deliberate: a
// dangling ref should degrade the page, not fail the build, and the contract test that checks
// every ref resolves is the place where a dangling ref is a failure.
func (r docRefs) resolve(node *yaml.Node, section string) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	prefix := "#/components/" + section + "/"
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != "$ref" {
			continue
		}
		ref := node.Content[i+1].Value
		if !strings.HasPrefix(ref, prefix) {
			return node
		}
		var table map[string]*yaml.Node
		switch section {
		case "parameters":
			table = r.parameters
		case "responses":
			table = r.responses
		case "requestBodies":
			table = r.requestBodies
		}
		if target, ok := table[strings.TrimPrefix(ref, prefix)]; ok {
			return r.resolve(target, section)
		}
		return node
	}
	return node
}

// docOperation is one method on one path.
type docOperation struct {
	Method      string
	Path        string
	OperationID string
	Summary     string
	Description string
	Tags        []string
	Security    []string
	// RequiredScope is the key scope a credential must carry, read from `x-required-scope`.
	// It is not in the security requirement array because an HTTP bearer scheme has no scopes;
	// see the contract test TestSecurityRequirementsCarryNoScopes.
	RequiredScope string
	Parameters    []docParameter
	RequestBody   docBody
	Responses     []docResponse
	Deprecated    bool
}

type docParameter struct {
	Name        string
	In          string
	Required    bool
	Description string
	Schema      docSchema
}

type docBody struct {
	Required   bool
	SchemaName string
	Examples   []docExample
}

// docExample is one named example, rendered to JSON so the page shows the same bytes a client
// would send. A reader copying out of this page has to copy something that parses.
type docExample struct {
	Name string
	Body string
}

type docResponse struct {
	Status      string
	Description string
	SchemaNames []string
	ContentType []string
	Headers     []string
}

// docSchema is the flattened view of one schema, with `$ref`s reduced to names so the page can
// link to a schema section instead of printing a JSON pointer at the reader.
type docSchema struct {
	Name        string
	Type        string
	Description string
	Properties  []docProperty
	Enum        []string
	Constraints []string
}

type docProperty struct {
	Name        string
	Type        string
	Required    bool
	Description string
	Constraints []string
	Ref         string
	Nullable    bool
}

// httpMethodOrder is the order methods are printed in within one path, so two versions of the
// contract are comparable by eye. Paths keep the document's own order, because the document
// groups related routes together deliberately.
var httpMethodOrder = []string{"get", "put", "post", "patch", "delete", "options", "head", "trace"}

// RenderDocs builds the HTML reference page from a contract document.
//
// It takes the document as an argument rather than reading the embedded copy, so the generator
// can run over a working-tree file, the test can run over the bytes the binary serves, and a
// caller can render a document that is not this repository's.
func RenderDocs(document []byte) ([]byte, error) {
	var m docModel
	if err := yaml.Unmarshal(document, &m); err != nil {
		return nil, fmt.Errorf("parse the contract: %w", err)
	}
	if m.Paths.Kind == 0 || len(m.Paths.Content) == 0 {
		return nil, fmt.Errorf("the contract has no paths, so the page would be empty")
	}

	// Operations are grouped by tag in tag-declaration order, because the tag order is the only
	// ordering in the document a human chose deliberately. An operation with no tag lands in one
	// trailing group rather than being dropped, which a page that silently omits a route would.
	byTag := map[string][]docOperation{}
	tagDescriptions := map[string]string{}
	refs := newDocRefs(&m)
	var tagOrder []string
	for _, t := range m.Tags {
		tagDescriptions[t.Name] = t.Description
	}
	all := readOperations(&m.Paths, refs, &m.Security)
	// A page with headings and no operations is a reference that answers nothing, so a document
	// whose paths declare no HTTP method is refused rather than rendered into an empty shell.
	if len(all) == 0 {
		return nil, fmt.Errorf("the contract declares paths but no operation, so the page would be empty")
	}
	for _, op := range all {
		tags := op.Tags
		if len(tags) == 0 {
			tags = []string{"other"}
		}
		for _, t := range tags {
			if _, ok := byTag[t]; !ok && !slicesContains(tagOrder, t) {
				tagOrder = append(tagOrder, t)
			}
			byTag[t] = append(byTag[t], op)
		}
	}
	// Declared tags first, in the order the document declares them, then any tag an operation
	// uses without declaring. Appending tagOrder wholesale would list the declared tags twice and
	// render every operation twice, which is the kind of duplication a reader notices and an
	// automated check does not.
	order := make([]string, 0, len(m.Tags)+len(tagOrder))
	for _, t := range m.Tags {
		order = append(order, t.Name)
	}
	for _, t := range tagOrder {
		if !slicesContains(order, t) {
			order = append(order, t)
		}
	}

	var b bytes.Buffer
	writeDocHeader(&b, m.Info.Title, m.Info.Version, m.Info.Description)
	writeDocTOC(&b, order, byTag)
	for _, tag := range order {
		writeDocTag(&b, tag, tagDescriptions[tag], byTag[tag])
	}
	writeDocSchemas(&b, &m.Components.Schemas)
	writeDocFooter(&b)
	return b.Bytes(), nil
}

func writeDocHeader(b *bytes.Buffer, title, version, description string) {
	b.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + html.EscapeString(title) + `</title>
<style>
:root { color-scheme: light dark; --fg: #1a1a1a; --bg: #ffffff; --muted: #5c5c5c;
  --line: #d8d8d8; --code-bg: #f2f2f2; --get: #1f6f43; --post: #1c4f8b;
  --patch: #8a5a00; --delete: #a02020; --other: #444444; }
@media (prefers-color-scheme: dark) { :root { --fg: #e8e8e8; --bg: #141414; --muted: #a0a0a0;
  --line: #3a3a3a; --code-bg: #242424; --get: #6fcf97; --post: #7fb3f0;
  --patch: #e0b055; --delete: #ef8080; --other: #c0c0c0; } }
* { box-sizing: border-box; }
body { margin: 0 auto; padding: 2rem 1.25rem 6rem; max-width: 62rem;
  font: 16px/1.6 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
  color: var(--fg); background: var(--bg); }
h1 { font-size: 1.9rem; margin: 0 0 .25rem; }
h2 { font-size: 1.35rem; margin: 3rem 0 .5rem; padding-bottom: .3rem;
  border-bottom: 1px solid var(--line); }
h3 { font-size: 1.05rem; margin: 2rem 0 .4rem; }
h4 { font-size: .95rem; margin: 1.25rem 0 .35rem; color: var(--muted);
  text-transform: uppercase; letter-spacing: .04em; }
h5 { font-size: .85rem; margin: .9rem 0 .3rem; color: var(--muted); }
p, li { margin: .4rem 0; }
a { color: inherit; }
code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
code { background: var(--code-bg); padding: .1em .35em; border-radius: 3px; font-size: .9em; }
pre { background: var(--code-bg); padding: .75rem 1rem; border-radius: 4px; overflow-x: auto;
  font-size: .82rem; line-height: 1.45; }
pre code { background: none; padding: 0; }
table { border-collapse: collapse; width: 100%; margin: .6rem 0 1rem; font-size: .92rem; }
th, td { text-align: left; vertical-align: top; padding: .4rem .6rem;
  border-bottom: 1px solid var(--line); }
th { font-weight: 600; color: var(--muted); font-size: .8rem; text-transform: uppercase;
  letter-spacing: .03em; }
nav ul { list-style: none; padding-left: 0; }
nav > ul > li { margin: .15rem 0; }
nav ul ul { padding-left: 1.2rem; font-size: .92rem; }
.method { display: inline-block; min-width: 4.2rem; text-align: center; font-size: .72rem;
  font-weight: 700; letter-spacing: .06em; padding: .12rem .4rem; border-radius: 3px;
  color: #fff; background: var(--other); }
.method-get { background: var(--get); } .method-post { background: var(--post); }
.method-patch { background: var(--patch); } .method-delete { background: var(--delete); }
.muted { color: var(--muted); }
.path { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: .95rem; }
footer { margin-top: 3rem; padding-top: 1rem; border-top: 1px solid var(--line);
  color: var(--muted); font-size: .85rem; }
</style>
</head>
<body>
`)
	b.WriteString("<h1>" + html.EscapeString(title) + "</h1>\n")
	if version != "" {
		b.WriteString(`<p class="muted">Version ` + html.EscapeString(version) + `</p>` + "\n")
	}
	b.WriteString(inlineMarkup(description))
}

func writeDocTOC(b *bytes.Buffer, order []string, byTag map[string][]docOperation) {
	b.WriteString("<nav>\n<h2>Contents</h2>\n<ul>\n<li><a href=\"#schemas\">Schemas</a></li>\n")
	for _, tag := range order {
		fmt.Fprintf(b, "<li><a href=\"#%s\">%s</a>\n<ul>\n", "tag-"+slug(tag), html.EscapeString(tag))
		for _, op := range byTag[tag] {
			fmt.Fprintf(b, "<li><code>%s</code> <span class=\"path\">%s</span></li>\n",
				strings.ToUpper(op.Method), html.EscapeString(op.Path))
		}
		b.WriteString("</ul>\n</li>\n")
	}
	b.WriteString("</ul>\n</nav>\n<hr>\n")
}

func writeDocTag(b *bytes.Buffer, tag, description string, ops []docOperation) {
	fmt.Fprintf(b, "<h2 id=\"%s\">%s</h2>\n", "tag-"+slug(tag), html.EscapeString(tag))
	if description != "" {
		b.WriteString(inlineMarkup(description))
	}
	for _, op := range ops {
		writeDocOperation(b, op)
	}
}

func writeDocOperation(b *bytes.Buffer, op docOperation) {
	fmt.Fprintf(b, "<h3><span class=\"method method-%s\">%s</span> <span class=\"path\">%s</span></h3>\n",
		strings.ToLower(op.Method), strings.ToUpper(op.Method), html.EscapeString(op.Path))
	if op.Summary != "" {
		b.WriteString("<p><strong>" + inlineSpan(op.Summary) + "</strong></p>\n")
	}
	if op.Deprecated {
		b.WriteString(`<p class="muted">Deprecated.</p>` + "\n")
	}
	if op.Description != "" {
		b.WriteString(inlineMarkup(op.Description))
	}
	fmt.Fprintf(b, "<p class=\"muted\">Operation ID: <code>%s</code></p>\n", html.EscapeString(op.OperationID))

	b.WriteString("<h4>Authorisation</h4>\n")
	if len(op.Security) == 0 {
		b.WriteString(`<p class="muted">None. This route takes no credential.</p>` + "\n")
	} else {
		for _, s := range op.Security {
			if op.RequiredScope != "" {
				fmt.Fprintf(b, "<p>Requires %s, held by a key with the <code>%s</code> scope.</p>\n",
					s, html.EscapeString(op.RequiredScope))
				continue
			}
			b.WriteString("<p>Requires " + s + ".</p>\n")
		}
	}

	if len(op.Parameters) > 0 {
		b.WriteString("<h4>Parameters</h4>\n<table>\n<tr><th>Name</th><th>In</th><th>Required</th>" +
			"<th>Type</th><th>Description</th></tr>\n")
		for _, p := range op.Parameters {
			required := "no"
			if p.Required {
				required = "yes"
			}
			fmt.Fprintf(b, "<tr><td><code>%s</code></td><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
				html.EscapeString(p.Name), html.EscapeString(p.In), required,
				html.EscapeString(scalarType(p.Schema)),
				inlineMarkup(p.Description))
		}
		b.WriteString("</table>\n")
	}

	if op.RequestBody.SchemaName != "" {
		b.WriteString("<h4>Request body</h4>\n")
		required := "optional"
		if op.RequestBody.Required {
			required = "required"
		}
		fmt.Fprintf(b, "<p>The body is <strong>%s</strong> and is %s. Undeclared fields are "+
			"rejected with <code>400 validation_failed</code>.</p>\n",
			schemaLink(op.RequestBody.SchemaName), required)
		for _, ex := range op.RequestBody.Examples {
			fmt.Fprintf(b, "<h5>Example: %s</h5>\n<pre><code>%s</code></pre>\n",
				html.EscapeString(ex.Name), html.EscapeString(ex.Body))
		}
	}

	b.WriteString("<h4>Responses</h4>\n<table>\n<tr><th>Status</th><th>Content</th>" +
		"<th>Schema</th><th>Headers</th><th>Description</th></tr>\n")
	for _, r := range op.Responses {
		schema := "—"
		if len(r.SchemaNames) > 0 {
			parts := make([]string, 0, len(r.SchemaNames))
			for _, n := range r.SchemaNames {
				parts = append(parts, schemaLink(n))
			}
			schema = strings.Join(parts, ", ")
		}
		content := "—"
		if len(r.ContentType) > 0 {
			content = html.EscapeString(strings.Join(r.ContentType, ", "))
		}
		headers := "—"
		if len(r.Headers) > 0 {
			parts := make([]string, 0, len(r.Headers))
			for _, h := range r.Headers {
				parts = append(parts, "<code>"+html.EscapeString(h)+"</code>")
			}
			headers = strings.Join(parts, ", ")
		}
		fmt.Fprintf(b, "<tr><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			html.EscapeString(r.Status), content, schema, headers, inlineMarkup(r.Description))
	}
	b.WriteString("</table>\n")
}

// writeDocSchemas renders every schema in the document, in document order.
//
// A schema reachable only from another schema is still rendered. The alternative is a page that
// silently omits a type, and a reference that omits a type cannot answer a question, which is
// the only reason to have one.
func writeDocSchemas(b *bytes.Buffer, schemasNode *yaml.Node) {
	b.WriteString("<h2 id=\"schemas\">Schemas</h2>\n")
	if schemasNode == nil || len(schemasNode.Content) == 0 {
		b.WriteString(`<p class="muted">This document defines no schemas.</p>` + "\n")
		return
	}
	for i := 0; i+1 < len(schemasNode.Content); i += 2 {
		name := schemasNode.Content[i].Value
		s := readSchema(schemasNode.Content[i+1], name)
		fmt.Fprintf(b, "<h3 id=\"schema-%s\">%s</h3>\n", slug(name), html.EscapeString(name))
		if s.Type != "" {
			fmt.Fprintf(b, "<p class=\"muted\">Type <code>%s</code></p>\n", html.EscapeString(s.Type))
		}
		if s.Description != "" {
			b.WriteString(inlineMarkup(s.Description))
		}
		if len(s.Enum) > 0 {
			b.WriteString("<p>One of: ")
			for j, e := range s.Enum {
				if j > 0 {
					b.WriteString(", ")
				}
				b.WriteString("<code>" + html.EscapeString(e) + "</code>")
			}
			b.WriteString("</p>\n")
		}
		if len(s.Constraints) > 0 {
			b.WriteString("<ul>\n")
			for _, c := range s.Constraints {
				b.WriteString("<li>" + inlineMarkup(c) + "</li>\n")
			}
			b.WriteString("</ul>\n")
		}
		if len(s.Properties) > 0 {
			b.WriteString("<table>\n<tr><th>Field</th><th>Type</th><th>Required</th><th>Description</th></tr>\n")
			for _, p := range s.Properties {
				required := "no"
				if p.Required {
					required = "yes"
				}
				fmt.Fprintf(b, "<tr><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
					html.EscapeString(p.Name), schemaLinkOr(p.Ref, p.Type), required,
					inlineMarkup(p.Description))
			}
			b.WriteString("</table>\n")
		}
		if ex, ok := schemaExamples(schemasNode.Content[i+1]); ok {
			writeSchemaExamples(b, ex)
		}
	}
}

// writeSchemaExamples renders a schema's own named examples, sorted by name so the page is stable
// regardless of how the author ordered them in the document.
func writeSchemaExamples(b *bytes.Buffer, examples map[string]string) {
	names := make([]string, 0, len(examples))
	for n := range examples {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(b, "<h4>Example: %s</h4>\n<pre><code>%s</code></pre>\n",
			html.EscapeString(n), html.EscapeString(examples[n]))
	}
}

func writeDocFooter(b *bytes.Buffer) {
	b.WriteString(`<footer>
<p>Generated from <code>api/openapi.yaml</code> by <code>make docs</code>. Regenerate after
changing the contract; this page is a rendering of it and carries no information the contract does
not.</p>
<p>This page is self-contained: no third-party JavaScript, no stylesheet and no font is loaded
from a network, because the service it documents runs on a host with no egress.</p>
</footer>
</body>
</html>
`)
}

// inlineMarkup renders the small subset of Markdown the contract uses, after HTML-escaping.
//
// Escaping first and transforming second is the whole safety argument: the contract's
// descriptions contain `<`, `>` and `&` inside prose, and a page that emitted raw HTML from a
// field description would be a stored-injection vector on a page an operator is told to trust.
// The escaping happens inside markupLine, so every path into the page goes through it and there
// is no caller that can forget.
func inlineMarkup(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	blocks := strings.Split(strings.TrimRight(s, "\n"), "\n\n")
	out := make([]string, 0, len(blocks))
	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		pre := false
		for j, line := range lines {
			lines[j] = markupLine(line)
			if strings.HasPrefix(line, "  ") {
				pre = true
			}
		}
		joined := strings.Join(lines, "\n")
		switch {
		case pre:
			out = append(out, "<pre><code>"+strings.Trim(joined, "\n")+"</code></pre>")
		case strings.Contains(joined, "\n"):
			out = append(out, "<p>"+strings.ReplaceAll(joined, "\n", "<br>")+"</p>")
		default:
			out = append(out, "<p>"+joined+"</p>")
		}
	}
	return strings.Join(out, "\n") + "\n"
}

// inlineSpan renders text for a caller that has already opened a block element, so it must not
// open another one. A summary is one sentence; if an author wraps it across lines, the break is
// not a paragraph and folding it to a space is the faithful reading.
func inlineSpan(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, line := range lines {
		lines[i] = markupLine(strings.TrimSpace(line))
	}
	return strings.Join(lines, " ")
}

// markupLine escapes one line and then turns the contract's inline markers into elements.
//
// The markers chosen are the three the document actually uses: a backtick pair, a double asterisk
// and a single asterisk pair. Single asterisks are only honoured when they hug the word, because
// `a * b * c` is arithmetic in prose and rendering half of it as emphasis would be worse than
// leaving the asterisks visible.
func markupLine(line string) string {
	line = html.EscapeString(line)
	var out strings.Builder
	for i := 0; i < len(line); {
		if line[i] == '`' {
			if j := strings.IndexByte(line[i+1:], '`'); j >= 0 {
				out.WriteString("<code>")
				out.WriteString(line[i+1 : i+1+j])
				out.WriteString("</code>")
				i += j + 2
				continue
			}
		}
		if strings.HasPrefix(line[i:], "**") {
			if j := strings.Index(line[i+2:], "**"); j >= 0 {
				out.WriteString("<strong>")
				out.WriteString(line[i+2 : i+2+j])
				out.WriteString("</strong>")
				i += j + 4
				continue
			}
		}
		if line[i] == '*' && i+1 < len(line) && line[i+1] != ' ' {
			if j := strings.IndexByte(line[i+1:], '*'); j > 0 && line[i+1+j-1] != ' ' {
				out.WriteString("<em>")
				out.WriteString(line[i+1 : i+1+j])
				out.WriteString("</em>")
				i += j + 2
				continue
			}
		}
		out.WriteByte(line[i])
		i++
	}
	return out.String()
}

func schemaLink(name string) string {
	if name == "" {
		return "—"
	}
	return "<a href=\"#schema-" + slug(name) + "\"><code>" + html.EscapeString(name) + "</code></a>"
}

func schemaLinkOr(ref, typ string) string {
	if ref != "" {
		return schemaLink(ref)
	}
	if typ == "" {
		return "—"
	}
	return "<code>" + html.EscapeString(typ) + "</code>"
}

func scalarType(s docSchema) string {
	switch {
	case s.Type != "":
		return s.Type
	default:
		return "—"
	}
}

// slug builds a GitHub-compatible anchor from a name.
//
// The rules mirror the link checker in tools/check-docs.ps1: lowercase, spaces to hyphens,
// punctuation dropped. Dropping rather than encoding is deliberate, because two names that
// encoded to the same anchor would give two sections one target and send the reader to the
// first one.
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		case r == ' ' || r == '_' || r == '-' || r == '/' || r == '.' || r == '{' || r == '}':
			dash = true
		default:
		}
	}
	return b.String()
}

func slicesContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// readOperations walks the paths mapping and returns one entry per method.
//
// Methods are selected by key rather than by position: a YAML mapping holds its pairs in
// document order and the document does not sort them, so positional indexing would attribute a
// body to the wrong verb.
func readOperations(paths *yaml.Node, refs docRefs, globalSecurity *yaml.Node) []docOperation {
	var ops []docOperation
	for i := 0; i+1 < len(paths.Content); i += 2 {
		path := paths.Content[i].Value
		item := paths.Content[i+1]
		shared := sharedParameters(item, refs)
		for j := 0; j+1 < len(item.Content); j += 2 {
			method := strings.ToLower(item.Content[j].Value)
			if !isHTTPMethod(method) {
				continue
			}
			ops = append(ops, readOperation(method, path, item.Content[j+1], shared, refs, globalSecurity))
		}
	}
	sort.SliceStable(ops, func(a, b int) bool {
		if ops[a].Path != ops[b].Path {
			return false
		}
		return methodRank(ops[a].Method) < methodRank(ops[b].Method)
	})
	return ops
}

func isHTTPMethod(m string) bool {
	return methodRank(m) < len(httpMethodOrder)
}

func methodRank(m string) int {
	for i, k := range httpMethodOrder {
		if k == strings.ToLower(m) {
			return i
		}
	}
	return len(httpMethodOrder)
}

// sharedParameters returns the path-level `parameters` list. Entries are `$ref`s into
// components/parameters, so they are followed and rendered like any other parameter.
func sharedParameters(item *yaml.Node, refs docRefs) []docParameter {
	for i := 0; i+1 < len(item.Content); i += 2 {
		if item.Content[i].Value == "parameters" {
			return readParameters(item.Content[i+1], refs)
		}
	}
	return nil
}

func readOperation(method, path string, node *yaml.Node, shared []docParameter, refs docRefs, globalSecurity *yaml.Node) docOperation {
	op := docOperation{Method: method, Path: path, Parameters: append([]docParameter{}, shared...)}
	declaredSecurity := false
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		val := node.Content[i+1]
		switch key {
		case "operationId":
			op.OperationID = val.Value
		case "summary":
			op.Summary = val.Value
		case "description":
			op.Description = val.Value
		case "deprecated":
			op.Deprecated = val.Value == "true"
		case "x-required-scope":
			op.RequiredScope = val.Value
		case "tags":
			for _, t := range val.Content {
				op.Tags = append(op.Tags, t.Value)
			}
		case "security":
			// An operation that declares no `security` inherits the document-level one, which is
			// what OpenAPI means by omission. Reading only the operation-level key would call the
			// whole data plane unauthenticated, which is the opposite of the truth.
			op.Security = readSecurityRequirements(val)
			declaredSecurity = true
		case "parameters":
			op.Parameters = append(op.Parameters, readParameters(val, refs)...)
		case "requestBody":
			op.RequestBody = readRequestBody(val, refs)
		case "responses":
			op.Responses = readResponses(val, refs)
		}
	}
	if !declaredSecurity {
		op.Security = readSecurityRequirements(globalSecurity)
	}
	return op
}

// readSecurityRequirements returns the scheme names a security requirement list demands, or nil
// for `security: []`, which is how a route says it takes no credential.
func readSecurityRequirements(node *yaml.Node) []string {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	var out []string
	for _, entry := range node.Content {
		for j := 0; j+1 < len(entry.Content); j += 2 {
			out = append(out, "`"+entry.Content[j].Value+"`")
		}
	}
	return out
}

func readParameters(node *yaml.Node, refs docRefs) []docParameter {
	var out []docParameter
	for _, entry := range node.Content {
		entry = refs.resolve(entry, "parameters")
		p := docParameter{}
		for k := 0; k+1 < len(entry.Content); k += 2 {
			switch entry.Content[k].Value {
			case "name":
				p.Name = entry.Content[k+1].Value
			case "in":
				p.In = entry.Content[k+1].Value
			case "required":
				p.Required = entry.Content[k+1].Value == "true"
			case "description":
				p.Description = entry.Content[k+1].Value
			case "schema":
				p.Schema = readSchema(entry.Content[k+1], "")
			}
		}
		if p.Name != "" {
			out = append(out, p)
		}
	}
	return out
}

func readRequestBody(node *yaml.Node, refs docRefs) docBody {
	node = refs.resolve(node, "requestBodies")
	body := docBody{Required: boolField(node, "required")}
	for k := 0; k+1 < len(node.Content); k += 2 {
		if node.Content[k].Value != "content" {
			continue
		}
		for c := 0; c+1 < len(node.Content[k+1].Content); c += 2 {
			media := node.Content[k+1].Content[c+1]
			for e := 0; e+1 < len(media.Content); e += 2 {
				switch media.Content[e].Value {
				case "schema":
					if ref := refName(media.Content[e+1]); ref != "" {
						body.SchemaName = ref
					}
				case "examples":
					for x := 0; x+1 < len(media.Content[e+1].Content); x += 2 {
						if ex, ok := renderExample(media.Content[e+1].Content[x].Value,
							media.Content[e+1].Content[x+1]); ok {
							body.Examples = append(body.Examples, ex)
						}
					}
				}
			}
		}
	}
	return body
}

func readResponses(node *yaml.Node, refs docRefs) []docResponse {
	var out []docResponse
	for r := 0; r+1 < len(node.Content); r += 2 {
		resp := docResponse{Status: node.Content[r].Value}
		body := refs.resolve(node.Content[r+1], "responses")
		resp.Description = descriptionOf(body)
		for k := 0; k+1 < len(body.Content); k += 2 {
			switch body.Content[k].Value {
			case "headers":
				for h := 0; h+1 < len(body.Content[k+1].Content); h += 2 {
					if name := refName(body.Content[k+1].Content[h]); name != "" {
						resp.Headers = append(resp.Headers, name)
						continue
					}
					resp.Headers = append(resp.Headers, body.Content[k+1].Content[h].Value)
				}
			case "content":
				for c := 0; c+1 < len(body.Content[k+1].Content); c += 2 {
					media := body.Content[k+1].Content[c+1]
					resp.ContentType = append(resp.ContentType, body.Content[k+1].Content[c].Value)
					for e := 0; e+1 < len(media.Content); e += 2 {
						if media.Content[e].Value != "schema" {
							continue
						}
						if ref := refName(media.Content[e+1]); ref != "" {
							resp.SchemaNames = append(resp.SchemaNames, ref)
						}
					}
				}
			}
		}
		out = append(out, resp)
	}
	return out
}

func descriptionOf(node *yaml.Node) string {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "description" {
			return node.Content[i+1].Value
		}
	}
	return ""
}

func boolField(node *yaml.Node, key string) bool {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1].Value == "true"
		}
	}
	return false
}

// refName returns the last segment of a local `$ref`, which is the schema, response or parameter
// name. It accepts either the mapping that carries the `$ref` or the scalar that holds the
// pointer, because both shapes occur in the document and only the mapping shape is obvious.
func refName(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	if n.Kind == yaml.AliasNode {
		return refName(n.Alias)
	}
	if n.Kind == yaml.ScalarNode {
		return lastSegment(n.Value)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value != "$ref" {
			continue
		}
		return lastSegment(n.Content[i+1].Value)
	}
	return ""
}

func lastSegment(ref string) string {
	if idx := strings.LastIndex(ref, "/"); idx >= 0 {
		return ref[idx+1:]
	}
	return ref
}

// renderExample extracts the `value` of a named example and renders it as JSON.
func renderExample(name string, node *yaml.Node) (docExample, bool) {
	if node == nil {
		return docExample{}, false
	}
	ex := node
	if ex.Kind == yaml.AliasNode && ex.Alias != nil {
		ex = ex.Alias
	}
	for i := 0; i+1 < len(ex.Content); i += 2 {
		if ex.Content[i].Value != "value" {
			continue
		}
		var v any
		if err := ex.Content[i+1].Decode(&v); err != nil {
			return docExample{}, false
		}
		rendered, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return docExample{}, false
		}
		return docExample{Name: name, Body: string(rendered)}, true
	}
	return docExample{}, false
}

// schemaExamples renders every named example belonging to one schema, sorted by name.
func schemaExamples(schema *yaml.Node) (map[string]string, bool) {
	out := map[string]string{}
	for k := 0; k+1 < len(schema.Content); k += 2 {
		if schema.Content[k].Value != "examples" {
			continue
		}
		for e := 0; e+1 < len(schema.Content[k+1].Content); e += 2 {
			if ex, ok := renderExample(schema.Content[k+1].Content[e].Value,
				schema.Content[k+1].Content[e+1]); ok {
				out[ex.Name] = ex.Body
			}
		}
	}
	return out, len(out) > 0
}

func readSchema(node *yaml.Node, name string) docSchema {
	s := docSchema{Name: name}
	if node == nil {
		return s
	}
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	required := map[string]bool{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		val := node.Content[i+1]
		switch key {
		case "$ref":
			s.Type = refName(val)
		case "type":
			s.Type = unionType(val)
		case "format":
			if s.Type != "" {
				s.Type += " (" + val.Value + ")"
			} else {
				s.Type = val.Value
			}
		case "description":
			s.Description = val.Value
		case "required":
			for _, r := range val.Content {
				required[r.Value] = true
			}
		case "enum":
			for _, e := range val.Content {
				s.Enum = append(s.Enum, e.Value)
			}
		case "properties":
			for p := 0; p+1 < len(val.Content); p += 2 {
				prop := docProperty{
					Name:     val.Content[p].Value,
					Required: required[val.Content[p].Value],
				}
				prop.Type, prop.Ref, prop.Description, prop.Constraints, prop.Nullable =
					readProperty(val.Content[p+1])
				s.Properties = append(s.Properties, prop)
			}
		default:
			if c := constraint(key, val); c != "" {
				s.Constraints = append(s.Constraints, c)
			}
		}
	}
	return s
}

func readProperty(node *yaml.Node) (typ, ref, description string, constraints []string, nullable bool) {
	if node == nil {
		return "", "", "", nil, false
	}
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		val := node.Content[i+1]
		switch key {
		case "$ref":
			ref = refName(val)
		case "type":
			typ = unionType(val)
			nullable = nullable || isNullable(val)
		case "format":
			if typ == "" {
				typ = val.Value
			} else {
				typ += " (" + val.Value + ")"
			}
		case "description":
			description = val.Value
		case "nullable":
			if val.Value == "true" {
				nullable = true
			}
		default:
			if c := constraint(key, val); c != "" {
				constraints = append(constraints, c)
			}
		}
	}
	if nullable {
		constraints = append(constraints, "May be `null`.")
	}
	return typ, ref, description, constraints, nullable
}

func unionType(node *yaml.Node) string {
	if node == nil {
		return ""
	}
	if node.Kind == yaml.ScalarNode {
		return node.Value
	}
	names := make([]string, 0, len(node.Content))
	for _, c := range node.Content {
		names = append(names, c.Value)
	}
	return strings.Join(names, " or ")
}

func isNullable(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.ScalarNode {
		return node.Value == "null"
	}
	for _, c := range node.Content {
		if c.Value == "null" {
			return true
		}
	}
	return false
}

// constraint renders one validation keyword as prose, so a reader sees the bound without having
// to know that `maximum` is what `maximum` means in JSON Schema. An unknown keyword renders as
// nothing, which means a future keyword is ignored by the page rather than printed raw.
func constraint(key string, node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	v := node.Value
	switch key {
	case "additionalProperties":
		if v == "false" {
			return "**Undeclared fields are rejected.** A field that is not in this schema is " +
				"a `400 validation_failed`."
		}
	case "pattern":
		return "Must match the pattern `" + v + "`."
	case "minimum":
		return "Must be at least `" + v + "`."
	case "maximum":
		return "Must be at most `" + v + "`."
	case "exclusiveMinimum":
		return "Must be greater than `" + v + "`."
	case "minLength":
		return "Must be at least " + v + " characters."
	case "maxLength":
		return "Must be at most " + v + " characters."
	case "minItems":
		return "Must have at least " + v + " entries."
	case "maxItems":
		return "Must have at most " + v + " entries."
	case "minProperties":
		return "Must have at least " + v + " fields."
	case "maxProperties":
		return "Must have at most " + v + " fields."
	case "uniqueItems":
		if v == "true" {
			return "Entries must be unique."
		}
	}
	return ""
}
