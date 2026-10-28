package main

import (
	"os"
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
