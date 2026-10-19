package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

// catalogueDump is an ordered text form of everything a migration can create
// in the public schema: columns with their type, nullability, default and
// identity, every constraint's definition, every index's definition, and every
// enum's labels in order. Two databases with the same dump have the same
// schema for every purpose the specification cares about. The runner's own
// bookkeeping tables are excluded, because data-model.md does not state them.
func catalogueDump(t *testing.T, db *sql.DB) string {
	t.Helper()
	queries := []string{
		`SELECT 'column ' || c.relname || '.' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
		        || CASE WHEN a.attnotnull THEN ' not null' ELSE '' END
		        || coalesce(' default ' || pg_get_expr(d.adbin, d.adrelid), '')
		        || CASE a.attidentity WHEN 'a' THEN ' identity always' WHEN 'd' THEN ' identity by default' ELSE '' END
		   FROM pg_attribute a
		   JOIN pg_class c ON c.oid = a.attrelid
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		   LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		  WHERE n.nspname = 'public' AND c.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped
		    AND c.relname NOT IN ('goose_db_version', 'quotacore_migration_checksums')
		  ORDER BY c.relname, a.attnum`,
		`SELECT 'constraint ' || c.relname || '.' || k.conname || ' ' || pg_get_constraintdef(k.oid)
		   FROM pg_constraint k
		   JOIN pg_class c ON c.oid = k.conrelid
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = 'public'
		    AND c.relname NOT IN ('goose_db_version', 'quotacore_migration_checksums')
		  ORDER BY c.relname, k.conname`,
		`SELECT 'index ' || indexname || ' ' || indexdef
		   FROM pg_indexes
		  WHERE schemaname = 'public'
		    AND tablename NOT IN ('goose_db_version', 'quotacore_migration_checksums')
		  ORDER BY indexname`,
		`SELECT 'enum ' || t.typname || ' ' || string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder)
		   FROM pg_type t
		   JOIN pg_enum e ON e.enumtypid = t.oid
		   JOIN pg_namespace n ON n.oid = t.typnamespace
		  WHERE n.nspname = 'public'
		  GROUP BY t.typname
		  ORDER BY t.typname`,
		`SELECT 'trigger ' || tgname FROM pg_trigger WHERE NOT tgisinternal ORDER BY tgname`,
		`SELECT 'rule ' || rulename FROM pg_rules WHERE schemaname = 'public' ORDER BY rulename`,
	}
	var b strings.Builder
	for _, q := range queries {
		rows, err := db.QueryContext(context.Background(), q)
		if err != nil {
			t.Fatalf("dump catalogue: %v", err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("dump catalogue: %v", err)
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("dump catalogue: %v", err)
		}
		rows.Close()
	}
	return b.String()
}

// repoFile reads a file relative to the repository root.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this source file")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}

var sqlBlock = regexp.MustCompile("(?s)```sql\n(.*?)```")

// dataModelDDL is every fenced sql block in section 2 of data-model.md, which
// is the stated-once source for the schema, split into statements.
func dataModelDDL(t *testing.T) []string {
	t.Helper()
	doc := repoFile(t, "docs/architecture/data-model.md")
	start := strings.Index(doc, "\n## 2. PostgreSQL schema")
	end := strings.Index(doc, "\n## 3. ")
	if start < 0 || end < start {
		t.Fatal("data-model.md no longer has its section 2 and section 3 headings")
	}
	var stmts []string
	for _, m := range sqlBlock.FindAllStringSubmatch(doc[start:end], -1) {
		var lines []string
		for _, l := range strings.Split(m[1], "\n") {
			if i := strings.Index(l, "--"); i >= 0 {
				l = l[:i]
			}
			lines = append(lines, l)
		}
		for _, s := range strings.Split(strings.Join(lines, "\n"), ";") {
			if s = strings.TrimSpace(s); s != "" {
				stmts = append(stmts, s)
			}
		}
	}
	if len(stmts) < 20 {
		t.Fatalf("found %d statements in data-model.md section 2, which is not the schema", len(stmts))
	}
	return stmts
}

// TestTheSchemaIsTheDataModel is DoD 2: the DDL the migrations build is the
// DDL data-model.md states, compared as catalogues rather than by eye, so a
// drift in either file fails.
func TestTheSchemaIsTheDataModel(t *testing.T) {
	migrated := migratedDatabase(t)
	documented := freshDatabase(t)
	for _, stmt := range dataModelDDL(t) {
		if _, err := documented.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("data-model.md statement does not apply: %v\n%s", err, stmt)
		}
	}
	got, want := catalogueDump(t, migrated), catalogueDump(t, documented)
	if got != want {
		t.Errorf("the migrated schema differs from data-model.md section 2\n%s", lineDiff(want, got))
	}
}

