package api

import (
	"bytes"
	"encoding/json"
	"html"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The page at /docs is a rendering of the contract, and a rendering can be wrong in ways the
// source cannot: it can drop a route, it can link to a section that is not there, and it can
// quietly start loading something from the network. Each of those is a test here.

func TestRenderDocsRejectsAnUnusableDocument(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"not yaml":               "\tthis: [is not",
		"empty":                  "",
		"no paths at all":        "openapi: 3.1.0\ninfo:\n  title: x\n",
		"an empty paths mapping": "openapi: 3.1.0\npaths: {}\n",
		"a path with no method":  "openapi: 3.1.0\npaths:\n  /thing:\n    summary: no method here\n",
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			page, err := RenderDocs([]byte(document))
			if err == nil {
				t.Fatalf("rendering succeeded, so the generator would write an unusable page: %d bytes",
					len(page))
			}
		})
	}
}

// TestRenderDocsIsDeterministic is the property every other test here leans on. A page that
// differed between runs could not be committed, could not be diffed, and could not be checked.
func TestRenderDocsIsDeterministic(t *testing.T) {
	t.Parallel()

	first, err := RenderDocs(Document)
	if err != nil {
		t.Fatalf("render the page: %v", err)
	}
	second, err := RenderDocs(Document)
	if err != nil {
		t.Fatalf("render the page again: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("two renderings of the same contract differ")
	}
}

// TestDocsPageIsUpToDate is the drift check in test form. CI runs the same comparison through
// `make docs-check`; having it here as well means a developer sees the failure before pushing
// rather than after.
func TestDocsPageIsUpToDate(t *testing.T) {
	t.Parallel()

	generated, err := RenderDocs(Document)
	if err != nil {
		t.Fatalf("render the page: %v", err)
	}
	if !bytes.Equal(DocsPage, generated) {
		t.Errorf("api/docs.html differs from the page generated from api/openapi.yaml; "+
			"run `make docs` and commit the result (%d committed bytes against %d generated)",
			len(DocsPage), len(generated))
	}
}

// TestDocsPageIsSelfContained is the claim the page itself makes, and the reason the renderer has
// no documentation library. A reference that needs a CDN is a reference that does not work on the
// host this product runs on.
func TestDocsPageIsSelfContained(t *testing.T) {
	t.Parallel()

	forbidden := []struct {
		needle string
		why    string
	}{
		{"<script", "a script tag, which needs a runtime this page deliberately does not have"},
		{"<link ", "a link element, which is how a stylesheet is loaded from somewhere else"},
		{"<iframe", "a frame, which is a page loading another page"},
		{"<img", "an image, which is a request to another host"},
		{"@import", "a CSS import, which is a request to another host"},
		{"src=", "any src attribute, which is a request to another host"},
		{"http://", "an absolute http URL"},
		{"https://", "an absolute https URL"},
	}
	for _, f := range forbidden {
		if bytes.Contains(DocsPage, []byte(f.needle)) {
			t.Errorf("the page contains %q: %s", f.needle, f.why)
		}
	}
}

// TestDocsPageAnchorsResolve is the test that stops a page linking to itself wrongly. Every
// fragment link is checked against the set of ids the page actually defines, so a renamed schema
// cannot leave forty dead links behind it.
func TestDocsPageAnchorsResolve(t *testing.T) {
	t.Parallel()

	ids := map[string]bool{}
	for _, match := range regexp.MustCompile(`\sid="([^"]+)"`).FindAllStringSubmatch(string(DocsPage), -1) {
		ids[match[1]] = true
	}
	if len(ids) == 0 {
		t.Fatal("the page defines no ids, so it is not the page the generator writes")
	}
	links := 0
	for _, match := range regexp.MustCompile(`href="#([^"]+)"`).FindAllStringSubmatch(string(DocsPage), -1) {
		links++
		if !ids[match[1]] {
			t.Errorf("the page links to #%s, which it does not define", match[1])
		}
	}
	if links == 0 {
		t.Error("the page has no internal links at all, so the contents list did not render")
	}
}

// TestDocsPageShowsEveryRoute is the anti-omission test. A page that quietly drops one route is
// the failure mode of every documentation generator, and the operator who needs that route is the
// one who does not notice.
func TestDocsPageShowsEveryRoute(t *testing.T) {
	t.Parallel()

	m := parseDocModel(t, Document)
	operations := readOperations(&m.Paths, newDocRefs(&m), &m.Security)
	if len(operations) == 0 {
		t.Fatal("the contract yielded no operations, so this test would pass vacuously")
	}
	page := string(DocsPage)
	for _, op := range operations {
		marker := `<span class="path">` + op.Path + `</span>`
		// The contents list and the body each name the path, so two is the expected count for
		// every route. The point is that it is not one, which is what a dropped body looks like.
		if got := strings.Count(page, marker); got < 2 {
			t.Errorf("%s %s appears %d times on the page, expected at least 2",
				strings.ToUpper(op.Method), op.Path, got)
		}
		if op.OperationID != "" && !strings.Contains(page, "<code>"+op.OperationID+"</code>") {
			t.Errorf("the page does not mention operationId %q", op.OperationID)
		}
		if op.Summary != "" && !strings.Contains(page, html.EscapeString(op.Summary)) {
			t.Errorf("the page does not show the summary of %s %s", strings.ToUpper(op.Method), op.Path)
		}
	}
}

// TestDocsPageShowsEverySchema is the same anti-omission test for types. A schema reachable only
// from another schema must still be documented, or a reader follows a link into a page that ends.
func TestDocsPageShowsEverySchema(t *testing.T) {
	t.Parallel()

	m := parseDocModel(t, Document)
	names := make([]string, 0, len(m.Components.Schemas.Content)/2)
	for i := 0; i+1 < len(m.Components.Schemas.Content); i += 2 {
		names = append(names, m.Components.Schemas.Content[i].Value)
	}
	if len(names) == 0 {
		t.Fatal("the contract defines no schemas, so this test would pass vacuously")
	}
	sort.Strings(names)
	page := string(DocsPage)
	for _, name := range names {
		if !strings.Contains(page, `id="schema-`+slug(name)+`"`) {
			t.Errorf("the page has no section for schema %q", name)
		}
	}
}

// TestDocsPageRendersExamplesAsJSON checks that every example block in the page is parseable JSON.
// An example that does not parse is the most damaging thing a reference can contain, because it
// looks copy-pasteable and is not.
func TestDocsPageRendersExamplesAsJSON(t *testing.T) {
	t.Parallel()

	blocks := regexp.MustCompile(`(?s)<pre><code>(.*?)</code></pre>`).FindAllSubmatch(DocsPage, -1)
	if len(blocks) == 0 {
		t.Fatal("the page contains no example blocks, so the renderer is not producing them")
	}
	checked := 0
	for _, block := range blocks {
		body := html.UnescapeString(string(block[1]))
		if !strings.HasPrefix(strings.TrimSpace(body), "{") {
			continue
		}
		checked++
		if !json.Valid([]byte(body)) {
			t.Errorf("an example on the page is not parseable JSON:\n%s", body)
		}
	}
	if checked == 0 {
		t.Error("every example on the page was a non-object, so nothing was actually checked")
	}
}

// TestDocsPageEscapesContractProse is the safety test. The contract is a file in the repository, so
// it is not attacker-controlled today, but a description is prose that will one day contain a URL
// or a snippet copied from somewhere else, and a page that renders that raw is one paste away from
// being one.
func TestDocsPageEscapesContractProse(t *testing.T) {
	t.Parallel()

	hostile := "openapi: 3.1.0\n" +
		"info:\n" +
		"  title: Escaping\n" +
		"  version: 0.0.0\n" +
		"paths:\n" +
		"  /thing:\n" +
		"    get:\n" +
		"      operationId: thing\n" +
		"      summary: <script>alert(1)</script>\n" +
		"      description: |\n" +
		"        Prose with <script>alert(2)</script> and a & in it.\n" +
		"      responses:\n" +
		"        '200':\n" +
		"          description: Also <script>alert(3)</script>\n"
	page, err := RenderDocs([]byte(hostile))
	if err != nil {
		t.Fatalf("render the page: %v", err)
	}
	if bytes.Contains(page, []byte("<script>")) {
		t.Error("the page emitted a script tag from contract prose")
	}
	for _, want := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "&amp;"} {
		if !bytes.Contains(page, []byte(want)) {
			t.Errorf("the page does not contain the escaped form %q", want)
		}
	}
}

