package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const catalogue = "| Row | What it proves | Test | Suite |\n| --- | --- | --- | --- |\n" +
	"| `T-01` | contention | `TestA`, `TestB` | integration |\n" +
	"| `T-13` | a full store | `TestC` | reference |\n"

func stream(lines ...string) *strings.Reader { return strings.NewReader(strings.Join(lines, "\n")) }

func TestEveryNamedTestThatPassedPasses(t *testing.T) {
	rows, err := parseCatalogue(catalogue)
	if err != nil {
		t.Fatal(err)
	}
	res, failed, err := readEvents(stream(
		`{"Action":"pass","Package":"p","Test":"TestA","Elapsed":0.5}`,
		`{"Action":"pass","Package":"p","Test":"TestB/sub","Elapsed":0.1}`,
		`{"Action":"pass","Package":"p","Test":"TestB","Elapsed":0.2}`,
	))
	if err != nil || len(failed) != 0 {
		t.Fatal(err, failed)
	}
	table, ok := report(rows, "integration", res)
	if !ok || strings.Count(table, "| pass |") != 2 || strings.Contains(table, "TestC") {
		t.Fatalf("ok %v:\n%s", ok, table)
	}
}

func TestANamedTestThatDidNotRunOrWasSkippedFails(t *testing.T) {
	rows, _ := parseCatalogue(catalogue)
	res, _, _ := readEvents(stream(`{"Action":"skip","Package":"p","Test":"TestA"}`))
	table, ok := report(rows, "integration", res)
	if ok || !strings.Contains(table, "**skip**") || !strings.Contains(table, "**did not run**") {
		t.Fatalf("ok %v:\n%s", ok, table)
	}
}

func TestAnyFailingTestIsReportedWithItsOutput(t *testing.T) {
	_, failed, _ := readEvents(stream(
		`{"Action":"output","Package":"p","Test":"TestZ","Output":"boom\n"}`,
		`{"Action":"fail","Package":"p","Test":"TestZ"}`,
	))
	if len(failed) != 1 || failed[0].name != "TestZ" || failed[0].output != "boom\n" {
		t.Fatalf("%+v", failed)
	}
}

func TestASuiteNoRowNamesFails(t *testing.T) {
	rows, _ := parseCatalogue(catalogue)
	if _, ok := report(rows, "nightly", nil); ok {
		t.Fatal("a suite with no rows passed")
	}
}

// TestTheCatalogueNamesAllFifteenRows reads the committed catalogue: every T-row of
// testing-strategy §2 has a test and a suite (IP-16 D14).
func TestTheCatalogueNamesAllFifteenRows(t *testing.T) {
	doc, err := os.ReadFile("../../docs/release/v0-1-evidence.md")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseCatalogue(string(doc))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Suite != "integration" && r.Suite != "reference" && r.Suite != "chaos" {
			t.Errorf("%s runs in suite %q, which no workflow reports", r.ID, r.Suite)
		}
		seen[r.ID] = true
	}
	for i := 1; i <= 15; i++ {
		id := "T-" + string(rune('0'+i/10)) + string(rune('0'+i%10))
		if !seen[id] {
			t.Errorf("%s has no row", id)
		}
	}
}

// TestEveryTestTheEvidenceNamesExists reads the whole evidence document, not
// only its catalogue: every test function it cites as proof is declared in some
// _test.go file, so a renamed or deleted test cannot leave a claim citing
// nothing (IP-16 D13).
func TestEveryTestTheEvidenceNamesExists(t *testing.T) {
	doc, err := os.ReadFile("../../docs/release/v0-1-evidence.md")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	decl := regexp.MustCompile(`(?m)^func (Test\w+)\(`)
	err = filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range decl.FindAllStringSubmatch(string(src), -1) {
			declared[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cited := regexp.MustCompile("`(Test\\w+)`").FindAllStringSubmatch(string(doc), -1)
	if len(cited) == 0 {
		t.Fatal("the evidence document names no test")
	}
	for _, m := range cited {
		if !declared[m[1]] {
			t.Errorf("the evidence document cites %s, which no _test.go declares", m[1])
		}
	}
}
