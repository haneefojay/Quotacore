package api

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// DoD item 6 for IP-01: no package contains product behaviour. The route half
// is asserted by TestRouterServesOnlyTheTwoHealthProbes. This asserts the
// other half as a source-level check, which is the form testing-strategy.md
// section 4 blesses for a property that has no other observable: the package
// set is exactly the three this phase owns, and no file in the module mentions
// the arithmetic the product exists to perform.
func TestNoProductBehaviour(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this source file")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))

	wantPackages := map[string]bool{
		"cmd/quotacore": true,
		"internal/api":  true,
		"internal/db":   true,
	}
	gotPackages := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel != "." && (strings.HasPrefix(rel, ".") || rel == "docs" || rel == "bin" || rel == "dist") {
			return filepath.SkipDir
		}
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			return readErr
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			gotPackages[rel] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	if len(gotPackages) != len(wantPackages) {
		t.Errorf("packages = %v, want exactly %v", keys(gotPackages), keys(wantPackages))
	}
	for p := range wantPackages {
		if !gotPackages[p] {
			t.Errorf("package %s is missing", p)
		}
	}
	for p := range gotPackages {
		if !wantPackages[p] {
			t.Errorf("package %s exists, and this phase does not own it", p)
		}
	}

	productSymbols := []string{
		"cycleIndex", "CycleAnchor", "RollCycle", "AdvanceCycle",
		"balanceRemaining", "DeductBalance", "CheckAndDeduct",
		"idempotencyKey", "EVALSHA", "EvalSha", "ScriptSha",
	}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(rel)
			if rel != "." && (strings.HasPrefix(rel, ".") || rel == "docs" || rel == "bin" || rel == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if sameFile(path, file) {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, symbol := range productSymbols {
			if strings.Contains(string(body), symbol) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s mentions %s, which is product behaviour", filepath.ToSlash(rel), symbol)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// sameFile compares two paths after resolving them, so the file that names the
// forbidden symbols is not failed for naming them.
func sameFile(a, b string) bool {
	ra, errA := filepath.Abs(a)
	rb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	return filepath.Clean(ra) == filepath.Clean(rb)
}
