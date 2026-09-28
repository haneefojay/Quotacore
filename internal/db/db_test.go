package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEmbeddedVersionReadsTheFileSet(t *testing.T) {
	v, err := EmbeddedVersion()
	if err != nil {
		t.Fatalf("EmbeddedVersion: %v", err)
	}
	if v != 1 {
		t.Errorf("EmbeddedVersion = %d, want 1", v)
	}
}

func TestServerVersionGate(t *testing.T) {
	cases := []struct {
		raw     string
		wantErr bool
	}{
		{"16.15 (Debian 16.15-1.pgdg13+2)", false},
		{"17.2", false},
		{"15.8 (Debian 15.8-1)", true},
		{"9.6.24", true},
		{"", true},
		{"unknown", true},
	}
	for _, tc := range cases {
		err := checkServerVersion(tc.raw)
		if tc.wantErr && err == nil {
			t.Errorf("checkServerVersion(%q) = nil, want a refusal below %d", tc.raw, MinServerMajor)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("checkServerVersion(%q) = %v, want accepted", tc.raw, err)
		}
	}
}

func TestUpOnlyIsStructural(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this source file")
	}
	dir := filepath.Dir(file)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		src := string(body)
		for _, forbidden := range []string{"goose.Down", "goose.Reset", "goose.Redo", "RunContext"} {
			if strings.Contains(src, forbidden) {
				t.Errorf("%s calls %s, and the server has no down path (ADR-0011)", e.Name(), forbidden)
			}
		}
	}
}

// TestMigrationsAgainstRealPostgres runs only against a real database, and the
// gate is QUOTACORE_REQUIRE_INTEGRATION rather than CI. CI is the wrong signal:
// every hosted runner sets it, including the build job that has no database and
// must not be made to fail by this test. The job that brings the database up sets
// QUOTACORE_REQUIRE_INTEGRATION=1, so a green pipeline still cannot have skipped
// this test by accident, and the failure is attributable to one named variable.
func TestMigrationsAgainstRealPostgres(t *testing.T) {
	url := os.Getenv("QUOTACORE_DATABASE_URL")
	required := os.Getenv("QUOTACORE_REQUIRE_INTEGRATION") != ""
	if url == "" {
		if required {
			t.Fatal("QUOTACORE_REQUIRE_INTEGRATION is set but QUOTACORE_DATABASE_URL is unset, so this test would skip silently")
		}
		t.Skip("QUOTACORE_DATABASE_URL is unset, so there is no database to migrate")
	}
	ctx := context.Background()
	db, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := Migrate(ctx, db, MigrateOptions{}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	current, err := MigrationsCurrent(ctx, db)
	if err != nil {
		t.Fatalf("MigrationsCurrent: %v", err)
	}
	if !current {
		t.Error("MigrationsCurrent = false immediately after Migrate")
	}
	if err := Migrate(ctx, db, MigrateOptions{}); err != nil {
		t.Fatalf("second Migrate is not idempotent: %v", err)
	}
	if err := Migrate(ctx, db, MigrateOptions{Disabled: true}); err != nil {
		t.Fatalf("Migrate with Disabled: %v", err)
	}
	var recorded sql.NullInt64
	row := db.QueryRowContext(ctx, "SELECT max(version_id) FROM goose_db_version WHERE is_applied")
	if err := row.Scan(&recorded); err != nil {
		t.Fatalf("read goose version table: %v", err)
	}
	if !recorded.Valid || recorded.Int64 != 1 {
		t.Errorf("recorded version = %v, want 1", recorded)
	}
}
