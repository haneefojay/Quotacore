package db

import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// seedAged inserts an event and an audit row created days before now.
func seedAged(t *testing.T, db *sql.DB, tenant string, now time.Time, days ...int) {
	t.Helper()
	ctx := context.Background()
	for _, d := range days {
		at := now.Add(-time.Duration(d)*24*time.Hour - time.Hour)
		if _, err := db.ExecContext(ctx, `INSERT INTO usage_events (tenant_id, feature_id, cycle_index, delta,
			balance_after, source, event_type, request_id, created_at)
			SELECT $1::uuid, id, 0, -1, 1, 'api', 'consumed', 'r', $2 FROM features`, tenant, at); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO audit_log (actor, action, subject_type, request_id, created_at)
			VALUES ('bootstrap', 'tenant.create', 'tenant', 'r', $1)`, at); err != nil {
			t.Fatal(err)
		}
	}
}

func ages(t *testing.T, db *sql.DB, table string, now time.Time) []int {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT created_at FROM `+table+` ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatal(err)
		}
		out = append(out, int(now.Sub(at)/(24*time.Hour)))
	}
	return out
}

// TestRetentionRemovesOnlyExpiredRows is DoD 6: one tick deletes exactly the
// events older than the event retention and the audit rows older than the
// audit retention, in batches no larger than the batch size, once even when
// two instances tick together; a zero keeps everything, and nothing is
// deleted before the first tick.
func TestRetentionRemovesOnlyExpiredRows(t *testing.T) {
	db, tenant := ledgerFixture(t)
	ctx := context.Background()
	now := time.Now()
	// Ten rows past each cutoff, so a batch of 3 has to run four times.
	old := []int{}
	for range 10 {
		old = append(old, 402)
	}
	seedAged(t, db, tenant, now, append(old, 89, 91, 399, 401)...)
	opt := RetentionOptions{EventDays: 90, AuditDays: 400, BatchSize: 3}

	// Two instances at once: one prunes, the other finds the lock held.
	var wg sync.WaitGroup
	results := make([]Pruned, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = PruneOnce(ctx, db, now, opt)
		}()
	}
	wg.Wait()
	var events, audit int64
	for i := range 2 {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		events += results[i].Events
		audit += results[i].Audit
	}
	if events != 13 || audit != 11 {
		t.Errorf("deleted %d events and %d audit rows over both instances, want 13 and 11", events, audit)
	}
	if got := ages(t, db, "usage_events", now); len(got) != 1 || got[0] != 89 {
		t.Errorf("events left, by age in days: %v, want [89]", got)
	}
	if got := ages(t, db, "audit_log", now); len(got) != 3 || got[0] != 399 {
		t.Errorf("audit rows left, by age in days: %v, want the 89, 91 and 399", got)
	}

	// A second tick finds nothing more.
	p, err := PruneOnce(ctx, db, now, opt)
	if err != nil || p.Events != 0 || p.Audit != 0 {
		t.Errorf("a second tick = %+v, %v", p, err)
	}

	// Zero keeps a table's rows.
	seedAged(t, db, tenant, now, 500)
	p, err = PruneOnce(ctx, db, now, RetentionOptions{EventDays: 0, AuditDays: 0})
	if err != nil || p.Events != 0 || p.Audit != 0 {
		t.Errorf("retention 0 = %+v, %v; want nothing deleted", p, err)
	}

	// The job waits FirstRun before its first tick.
	run, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		RunRetention(run, db, RetentionOptions{EventDays: 90, AuditDays: 400, FirstRun: time.Hour, Interval: time.Hour})
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if n := countRows(t, db, `SELECT count(*) FROM usage_events WHERE created_at < now() - interval '400 days'`); n != 1 {
		t.Errorf("the job deleted before its first tick")
	}
}

// TestNoRouteReachesRetention is the other half of DoD 6: no API call deletes
// an event. The job's entry points are called from cmd/quotacore's assembly
// and from nowhere else in the module's non-test source.
func TestNoRouteReachesRetention(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	callers := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".ogen-check" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			}
			switch name {
			case "RunRetention", "PruneOnce", "pruneTable", "pruneBatch":
				callers[rel] = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for c := range callers {
		switch c {
		case "cmd/quotacore/assemble.go", "internal/db/retention.go":
		default:
			t.Errorf("%s calls the retention job, which only the assembly may start (DR-044, NFR-C6)", c)
		}
	}
	if !callers["cmd/quotacore/assemble.go"] {
		t.Error("nothing starts the retention job")
	}
}
