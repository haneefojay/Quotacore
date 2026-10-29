package db

import (
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/snapshot"
)

type fixedVersions struct {
	v      int64
	before func()
	calls  int
}

func (f *fixedVersions) TenantVersion(context.Context, string) (int64, error) {
	f.calls++
	if f.before != nil {
		f.before()
	}
	return f.v, nil
}

// TestPostgresLoaderProjectsATenant: the loaded tenant is its rows — the
// plan and every entitlement with its interval and its feature's archive
// state, overrides that inherit through nil, the anchor in UTC, and the
// version from the datastore.
func TestPostgresLoaderProjectsATenant(t *testing.T) {
	db := migratedDatabase(t)
	ctx := context.Background()
	f := newFixture(t, db, "pro", "llm.tokens")
	tenant := f.tenant(t, "acme")
	if _, err := db.ExecContext(ctx, "UPDATE tenants SET timezone = 'Europe/Berlin', anchor_at = '2026-01-31T23:00:00+00' WHERE id = $1", tenant); err != nil {
		t.Fatal(err)
	}
	labels := []string{"hourly", "daily", "weekly", "monthly", "yearly", "never"}
	for i, l := range labels {
		var fid string
		state := "active"
		if l == "weekly" {
			state = "archived"
		}
		if err := db.QueryRowContext(ctx, "INSERT INTO features (key, name, unit, state) VALUES ($1, 'F', 'token', $2) RETURNING id", "f."+l, state).Scan(&fid); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO plan_entitlements (plan_id, feature_id, limit_value, interval) VALUES ($1, $2, $3, $4)", f.planID, fid, i+1, l); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO tenant_entitlement_overrides (tenant_id, feature_id, limit_value, interval)
		SELECT $1, id, NULL, 'daily' FROM features WHERE key = 'f.hourly'`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO tenant_entitlement_overrides (tenant_id, feature_id, limit_value)
		SELECT $1, id, 500 FROM features WHERE key = 'llm.tokens'`, tenant); err != nil {
		t.Fatal(err)
	}
	// The pins are the plan's cadences with the override's in front, as
	// provisioning writes them, and one feature retired from the plan with
	// the limit it had and the end of the window it was removed in (IP-10).
	if _, err := db.ExecContext(ctx, `INSERT INTO tenant_entitlement_pins (tenant_id, feature_id, interval)
		SELECT $1, e.feature_id, coalesce(o.interval, e.interval) FROM plan_entitlements e
		  LEFT JOIN tenant_entitlement_overrides o ON o.tenant_id = $1 AND o.feature_id = e.feature_id
		 WHERE e.plan_id = $2`, tenant, f.planID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `WITH g AS (INSERT INTO features (key, name, unit) VALUES ('f.gone', 'F', 'token') RETURNING id)
		INSERT INTO tenant_entitlement_pins (tenant_id, feature_id, interval, retired_limit, retired_until)
		SELECT $1, id, 'monthly', 3, '2026-03-01T00:00:00+00' FROM g`, tenant); err != nil {
		t.Fatal(err)
	}

	got, err := NewLoader(db, &fixedVersions{v: 9}).Load(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != tenant || got.ExternalID != "acme" || got.State != "active" || got.Timezone != "Europe/Berlin" || got.Version != 9 {
		t.Errorf("tenant fields: %+v", got)
	}
	if want := time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC); !got.Anchor.Equal(want) || got.Anchor.Location() != time.UTC {
		t.Errorf("anchor = %v, want %v in UTC", got.Anchor, want)
	}
	if got.Plan.ID != f.planID || got.Plan.Key != "pro" || len(got.Plan.Entitlements) != 7 {
		t.Fatalf("plan: %+v", got.Plan)
	}
	for i, l := range labels {
		e := got.Plan.Entitlements["f."+l]
		if e.Limit != int64(i+1) || e.Interval.String() != l || e.Archived != (l == "weekly") {
			t.Errorf("entitlement f.%s = %+v", l, e)
		}
	}
	if e := got.Plan.Entitlements["llm.tokens"]; e.Limit != 100 || e.Interval != cycle.Monthly {
		t.Errorf("fixture entitlement = %+v", e)
	}
	if o := got.Overrides["f.hourly"]; o.Limit != nil || o.Interval == nil || *o.Interval != cycle.Daily {
		t.Errorf("an override of the interval only must inherit the limit: %+v", o)
	}
	if o := got.Overrides["llm.tokens"]; o.Limit == nil || *o.Limit != 500 || o.Interval != nil {
		t.Errorf("an override of the limit only must inherit the interval: %+v", o)
	}
	if len(got.Pins) != 8 {
		t.Errorf("pins = %d, want the plan's seven and the retired one", len(got.Pins))
	}
	if p := got.Pins["f.hourly"]; p.Interval != cycle.Daily || p.RetiredLimit != nil {
		t.Errorf("the pin of an interval override = %+v", p)
	}
	if p := got.Pins["f.weekly"]; !p.Archived {
		t.Errorf("the pin of an archived feature = %+v", p)
	}
	until := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if p := got.Pins["f.gone"]; p.RetiredLimit == nil || *p.RetiredLimit != 3 || p.RetiredUntil == nil || !p.RetiredUntil.Equal(until) {
		t.Errorf("a retired pin = %+v", p)
	}

	// The loaded tenant resolves through the snapshot the way the rows say.
	s := snapshot.New(snapshot.Options{Loader: NewLoader(db, &fixedVersions{v: 9}), MissRate: 1e9, ControlPlaneTimeout: 5 * time.Second})
	r, err := s.Resolve(tenant, "f.weekly")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Archived {
		t.Errorf("an archived feature resolved as %+v", r)
	}
}

