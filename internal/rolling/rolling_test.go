package rolling

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTheSevenWindows(t *testing.T) {
	want := map[string]time.Duration{
		"rolling_5m": 5 * time.Second, "rolling_15m": 15 * time.Second, "rolling_1h": time.Minute,
		"rolling_6h": 6 * time.Minute, "rolling_24h": 24 * time.Minute, "rolling_7d": 168 * time.Minute,
		"rolling_30d": 12 * time.Hour,
	}
	if len(All()) != len(want) {
		t.Fatalf("%d windows, want %d", len(All()), len(want))
	}
	for _, w := range All() {
		if w.Precision() != want[w.Name()] {
			t.Errorf("%s: precision %v, want %v", w.Name(), w.Precision(), want[w.Name()])
		}
		if w.Precision()%time.Second != 0 || w.Duration()%Buckets != 0 {
			t.Errorf("%s: a bucket is not a whole number of seconds", w.Name())
		}
		if p, ok := Parse(w.Name()); !ok || p != w {
			t.Errorf("Parse(%s) = %v, %t", w.Name(), p, ok)
		}
		if p, ok := FromSeconds(w.Seconds()); !ok || p != w {
			t.Errorf("FromSeconds(%d) = %v, %t", w.Seconds(), p, ok)
		}
	}
	for _, bad := range []string{"", "rolling_2h", "rolling_1H", "1h", "hourly"} {
		if _, ok := Parse(bad); ok {
			t.Errorf("Parse(%q) accepted a window outside the supported set", bad)
		}
	}
	if _, ok := FromSeconds(7200); ok {
		t.Error("FromSeconds(7200) accepted a window outside the supported set")
	}
}

// TestNoRollingCodeImportsTheCycleEngine is the import half of IP-19 DoD 3: the
// packages that carry the rolling cadence never import internal/cycle, so the
// second cadence cannot reach into the calendar engine (cycle-engine.md
// section 9, ADR-0008).
func TestNoRollingCodeImportsTheCycleEngine(t *testing.T) {
	for _, dir := range []string{"."} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				if strings.Contains(imp.Path.Value, "internal/cycle") {
					t.Errorf("%s imports %s; rolling windows share no rule with the calendar engine", e.Name(), imp.Path.Value)
				}
			}
		}
	}
}