func lineDiff(want, got string) string {
	w, g := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(want, "\n") {
		w[l] = true
	}
	for _, l := range strings.Split(got, "\n") {
		g[l] = true
	}
	var b strings.Builder
	for _, l := range strings.Split(want, "\n") {
		if !g[l] {
			fmt.Fprintf(&b, "  only in data-model.md: %s\n", l)
		}
	}
	for _, l := range strings.Split(got, "\n") {
		if !w[l] {
			fmt.Fprintf(&b, "  only in the migrations: %s\n", l)
		}
	}
	return b.String()
}

// TestMigrationsApplyFromEmpty is the from-empty half of DoD 1.
func TestMigrationsApplyFromEmpty(t *testing.T) {
	db := migratedDatabase(t)
	ctx := context.Background()
	current, err := MigrationsCurrent(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if !current {
		t.Error("MigrationsCurrent = false immediately after Migrate on an empty database")
	}
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public'
		AND tablename IN ('features','plans','plan_entitlements','tenants','tenant_entitlement_overrides','api_keys','usage_events','audit_log')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 8 {
		t.Errorf("found %d of the eight control-plane tables", tables)
	}
}

// seedRows inserts one row into each table that exists at the current
// version, and returns how many it inserted, so an upgrade can be shown to
// keep them.
func seedRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	ctx := context.Background()
	exists := func(table string) bool {
		var ok bool
		if err := db.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", "public."+table).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	steps := []struct{ table, sql string }{
		{"features", `INSERT INTO features (key, name, unit) VALUES ('seed.feature', 'Seed', 'token')`},
		{"plans", `INSERT INTO plans (key, name) VALUES ('seed', 'Seed')`},
		{"plan_entitlements", `INSERT INTO plan_entitlements (plan_id, feature_id, limit_value, interval)
			SELECT p.id, f.id, 10, 'monthly' FROM plans p, features f`},
		{"tenants", `INSERT INTO tenants (external_id, display_name, plan_id, anchor_at)
			SELECT 'seed-tenant', 'Seed', id, now() FROM plans`},
		{"tenant_entitlement_overrides", `INSERT INTO tenant_entitlement_overrides (tenant_id, feature_id, limit_value)
			SELECT t.id, f.id, 20 FROM tenants t, features f`},
		{"api_keys", `INSERT INTO api_keys (name, prefix, key_hash, scope) VALUES ('seed', 'abcdef01', '\x00', 'admin')`},
		{"usage_events", `INSERT INTO usage_events (tenant_id, feature_id, cycle_index, delta, balance_after, source, event_type, request_id)
			SELECT t.id, f.id, 0, -1, 9, 'api', 'consumed', '01J8Z6M2QK9V3X7B4C0N5TDEFA' FROM tenants t, features f`},
		{"audit_log", `INSERT INTO audit_log (actor, action, subject_type, request_id)
			VALUES ('bootstrap', 'tenant.create', 'tenant', '01J8Z6M2QK9V3X7B4C0N5TDEFA')`},
	}
	n := 0
	for _, s := range steps {
		if !exists(s.table) {
			continue
		}
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+s.table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			if _, err := db.ExecContext(ctx, s.sql); err != nil {
				t.Fatalf("seed %s: %v", s.table, err)
			}
		}
		n++
	}
	return n
}

