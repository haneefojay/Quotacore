// Command testreport reads `go test -json` on standard input and reports the named correctness
// tests, T-01 to T-15, by name (IP-16 DoD 1, D12).
//
// The catalogue is the table of named tests in docs/release/v0-1-evidence.md: each row names a
// T-row, the test functions that prove it, and the suite those functions run in. testreport
// checks the rows of the suite it is given. A named test that did not run, was skipped or failed
// fails the report, so a T-row cannot lose its test silently, and a test renamed without the
// catalogue fails the same way. Any other failing test in the stream fails it too, with its
// output, because the stream has replaced go test's own.
//
// The table goes to $GITHUB_STEP_SUMMARY when that is set, and to standard output always.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

func main() {
	catalogue := flag.String("catalogue", "docs/release/v0-1-evidence.md", "the document holding the named-test table")
	suite := flag.String("suite", "", "the suite whose rows this run must prove, as the table's Suite column names it")
	flag.Parse()
	if *suite == "" {
		fatal(errors.New("-suite is required"))
	}
	doc, err := os.ReadFile(*catalogue)
	if err != nil {
		fatal(err)
	}
	rows, err := parseCatalogue(string(doc))
	if err != nil {
		fatal(err)
	}
	results, failed, err := readEvents(os.Stdin)
	if err != nil {
		fatal(err)
	}
	table, ok := report(rows, *suite, results)
	fmt.Print(table)
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
			fmt.Fprint(f, table)
			f.Close()
		}
	}
	for _, f := range failed {
		fmt.Printf("\n--- FAIL: %s (%s)\n%s", f.name, f.pkg, f.output)
	}
	if !ok || len(failed) > 0 {
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "testreport:", err)
	os.Exit(2)
}

// row is one T-row of the catalogue.
type row struct {
	ID    string
	Tests []string
	Suite string
}

var (
	rowID    = regexp.MustCompile("^`(T-\\d{2})`$")
	testName = regexp.MustCompile("`(Test[A-Za-z0-9_]+)`")
)

// parseCatalogue reads every table row whose first cell is a T-row: | `T-nn` | … | `TestX` … | suite |.
// The tests are the third cell's, and the suite is the last cell.
func parseCatalogue(doc string) ([]row, error) {
	var rows []row
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(line, "| `T-") {
			continue
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) < 4 {
			return nil, fmt.Errorf("a named-test row with %d cells: %s", len(cells), line)
		}
		m := rowID.FindStringSubmatch(strings.TrimSpace(cells[0]))
		if m == nil {
			continue
		}
		r := row{ID: m[1], Suite: strings.TrimSpace(cells[len(cells)-1])}
		for _, t := range testName.FindAllStringSubmatch(cells[2], -1) {
			r.Tests = append(r.Tests, t[1])
		}
		if len(r.Tests) == 0 {
			return nil, fmt.Errorf("%s names no test", r.ID)
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return nil, errors.New("the catalogue has no named-test rows")
	}
	return rows, nil
}

// event is one line of go test -json.
type event struct {
	Action  string
	Package string
	Test    string
	Elapsed float64
	Output  string
}

// result is a top-level test's outcome, by name.
type result struct {
	Action  string
	Package string
	Elapsed time.Duration
}

type failure struct {
	name, pkg, output string
}

// readEvents collects each top-level test's last outcome, and the output of every test that
// failed. A subtest's outcome is folded into its parent's, which go test reports anyway.
func readEvents(r io.Reader) (map[string]result, []failure, error) {
	results := map[string]result{}
	output := map[string]*strings.Builder{}
	var failed []failure
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			// go test prints build failures as plain text; keep them visible.
			fmt.Fprintln(os.Stderr, sc.Text())
			continue
		}
		key := e.Package + " " + e.Test
		switch e.Action {
		case "output":
			b := output[key]
			if b == nil {
				b = &strings.Builder{}
				output[key] = b
			}
			b.WriteString(e.Output)
		case "pass", "fail", "skip":
			if e.Test == "" {
				if e.Action == "fail" {
					var out string
					if b := output[key]; b != nil {
						out = b.String()
					}
					failed = append(failed, failure{name: "(package)", pkg: e.Package, output: out})
				}
				continue
			}
			if strings.Contains(e.Test, "/") {
				continue
			}
			results[e.Test] = result{Action: e.Action, Package: e.Package, Elapsed: time.Duration(e.Elapsed * float64(time.Second))}
			if e.Action == "fail" {
				var out string
				if b := output[key]; b != nil {
					out = b.String()
				}
				failed = append(failed, failure{name: e.Test, pkg: e.Package, output: out})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	// A package that failed only because a test did repeats that test; keep the test.
	sort.SliceStable(failed, func(i, j int) bool { return failed[i].name != "(package)" && failed[j].name == "(package)" })
	return results, failed, nil
}

// report renders the suite's rows and says whether every named test passed.
func report(rows []row, suite string, results map[string]result) (string, bool) {
	var b strings.Builder
	ok := true
	fmt.Fprintf(&b, "### Named correctness tests, suite `%s`\n\n| Row | Test | Package | Result | Time |\n| --- | --- | --- | --- | --- |\n", suite)
	n := 0
	for _, r := range rows {
		if r.Suite != suite {
			continue
		}
		for _, t := range r.Tests {
			n++
			res, ran := results[t]
			verdict := res.Action
			switch {
			case !ran:
				verdict, ok = "**did not run**", false
			case res.Action != "pass":
				verdict, ok = "**"+res.Action+"**", false
			}
			pkg := strings.TrimPrefix(res.Package, "github.com/quotacore/quotacore/")
			fmt.Fprintf(&b, "| %s | `%s` | %s | %s | %s |\n", r.ID, t, pkg, verdict, res.Elapsed.Round(10*time.Millisecond))
		}
	}
	if n == 0 {
		fmt.Fprintf(&b, "| — | no row names suite `%s` | | **fail** | |\n", suite)
		ok = false
	}
	b.WriteString("\n")
	return b.String(), ok
}
