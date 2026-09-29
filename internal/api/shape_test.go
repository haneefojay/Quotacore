package api

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// DoD item 6 for IP-01, kept true by IP-02: no package contains product
// behaviour. The route half is asserted by TestRouterServesOnlyTheTwoHealthProbes.
// This asserts the other half as a source-level check, which is the form
// testing-strategy.md section 4 blesses for a property that has no other
// observable: the package set is exactly the five this phase owns, and no file
// in the module mentions the arithmetic the product exists to perform.
//
// The `api` package is here because IP-02 owns it. It holds ogen's generated
// types, JSON codecs and request validators, which is the opposite of product
// behaviour: a validator that rejects a malformed quota request is not the
// arithmetic that decides whether the request is allowed. What `api` must not
// contain is anything that *decides* anything, and the ogen config enforces
// that by disabling every router, client and unimplemented-handler feature.
//
// `tools/gendocs` is here because IP-02 owns it too. It renders the contract
// into the page at /docs, which is a rendering of a document rather than a
// decision about a request, and it is the same code the service's test suite
// runs when it checks the committed page is not stale. A documentation generator
// is not a place product behaviour can grow, and keeping it in its own command
// is what makes that true: it has no route, no store and no configuration.
//
// `internal/cycle` is here because IP-03 owns it. It is the pure boundary
// engine: a function from an anchor, an interval and a zone to a cycle window,
// which is arithmetic, not enforcement, and it is deliberately the only package
// that may perform cycle arithmetic (cycle-engine.md §4: "no second
// implementation"). cycleScopedSymbols captures that rule at the source level —
// the engine's vocabulary is allowed inside `internal/cycle` and nowhere else —
// while balance, idempotency and script symbols remain forbidden everywhere,
// including inside the engine.
//
// `internal/store`, `internal/snapshot` and `internal/observability` are here
// because IP-04 owns them. `internal/store` is the keyspace (the key layout that
// data-model.md section 3.1 fixes) and its connection pools, nothing that
// decides a request. `internal/snapshot` is the bounded projection of
// configuration the data plane resolves against (DR-039), which decides nothing
// about a balance. `internal/observability` is the seven metric collectors of
// observability.md section 1.4, which is bookkeeping, not behaviour, and which
// IP-13 mounts a route for. None of the three may mention a balancing symbol,
// and the source checks in `internal/store` extend the same property to the
// datastore cursor and control-plane import rules.
//
// `internal/script` is here because IP-05 owns it, and it is the one package
// that is allowed to contain product behaviour: the three Lua scripts that decide
// and apply a balance change in one execution, and the Go code that loads and runs
// them. It is allowed to contain exactly that and nothing else, which is what
// scriptScopedSymbols asserts - the vocabulary of running a script may appear
// inside `internal/script` and nowhere else. The rule this package is built to
// keep is the narrower one: the calendar has exactly one implementation. The
// cycle symbols stay forbidden everywhere, including in here, because a second
// boundary calculation inside a script would be a second authority over when a
// tenant's allowance resets, and the scripts instead take the window the caller
// already computed and are handed it.
func TestNoProductBehaviour(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this source file")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))

	wantPackages := map[string]bool{
		"api":                    true,
		"cmd/quotacore":          true,
		"internal/api":           true,
		"internal/cycle":         true,
		"internal/db":            true,
		"internal/observability": true,
		"internal/snapshot":      true,
		"internal/script":        true,
		"internal/store":         true,
		"tools/gendocs":          true,
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

	// The calendar has one implementation and that is this package's, so these
	// four are forbidden in every file including internal/cycle, because a
	// second boundary calculation anywhere would be a second authority over when
	// a tenant's allowance resets (cycle-engine.md §4).
	productSymbols := []string{
		"cycleIndex", "CycleAnchor", "RollCycle", "AdvanceCycle",
		"balanceRemaining", "DeductBalance", "CheckAndDeduct",
		"idempotencyKey",
	}
	// The engine's own arithmetic vocabulary, which may live only in
	// internal/cycle. A second implementation of boundary arithmetic anywhere
	// else would be a second authority over the calendar, which is the one
	// thing this phase exists to keep single (cycle-engine.md §4).
	cycleScopedSymbols := []string{
		"currentIndex", "addMonths", "addDays", "addYears",
	}
	// The vocabulary of running an embedded script, which may live only in
	// internal/script. Running a script is not product behaviour on its own - the
	// behaviour is in the Lua, which this scan does not read because it is not Go
	// - so the Go half of the package is the half this list constrains. Two
	// symbols are deliberately absent from it: the names a balance operation
	// could be given, `DeductBalance` and `CheckAndDeduct`, stay forbidden
	// everywhere, because the operations are called Consume, Refund and Check and
	// a second spelling of one of them is a second name for the same decision.
	// `ScriptSha` was moved here from the everywhere-forbidden list for the same
	// reason - addressing a script by its digest is script vocabulary, not a
	// second balance decision - and it is kept so the name cannot appear outside.
	scriptScopedSymbols := []string{
		"EVALSHA", "EvalSha", "ScriptSha",
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
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, symbol := range productSymbols {
			if strings.Contains(string(body), symbol) {
				t.Errorf("%s mentions %s, which is product behaviour", rel, symbol)
			}
		}
		if !strings.HasPrefix(rel, "internal/cycle") {
			for _, symbol := range cycleScopedSymbols {
				if strings.Contains(string(body), symbol) {
					t.Errorf("%s mentions %s, which may only appear inside internal/cycle", rel, symbol)
				}
			}
		}
		if !strings.HasPrefix(rel, "internal/script") {
			for _, symbol := range scriptScopedSymbols {
				if strings.Contains(string(body), symbol) {
					t.Errorf("%s mentions %s, which may only appear inside internal/script", rel, symbol)
				}
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
