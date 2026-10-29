package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
)

// databaseURL gates every integration test on the same pair of variables as
// TestMigrationsAgainstRealPostgres: no URL skips, unless
// QUOTACORE_REQUIRE_INTEGRATION says a skip would be a silent pass.
func databaseURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("QUOTACORE_DATABASE_URL")
	if u == "" {
		if os.Getenv("QUOTACORE_REQUIRE_INTEGRATION") != "" {
			t.Fatal("QUOTACORE_REQUIRE_INTEGRATION is set but QUOTACORE_DATABASE_URL is unset, so this test would skip silently")
		}
		t.Skip("QUOTACORE_DATABASE_URL is unset, so there is no database")
	}
	return u
}

// freshDatabase creates an empty database on the configured server and drops
// it when the test ends, so no two tests share a schema (testing-strategy.md,
// isolation). The name carries a random nonce, so a crashed run leaves
// nothing a later run could mistake for its own.
func freshDatabase(t *testing.T) *sql.DB {
	t.Helper()
	base := databaseURL(t)
	ctx := context.Background()
	admin, err := Open(ctx, base)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "qc_t_" + hex.EncodeToString(nonce[:])
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	db, err := Open(ctx, withDatabase(t, base, name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// migratedDatabase is a fresh database with every embedded migration applied.
func migratedDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db := freshDatabase(t)
	if err := Migrate(context.Background(), db, MigrateOptions{}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func withDatabase(t *testing.T, base, name string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}