// TestDocsPageNamesTheSource keeps the page honest about what it is. A reader who has found a
// contradiction needs to know which file to fix, and a page that does not say is a dead end.
func TestDocsPageNamesTheSource(t *testing.T) {
	t.Parallel()

	page := string(DocsPage)
	for _, needle := range []string{"api/openapi.yaml", "make docs", "<!DOCTYPE html>", "</html>"} {
		if !strings.Contains(page, needle) {
			t.Errorf("the page does not mention %q", needle)
		}
	}
}

// TestDocsPageShowsEachOperationOnce guards the duplication bug this renderer actually had: the
// declared tag list and the discovered tag list were both appended to the page order, so every
// operation was rendered twice. A duplicated section is not a cosmetic problem, because the
// contents list then points at an id that appears twice and a reader cannot tell which half is
// current.
func TestDocsPageShowsEachOperationOnce(t *testing.T) {
	t.Parallel()

	page := string(DocsPage)
	for _, match := range regexp.MustCompile(`<h3><span class="method method-[a-z]+">([A-Z]+)</span> <span class="path">([^<]+)</span></h3>`).
		FindAllStringSubmatch(page, -1) {
		if got := strings.Count(page, match[0]); got > 1 {
			t.Errorf("%s %s is documented more than once", match[1], match[2])
		}
	}
}

// TestTagHeadingsAppearOnce applies the same check to the group headings.
func TestTagHeadingsAppearOnce(t *testing.T) {
	t.Parallel()

	page := string(DocsPage)
	for _, match := range regexp.MustCompile(`<h2 id="tag-([^"]+)">`).FindAllStringSubmatch(page, -1) {
		heading := `<h2 id="tag-` + match[1] + `">`
		if got := strings.Count(page, heading); got != 1 {
			t.Errorf("the heading for tag %s appears %d times, expected 1", match[1], got)
		}
	}
}

// TestDocsPageCarriesTheContractVersion is the first thing a reader checks to know whether the
// page describes the binary they are running.
func TestDocsPageCarriesTheContractVersion(t *testing.T) {
	t.Parallel()

	m := parseDocModel(t, Document)
	if m.Info.Title == "" {
		t.Error("the contract has no title, so the page has no heading")
	}
	if m.Info.Version == "" {
		t.Error("the contract has no version, so the page cannot say which one it is")
	}
	page := string(DocsPage)
	if !strings.Contains(page, html.EscapeString(m.Info.Title)) {
		t.Error("the page does not carry the contract title")
	}
	if !strings.Contains(page, html.EscapeString(m.Info.Version)) {
		t.Error("the page does not carry the contract version")
	}
}

func parseDocModel(t *testing.T, document []byte) docModel {
	t.Helper()
	var m docModel
	if err := yaml.Unmarshal(document, &m); err != nil {
		t.Fatalf("parse the contract: %v", err)
	}
	return m
}
