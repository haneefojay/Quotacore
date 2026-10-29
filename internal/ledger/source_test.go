package ledger

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestTheWriterCanOnlyAppend is DoD 7 (D5, security-model.md section 2 as
// amended): the writer has no path to a balance and no SQL of its own, and the
// appender it writes through says nothing but an append to the two ledger
// tables, the replay's removal and re-count of a dead letter, and reads.
func TestTheWriterCanOnlyAppend(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	forbidden := []string{
		"github.com/quotacore/quotacore/internal/store",
		"github.com/quotacore/quotacore/internal/script",
		"github.com/quotacore/quotacore/internal/db",
		"github.com/redis/go-redis",
		"database/sql",
		"github.com/jackc/pgx",
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbidden {
				if strings.HasPrefix(path, bad) {
					t.Errorf("internal/ledger/%s imports %s; the writer reaches the database only through Appender", e.Name(), path)
				}
			}
		}
	}

	// Every statement in the appender, by its leading words.
	allowed := regexp.MustCompile(`^(?:WITH\b[\s\S]*\bINSERT INTO usage_events\b|INSERT INTO (?:usage_events|usage_event_dead_letters)\b|` +
		`SELECT\b|DELETE FROM usage_event_dead_letters\b|UPDATE usage_event_dead_letters\b|SAVEPOINT\b|ROLLBACK TO SAVEPOINT\b|` +
		// The loss record (IP-11 D8): the writer's session row and its gap
		// tallies, neither of them the ledger.
		`INSERT INTO (?:ledger_sessions|usage_event_gaps)\b|UPDATE ledger_sessions\b)`)
	statement := regexp.MustCompile(`(?i)^\s*(?:WITH|INSERT|SELECT|UPDATE|DELETE|MERGE|TRUNCATE|ALTER|DROP|CREATE|SAVEPOINT|ROLLBACK|COPY)\b`)
	path := filepath.Join(dir, "..", "db", "ledger.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil || !statement.MatchString(s) {
			return true
		}
		seen++
		if !allowed.MatchString(strings.TrimSpace(s)) {
			t.Errorf("internal/db/ledger.go says %q, which is not an append to the ledger", strings.Fields(s)[0]+" "+strings.Join(strings.Fields(s)[1:3], " "))
		}
		return true
	})
	if seen == 0 {
		t.Fatal("no statement found in internal/db/ledger.go; the scan is not reading it")
	}
}
