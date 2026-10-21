package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/quotacore/quotacore/internal/envelope"
)

// IP-13 DoD 6 and 7 (D9, D12): the alert rules are observability.md section
// 5, each reads served metrics and each P0 and P1 names a runbook; every
// runbook has the same headings, names only commands the binary has and
// routes the contract has, and answers an alert or a section 6 trigger.

type alertRule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

func alertRules(t *testing.T) []alertRule {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "ops", "alerts", "quotacore.rules.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Groups []struct {
			Rules []alertRule `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var out []alertRule
	for _, g := range doc.Groups {
		out = append(out, g.Rules...)
	}
	return out
}

var alertRow = regexp.MustCompile("^\\| ([^|`]+?(?:`[^`]+`[^|]*)?) \\| (.+) \\| (P[0-3]) \\| ")

// documentedAlerts is section 5's table: each row's name, run together as
// the rule names it, and its severity.
func documentedAlerts(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "architecture", "observability.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	section := text[strings.Index(text, "\n## 5. Alerts"):strings.Index(text, "\n## 6. ")]
	out := map[string]string{}
	for _, line := range strings.Split(section, "\n") {
		if m := alertRow.FindStringSubmatch(line); m != nil {
			out[ruleName(m[1])] = m[3]
		}
	}
	if len(out) < 20 {
		t.Fatalf("read %d alert rows from section 5", len(out))
	}
	return out
}

// ruleName runs a row's words together: "Data store memory approaching
// `maxmemory`" is DataStoreMemoryApproachingMaxmemory.
func ruleName(row string) string {
	var b strings.Builder
	for _, w := range strings.FieldsFunc(row, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		b.WriteString(strings.ToUpper(w[:1]) + w[1:])
	}
	return b.String()
}

var exprMetric = regexp.MustCompile(`quotacore_[a-z0-9_]+`)

// exprMetrics is every family an expression reads, a histogram's _bucket
// read as the histogram.
func exprMetrics(expr string, documented map[string][]string) []string {
	var out []string
	for _, m := range exprMetric.FindAllString(expr, -1) {
		if _, ok := documented[m]; !ok {
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				if base, cut := strings.CutSuffix(m, suffix); cut {
					if _, ok := documented[base]; ok {
						m = base
					}
				}
			}
		}
		out = append(out, m)
	}
	return out
}

// anchorOf is the anchor GitHub and check-docs give a heading.
func anchorOf(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

func headings(t *testing.T, path string) (map[string]bool, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if h, ok := strings.CutPrefix(line, "## "); ok {
			out[anchorOf(h)] = true
		}
	}
	return out, string(raw)
}

// TestEveryAlertIsTheDocumentsAndNamesARunbook (IP-13 DoD 6, D9).
func TestEveryAlertIsTheDocumentsAndNamesARunbook(t *testing.T) {
	rows := documentedAlerts(t)
	metrics := documentedMetrics(t)
	seen := map[string]bool{}
	for _, r := range alertRules(t) {
		seen[r.Alert] = true
		sev, ok := rows[r.Alert]
		switch {
		case !ok:
			t.Errorf("rule %s has no row in observability.md section 5", r.Alert)
		case r.Labels["severity"] != sev:
			t.Errorf("rule %s is %s and its row says %s", r.Alert, r.Labels["severity"], sev)
		}
		if r.Annotations["summary"] == "" {
			t.Errorf("rule %s has no summary", r.Alert)
		}
		names := exprMetrics(r.Expr, metrics)
		if len(names) == 0 {
			t.Errorf("rule %s reads no metric", r.Alert)
		}
		for _, m := range names {
			if _, ok := metrics[m]; !ok {
				t.Errorf("rule %s reads %s, which observability.md section 1 does not name", r.Alert, m)
			}
		}
		book := r.Annotations["runbook"]
		if (sev == "P0" || sev == "P1") && book == "" {
			t.Errorf("rule %s is %s and names no runbook", r.Alert, sev)
		}
		if book != "" {
			file, anchor, _ := strings.Cut(book, "#")
			hs, _ := headings(t, filepath.Join("..", "..", filepath.FromSlash(file)))
			if !hs[anchor] {
				t.Errorf("rule %s names %s, whose anchor #%s does not exist", r.Alert, book, anchor)
			}
		}
	}
	for name := range rows {
		if !seen[name] {
			t.Errorf("section 5's row %s has no rule", name)
		}
	}
}

// TestEveryAlertReadsAServedMetric (IP-13 DoD 6): every family a rule reads
// is in an assembled stack's scrape.
func TestEveryAlertReadsAServedMetric(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), 200)
	all, _ := s.scrape()
	served := map[string]bool{}
	for _, sr := range all {
		served[sr.family] = true
	}
	metrics := documentedMetrics(t)
	for _, r := range alertRules(t) {
		for _, m := range exprMetrics(r.Expr, metrics) {
			if !served[m] {
				t.Errorf("rule %s reads %s, which the stack does not serve", r.Alert, m)
			}
		}
	}
}

// The headings every runbook has, in order.
var runbookHeadings = []string{"Trigger", "Impact", "Check", "Decide", "Act", "Verify", "Escalate", "Source"}

var (
	commandUse = regexp.MustCompile("quotacore ([a-z][a-z-]+)")
	curlUse    = regexp.MustCompile(`curl [^\n]*-X ([A-Z]+) [^\n]*"\$QC_URL(/[^"?]*)`)
	indexRow   = regexp.MustCompile(`\| \[[^\]]+\]\(([a-z-]+\.md)\) \| ([^|]+) \|`)
)

// TestEveryRunbookIsFollowable (IP-13 DoD 7, D12).
func TestEveryRunbookIsFollowable(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "runbooks")
	_, index := headings(t, filepath.Join(dir, "README.md"))
	books := map[string]string{}
	for _, m := range indexRow.FindAllStringSubmatch(index, -1) {
		books[m[1]] = m[2]
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	param := regexp.MustCompile(`\\\{[a-z_]+\\\}`)
	isRoute := func(method, path string) bool {
		for _, op := range envelope.MustOperations() {
			pattern := "^" + param.ReplaceAllString(regexp.QuoteMeta(op.Path), `[^/]+`) + "$"
			if op.Method == method && regexp.MustCompile(pattern).MatchString(path) {
				return true
			}
		}
		return false
	}
	for _, e := range entries {
		if e.Name() == "README.md" || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		files++
		if books[e.Name()] == "" {
			t.Errorf("%s is not in the runbook index", e.Name())
		}
		hs, text := headings(t, filepath.Join(dir, e.Name()))
		last := -1
		for _, h := range runbookHeadings {
			if !hs[anchorOf(h)] {
				t.Errorf("%s has no ## %s", e.Name(), h)
				continue
			}
			at := strings.Index(text, "\n## "+h+"\n")
			if at < last {
				t.Errorf("%s has ## %s out of order", e.Name(), h)
			}
			last = at
		}
		for _, m := range commandUse.FindAllStringSubmatch(text, -1) {
			if !isCommand([]string{m[1]}) {
				t.Errorf("%s runs quotacore %s, which the binary does not have", e.Name(), m[1])
			}
		}
		for _, m := range curlUse.FindAllStringSubmatch(text, -1) {
			if !isRoute(m[1], m[2]) {
				t.Errorf("%s calls %s %s, which is not a contract route", e.Name(), m[1], m[2])
			}
		}
		if !strings.Contains(text, "curl") && !commandUse.MatchString(text) {
			t.Errorf("%s has no command to run", e.Name())
		}
	}
	if files != len(books) || files < 11 {
		t.Errorf("%d runbook files and %d index rows, want the same and at least eleven", files, len(books))
	}
	// Every alert's runbook is a runbook, and every runbook answers an alert or
	// a section 6 trigger, which the index names.
	for _, r := range alertRules(t) {
		if book := r.Annotations["runbook"]; book != "" {
			file, _, _ := strings.Cut(strings.TrimPrefix(book, "docs/runbooks/"), "#")
			if books[file] == "" {
				t.Errorf("rule %s names %s, which the index does not list", r.Alert, book)
			}
		}
	}
	for file, trigger := range books {
		if strings.TrimSpace(trigger) == "" {
			t.Errorf("%s has no trigger in the index", file)
		}
	}
}
