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
// makes exactly one database call, so a miss is one round trip.
func TestPostgresLoaderIsOneQuery(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(file), "loader.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Load" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "db" {
				calls++
			}
			return true
		})
		return false
	})
	if calls != 1 {
		t.Errorf("Load makes %d database calls, want 1 (DR-039)", calls)
	}
}
