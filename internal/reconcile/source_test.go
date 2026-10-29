package reconcile

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestTheReconcilerCannotMoveABalance is the source half of IP-11 DoD 2
// (DR-030, DR-042, IP-06 item 7): detection, not prevention, and never
// correction. This package imports no store, script, data-plane or SQL
// package, so it has no handle on a balance; the one store write it can
// cause is the reversal's refund, which it hands to its Refunder, and the
// only method it calls on that interface is Refund.
func TestTheReconcilerCannotMoveABalance(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	forbidden := []string{
		"github.com/quotacore/quotacore/internal/store",
		"github.com/quotacore/quotacore/internal/script",
		"github.com/quotacore/quotacore/internal/dataplane",
		"github.com/quotacore/quotacore/internal/db",
		"github.com/quotacore/quotacore/internal/admin",
		"github.com/redis/go-redis",
		"database/sql",
		"github.com/jackc/pgx",
		"net",
		"os",
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		scanned++
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbidden {
				if path == bad || strings.HasPrefix(path, bad+"/") {
					t.Errorf("internal/reconcile/%s imports %s; the reconciler reaches the stores only through its interfaces", e.Name(), path)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Refunder" && sel.Sel.Name != "Refund" {
				t.Errorf("internal/reconcile/%s calls Refunder.%s; the reversal's refund is the one store write allowed", e.Name(), sel.Sel.Name)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("no source scanned")
	}
}
