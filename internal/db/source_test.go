package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// rewrites matches a statement that would change history: an UPDATE or a
// DELETE of the audit log or the ledger (DR-044, INV-U2), a TRUNCATE of
// either, a DELETE of an API key (which would fire audit_log's ON DELETE SET
// NULL, the database rewriting an audit row), and an UPDATE that sets a
// feature's key (INV-F3).
var rewrites = regexp.MustCompile(`(?is)` +
	`\bupdate\s+(only\s+)?(public\.)?(audit_log|usage_events)\b` +
	`|\bdelete\s+from\s+(only\s+)?(public\.)?(audit_log|usage_events|api_keys)\b` +
	`|\btruncate\s+(table\s+)?[^;]*\b(audit_log|usage_events|api_keys)\b` +
	`|\bupdate\s+(only\s+)?(public\.)?features\s+set\s+([^;]*?,\s*)?key\s*=`)

// triggers matches DDL that would let the database rewrite a row behind the
// application's back.
var triggers = regexp.MustCompile(`(?i)\bcreate\s+(or\s+replace\s+)?(rule|trigger)\b`)

// retentionAllowed is the one place DR-044 and NFR-C6 permit a deletion: the
// bounded background retention job, which is IP-12's. IP-12 added exactly that
// file, not a pattern.
var retentionAllowed = map[string]bool{"internal/db/retention.go": true}

func TestTheRewriteMatcherCatchesWhatItMust(t *testing.T) {
	for _, s := range []string{
		"UPDATE audit_log SET actor = 'x'",
		"update  usage_events set delta = 1",
		"DELETE FROM audit_log WHERE id = $1",
		"delete from ONLY public.usage_events",
		"DELETE FROM api_keys WHERE id = $1",
		"TRUNCATE audit_log",
		"truncate table plans, usage_events",
		"UPDATE features SET name = $2, key = $3 WHERE id = $1",
		"UPDATE features SET key=$2",
	} {
		if !rewrites.MatchString(s) {
			t.Errorf("the matcher misses %q", s)
		}
	}
	for _, s := range []string{
		"INSERT INTO audit_log (actor) VALUES ($1)",
		"SELECT * FROM usage_events",
		"UPDATE api_keys SET state = 'revoked' WHERE id = $1",
		"UPDATE features SET unit = $2, version = version + 1 WHERE id = $1",
		"DELETE FROM plan_entitlements WHERE plan_id = $1",
		"UPDATE features SET name = $2 WHERE key = $1",
	} {
		if rewrites.MatchString(s) {
			t.Errorf("the matcher refuses %q, which changes no history", s)
		}
	}
}

// TestNoAuditOrLedgerRowIsRewritten is DoD 6: no string anywhere in the
// module's non-test Go source, and no migration, rewrites the audit log or the
// ledger. The scan is of string literals, parsed, so a statement split across
// a concatenation is still one statement only if it is one literal; the
// matcher self-test above proves the matcher, and this proves the corpus.
func TestNoAuditOrLedgerRowIsRewritten(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "dist", "node_modules", ".ogen-check":
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		switch {
		case strings.HasSuffix(path, ".sql"):
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if m := rewrites.FindString(string(body)); m != "" {
				t.Errorf("%s rewrites history: %q", rel, m)
			}
			if m := triggers.FindString(string(body)); m != "" {
				t.Errorf("%s creates a rule or trigger: %q", rel, m)
			}
		case strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go"):
			if retentionAllowed[rel] {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			scanned++
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				if m := rewrites.FindString(s); m != "" {
					t.Errorf("%s rewrites history: %q (DR-044, INV-U2)", rel, m)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 20 {
		t.Fatalf("scanned %d Go files, which is not the module", scanned)
	}
}
