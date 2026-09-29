package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The two source-level checks of IP-04's Definition of Done live beside the
// keyspace for the same reason shape_test.go exists in internal/api: the
// property has no other observable, so the check is a scan rather than a test,
// and a scan that is only a code review is not a scan. Both checks are the form
// testing-strategy.md section 4 blesses for exactly this kind of rule.
//
// The scanned set is every Go file under internal/ except internal/db, which is
// the control plane. That keeps the property true as IP-07 adds the handler
// package to the data plane: a package that needs cursor access or a Postgres
// connection is by definition not a data-plane package, and the scan refuses it
// the moment it appears.

func TestNoCursorOrKeysCommandOnTheDataPlane(t *testing.T) {
	badMethod := []string{"Keys", "Scan", "HScan", "SScan", "ZScan", "XScan", "ScanIterator"}
	badLiteral := []string{"KEYS", "SCAN", "HKEYS", "HSCAN", "SSCAN", "ZSCAN"}
	for _, f := range scanDataPlaneFiles(t) {
		if !f.parsed {
			continue
		}
		for _, arg := range f.literals {
			for _, lit := range badLiteral {
				if lit == strings.ToUpper(strings.TrimSpace(arg)) {
					t.Errorf("%s contains the datastore command %q, which cannot appear on the data plane (request-lifecycle.md section 6)", f.path, lit)
				}
			}
		}
		for _, call := range f.calls {
			for _, m := range badMethod {
				if call == m {
					t.Errorf("%s calls method %s, a cursor or a KEYS-family scan, which cannot appear on the data plane", f.path, call)
				}
			}
		}
	}
}

// TestNoControlPlaneConnectionOnTheDataPlane is the mechanical half of DoD
// item 1: no data-plane package may import the control plane, so a
// control-plane connection is structurally unreachable from the request path
// rather than merely unused. The bounded-miss path reaches configuration
// through the Loader interface in internal/snapshot, never through a database
// handle (DR-039).
func TestNoControlPlaneConnectionOnTheDataPlane(t *testing.T) {
	forbidden := map[string]string{
		"database/sql":                               "a SQL handle",
		"github.com/jackc/pgx/v5":                    "pgx",
		"github.com/jackc/pgx/v5/pgxpool":            "the connection pool",
		"github.com/quotacore/quotacore/internal/db": "the control plane",
	}
	for _, f := range scanDataPlaneFiles(t) {
		for _, imp := range f.imports {
			if why, bad := forbidden[imp]; bad {
				t.Errorf("%s imports %s (%s), which is the control plane (DR-039)", f.path, imp, why)
			}
		}
	}
}

// sourceFile is one Go source file with the parts the checks extract from it.
type sourceFile struct {
	path     string
	parsed   bool
	imports  []string
	calls    []string
	literals []string
}

// scanDataPlaneFiles walks internal/, parsing every .go file. internal/db is
// skipped: it owns the control-plane connection and the scanning property is
// about the packages that must not reach it. This very file is skipped, so the
// check cannot fail itself for naming the forbidden commands.
func scanDataPlaneFiles(t *testing.T) []sourceFile {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this source file")
	}
	dir := filepath.Dir(file)
	internal := filepath.Dir(dir)

	var out []sourceFile
	err := filepath.WalkDir(internal, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == filepath.Join(internal, "db") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if absEqual(path, file) {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		sf := sourceFile{path: path, parsed: true}
		for _, imp := range f.Imports {
			sf.imports = append(sf.imports, strings.Trim(imp.Path.Value, `"`))
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					sf.calls = append(sf.calls, sel.Sel.Name)
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					sf.literals = append(sf.literals, strings.Trim(x.Value, "`\""))
				}
			}
			return true
		})
		out = append(out, sf)
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	return out
}

// absEqual reports whether two paths name the same file, so the file that names
// the forbidden commands is not failed for naming them.
func absEqual(a, b string) bool {
	ra, errA := filepath.Abs(a)
	rb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	return filepath.Clean(ra) == filepath.Clean(rb)
}