func rowCounts(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"features", "plans", "plan_entitlements", "tenants", "tenant_entitlement_overrides", "api_keys", "usage_events", "audit_log"} {
		var n int
		if err := db.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

// TestEveryPreviousVersionUpgradesToTheSameSchema is the upgrade half of
// DoD 1: from every earlier version, with data in every table that version
// has, the upgrade ends in exactly the schema a fresh install has, and keeps
// the rows.
func TestEveryPreviousVersionUpgradesToTheSameSchema(t *testing.T) {
	ctx := context.Background()
	want := catalogueDump(t, migratedDatabase(t))
	latest, err := EmbeddedVersion()
	if err != nil {
		t.Fatal(err)
	}
	for k := int64(1); k < latest; k++ {
		t.Run(fmt.Sprintf("from_%05d", k), func(t *testing.T) {
			db := freshDatabase(t)
			if err := migrate(ctx, db, embeddedMigrations(), k); err != nil {
				t.Fatalf("migrate to %d: %v", k, err)
			}
			seeded := seedRows(t, db)
			if err := Migrate(ctx, db, MigrateOptions{}); err != nil {
				t.Fatalf("upgrade from %d: %v", k, err)
			}
			if got := catalogueDump(t, db); got != want {
				t.Errorf("upgrading from %d gives a different schema than a fresh install\n%s", k, lineDiff(want, got))
			}
			kept := 0
			for _, n := range rowCounts(t, db) {
				if n > 0 {
					kept++
				}
			}
			if kept != seeded {
				t.Errorf("seeded %d tables at version %d, %d still have rows after the upgrade", seeded, k, kept)
			}
		})
	}
}

// TestConcurrentStartsMigrateOnce holds the advisory lock to real DDL: four
// starts against one empty database all succeed and each version is recorded
// once. Each start has its own pool, as four replicas would.
func TestConcurrentStartsMigrateOnce(t *testing.T) {
	ctx := context.Background()
	first := freshDatabase(t)
	var name string
	if err := first.QueryRowContext(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	pools := []*sql.DB{first}
	for i := 0; i < 3; i++ {
		pools = append(pools, reopen(t, name))
	}
	var wg sync.WaitGroup
	errs := make([]error, len(pools))
	for i, p := range pools {
		wg.Add(1)
		go func(i int, p *sql.DB) {
			defer wg.Done()
			errs[i] = Migrate(ctx, p, MigrateOptions{})
		}(i, p)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("start %d: %v", i, err)
		}
	}
	var dupes int
	if err := first.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT version_id FROM goose_db_version
		WHERE version_id > 0 GROUP BY version_id HAVING count(*) > 1) d`).Scan(&dupes); err != nil {
		t.Fatal(err)
	}
	if dupes != 0 {
		t.Errorf("%d versions were applied more than once", dupes)
	}
}

func reopen(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := Open(context.Background(), withDatabase(t, databaseURL(t), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestTheMigrationLockIsReleased holds the other half of the pinned
// connection: after Migrate returns, a different session can take the lock at
// once, so a lock taken on one connection was released on the same one.
func TestTheMigrationLockIsReleased(t *testing.T) {
	ctx := context.Background()
	db := migratedDatabase(t)
	var name string
	if err := db.QueryRowContext(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	other := reopen(t, name)
	var got bool
	if err := other.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", advisoryLockKey).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("the migration advisory lock is still held after Migrate returned")
	}
	_, _ = other.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey)
}

// TestNoDownMigrationShips holds the file set to the runner: nothing after
// the reserved file carries a down section the binary could never run.
func TestNoDownMigrationShips(t *testing.T) {
	files, err := versionedFiles(embeddedMigrations())
	if err != nil {
		t.Fatal(err)
	}
	for v, name := range files {
		if v == 1 {
			continue // applied before this rule, and its bytes are checksummed
		}
		body, err := migrationsFS.ReadFile(migrationDir + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "+goose Down") {
			t.Errorf("%s ships a down section; the rollback is a restore (ADR-0011, NFR-OPS3)", name)
		}
		if !strings.HasPrefix(string(body), "-- +goose Up\n") {
			t.Errorf("%s does not open with its up annotation", name)
		}
	}
}

// TestAnEditedMigrationRefusesToStart is ADR-0011's checksum rule: once a
// file is applied, a binary carrying different bytes for it does not start.
func TestAnEditedMigrationRefusesToStart(t *testing.T) {
	ctx := context.Background()
	db := migratedDatabase(t)
	edited := fstest.MapFS{}
	files, err := versionedFiles(embeddedMigrations())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		body, err := migrationsFS.ReadFile(migrationDir + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		edited[name] = &fstest.MapFile{Data: body}
	}
	edited["00003_catalogue.sql"].Data = append(append([]byte{}, edited["00003_catalogue.sql"].Data...), []byte("\n-- an edit\n")...)
	err = migrate(ctx, db, edited, 0)
	var e *ErrMigrationEdited
	if !asEdited(err, &e) || e.Version != 3 {
		t.Fatalf("migrate with an edited applied file = %v, want ErrMigrationEdited for version 3", err)
	}
	if err := Migrate(ctx, db, MigrateOptions{}); err != nil {
		t.Errorf("the unedited set no longer starts: %v", err)
	}
}

func asEdited(err error, target **ErrMigrationEdited) bool {
	e, ok := err.(*ErrMigrationEdited)
	if ok {
		*target = e
	}
	return ok
}

// TestAPreChecksumDatabaseIsRecordedOnFirstStart covers a database migrated
// before the record existed: the first start records every applied version.
func TestAPreChecksumDatabaseIsRecordedOnFirstStart(t *testing.T) {
	ctx := context.Background()
	db := migratedDatabase(t)
	if _, err := db.ExecContext(ctx, "DROP TABLE quotacore_migration_checksums"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db, MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	latest, _ := EmbeddedVersion()
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM quotacore_migration_checksums").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != latest {
		t.Errorf("recorded %d checksums, want %d", n, latest)
	}
}
