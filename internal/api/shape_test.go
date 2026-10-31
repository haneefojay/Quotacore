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
//
// `internal/auth`, `internal/envelope` and `internal/dataplane` are here
// because IP-07 owns them, and IP-07 is where product behaviour reaches HTTP.
// `internal/auth` verifies a key and decides nothing about a balance.
// `internal/envelope` writes the one error shape and reads the status of each
// code from the contract. `internal/dataplane` is the four routes: it decides
// which refusal a request gets before the script and maps what the script
// decided, but the decision about a balance is still the script's alone, so
// the symbol rules below hold for it unchanged.
//
// `internal/admin` is here because IP-09 owns it: the control-plane routes,
// which create, archive, suspend and delete configuration and keys. It writes
// the opening balance of a new tenant from the window the engine computes and
// decides nothing about a request's balance, so the symbol rules hold for it
// unchanged, and it reaches Postgres only through internal/db.
//
// `internal/reconcile` is here because IP-11 owns it: the verifier's
// arithmetic, the in-service pass and the rebuild's classification. It does
// no I/O of its own and imports no store, script or SQL package (its own
// source check), so the one balance it can move is the reversal's refund,
// which it hands to the data plane's script.
//
// `internal/cli` is here because IP-14 owns it: the administration commands.
// It is a client of the contract over HTTP and imports no store, script or
// SQL package, so it can decide nothing about a balance; the symbol rules
// hold for it unchanged.
//
// `test/reference` and `tools/idemsize` are here because IP-15 owns them. The
// first is the suite against the Compose stack: it drives the built image over
// HTTP and the CLI and holds no code the service runs. The second measures what
// an idempotency record costs by calling the real consume script through
// internal/script, so the symbol rules hold for it unchanged: it decides
// nothing, and it runs only against a scratch store it refuses to touch unless
// empty.
//
// `tools/testreport` and `internal/worker` are here because IP-16 owns them.
// The first reads go test's JSON stream and reports the named correctness
// tests, and imports nothing of the product. The second is the reset worker
// (Q-13): it reads the snapshot's cached tenants and calls the data plane's
// Advance, which runs the request path's transition, so it computes windows
// with internal/cycle and decides nothing a request would not; its source
// check holds it off SQL, the store and the scripts (INV-WK2). `test/harness`,
// `test/load` and `test/chaos` are IP-16's suites against the Compose stack,
// as `test/reference` is: they drive the built image over HTTP, docker and the
// stores' own clients, and hold no code the service runs. The name of this test is kept
// for its history; what it guards now is that the package set is exactly the
// set the phases own, and that the calendar and the script stay single.
func TestNoProductBehaviour(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this source file")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))

	wantPackages := map[string]bool{
		"api":                true,
		"internal/admin":     true,
		"cmd/quotacore":      true,
		"internal/api":       true,
		"internal/auth":      true,
		"internal/cli":       true,
		"internal/cycle":     true,
		"internal/dataplane": true,
		"internal/envelope":  true,
		"internal/ledger":    true,
		"internal/reconcile": true,
		// IP-19's: the seven rolling windows, which import nothing of the
		// calendar's (DR-052, ADR-0008).
		"internal/rolling":       true,
		"internal/db":            true,
		"internal/observability": true,
		"internal/snapshot":      true,
		"internal/script":        true,
		"internal/store":         true,
		// IP-21's: the webhook secret, signature, payload and matching, with
		// no network code (DR-055, ADR-0019).
		"internal/webhook": true,
		"tools/gendocs":    true,
		"tools/idemsize":   true,
		"tools/testreport": true,
		"internal/worker":  true,
		"test/reference":   true,
		"test/harness":     true,
		"test/load":        true,
		"test/chaos":       true,
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