// TestPostgresLoaderTreatsDeletedAsUnknown is INV-T1 at the loader, plus the
// two other shapes of a tenant that cannot exist.
func TestPostgresLoaderTreatsDeletedAsUnknown(t *testing.T) {
	db := migratedDatabase(t)
	ctx := context.Background()
	f := newFixture(t, db, "pro", "llm.tokens")
	tenant := f.tenant(t, "gone")
	if _, err := db.ExecContext(ctx, "UPDATE tenants SET state = 'deleted', deleted_at = now() WHERE id = $1", tenant); err != nil {
		t.Fatal(err)
	}
	l := NewLoader(db, &fixedVersions{v: 1})
	for _, id := range []string{tenant, "00000000-0000-4000-8000-000000000000", "not-a-uuid", "acme"} {
		if _, err := l.Load(ctx, id); !errors.Is(err, snapshot.ErrTenantUnknown) {
			t.Errorf("Load(%q) = %v, want ErrTenantUnknown", id, err)
		}
	}
	suspended := f.tenant(t, "paused")
	if _, err := db.ExecContext(ctx, "UPDATE tenants SET state = 'suspended' WHERE id = $1", suspended); err != nil {
		t.Fatal(err)
	}
	if got, err := l.Load(ctx, suspended); err != nil || got.State != "suspended" {
		t.Errorf("a suspended tenant is served with its state: %v, %v", got, err)
	}
}

// TestPostgresLoaderReadsTheVersionFirst: a change committed between the
// version read and the row read must be in the rows, which is only true when
// the version is read first.
func TestPostgresLoaderReadsTheVersionFirst(t *testing.T) {
	db := migratedDatabase(t)
	ctx := context.Background()
	f := newFixture(t, db, "pro", "llm.tokens")
	tenant := f.tenant(t, "acme")
	v := &fixedVersions{v: 3, before: func() {
		if _, err := db.ExecContext(ctx, "UPDATE tenants SET timezone = 'Asia/Tokyo' WHERE id = $1", tenant); err != nil {
			t.Error(err)
		}
	}}
	got, err := NewLoader(db, v).Load(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if v.calls != 1 {
		t.Errorf("the version was read %d times, want once", v.calls)
	}
	if got.Timezone != "Asia/Tokyo" {
		t.Errorf("the rows predate the version read (timezone %q): a newer change would be cached under an older version and never pruned", got.Timezone)
	}
}

// TestPostgresLoaderFailureIsNotUnknown: an unreachable database is a
// failure the snapshot answers as control_plane_unavailable, never a tenant
// that does not exist.
func TestPostgresLoaderFailureIsNotUnknown(t *testing.T) {
	db := migratedDatabase(t)
	f := newFixture(t, db, "pro", "llm.tokens")
	tenant := f.tenant(t, "acme")
	closed := reopenClosed(t, db)
	_, err := NewLoader(closed, &fixedVersions{v: 1}).Load(context.Background(), tenant)
	if err == nil || errors.Is(err, snapshot.ErrTenantUnknown) {
		t.Errorf("Load on a closed pool = %v, want a failure that is not ErrTenantUnknown", err)
	}
}

func reopenClosed(t *testing.T, db *sql.DB) *sql.DB {
	t.Helper()
	var name string
	if err := db.QueryRowContext(context.Background(), "SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	c := reopen(t, name)
	c.Close()
	return c
}

// TestPostgresLoaderIsOneQuery holds DR-039's bound in the source: Load
// touches the database once, by handing it to loadTenantRow, and
// loadTenantRow makes exactly one call on what it is handed, so a miss is one
// round trip. The pins ride in the same statement (IP-10).
func TestPostgresLoaderIsOneQuery(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(file), "loader.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	dbUses, rowLoads, queries := 0, 0, 0
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		switch fn.Name.Name {
		case "Load":
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					if n.Sel.Name == "db" {
						dbUses++
					}
				case *ast.CallExpr:
					if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "loadTenantRow" {
						rowLoads++
					}
				}
				return true
			})
		case "loadTenantRow":
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "q" {
						queries++
					}
				}
				return true
			})
		}
		return false
	})
	if dbUses != 1 || rowLoads != 1 || queries != 1 {
		t.Errorf("Load uses the database %d times and loads a row %d times, and the row load makes %d calls; want 1, 1 and 1 (DR-039)",
			dbUses, rowLoads, queries)
	}
}

// expiredVersions answers as a store read does when its caller's deadline has
// already passed.
type expiredVersions struct{}

func (expiredVersions) TenantVersion(ctx context.Context, _ string) (int64, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

// TestAVersionReadOutOfTheCallersTimeIsNotTheStoreFailing (found by the v0.1
// code review): a first sight spends part of the control plane's deadline on
// Postgres before it reads the version, and a read that runs out of that time
// is the budget, not the data store, so it is not answered as the store's 503.
func TestAVersionReadOutOfTheCallersTimeIsNotTheStoreFailing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err := NewLoader(nil, expiredVersions{}).Load(ctx, "00000000-0000-4000-8000-000000000001")
	if err == nil || errors.Is(err, snapshot.ErrStoreUnavailable) {
		t.Fatalf("Load = %v, want the deadline and not ErrStoreUnavailable", err)
	}
}
