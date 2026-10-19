package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/auth"
	"github.com/quotacore/quotacore/internal/snapshot"
)

// TestKeyLoaderReadsByPrefix: the miss read returns the row a token's prefix
// names, with the verifier, the scope and the expiry, and an unknown prefix is
// auth.ErrKeyUnknown rather than an error the cache would answer with a 503.
func TestKeyLoaderReadsByPrefix(t *testing.T) {
	db := migratedDatabase(t)
	ctx := context.Background()
	issued, err := auth.IssueToken(auth.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	var id string
	if err := db.QueryRowContext(ctx, `INSERT INTO api_keys (name, prefix, key_hash, scope, expires_at)
		VALUES ('k', $1, $2, 'runtime', $3) RETURNING id`, issued.Prefix, issued.Hash, expires).Scan(&id); err != nil {
		t.Fatal(err)
	}
	l := NewKeyLoader(db)
	k, err := l.LoadKey(ctx, issued.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if k.ID != id || k.Prefix != issued.Prefix || k.Scope != auth.Runtime || k.State != "active" ||
		k.ExpiresAt == nil || !k.ExpiresAt.Equal(expires) || string(k.Hash) != string(issued.Hash) {
		t.Errorf("loaded key = %+v", k)
	}
	if _, err := l.LoadKey(ctx, "0123456789abcdef"); !errors.Is(err, auth.ErrKeyUnknown) {
		t.Errorf("unknown prefix: err = %v, want auth.ErrKeyUnknown", err)
	}

	// The reconcile reads every listed prefix at once and leaves out the ones
	// with no row; a revocation is visible in the state it returns.
	if _, err := db.ExecContext(ctx, `UPDATE api_keys SET state = 'revoked', revoked_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	rows, err := l.ReconcileKeys(ctx, []string{issued.Prefix, "0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[issued.Prefix].State != "revoked" {
		t.Errorf("reconcile = %+v, want the one key, revoked", rows)
	}

	// last_used_at moves forward and never back.
	later := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	if err := l.TouchLastUsed(ctx, map[string]time.Time{id: later}); err != nil {
		t.Fatal(err)
	}
	if err := l.TouchLastUsed(ctx, map[string]time.Time{id: later.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var used time.Time
	if err := db.QueryRowContext(ctx, `SELECT last_used_at FROM api_keys WHERE id = $1`, id).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if !used.Equal(later) {
		t.Errorf("last_used_at = %v, want %v", used, later)
	}
}

// TestExternalIDResolves: the first statement of a tenant's first sight finds
// the tenant by external_id, and a deleted tenant or a string that cannot be an
// external_id is unknown (INV-T1).
func TestExternalIDResolves(t *testing.T) {
	db := migratedDatabase(t)
	ctx := context.Background()
	f := newFixture(t, db, "pro", "llm.tokens")
	id := f.tenant(t, "acme-prod-01")
	gone := f.tenant(t, "acme:gone")
	if _, err := db.ExecContext(ctx, `UPDATE tenants SET state = 'deleted', deleted_at = now() WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	l := NewLoader(db, &fixedVersions{v: 1})
	got, err := l.TenantIDByExternalID(ctx, "acme-prod-01")
	if err != nil || got != id {
		t.Errorf("TenantIDByExternalID = %q, %v, want %q", got, err, id)
	}
	for _, ext := range []string{"acme:gone", "nobody", "has space", "a@b"} {
		if _, err := l.TenantIDByExternalID(ctx, ext); !errors.Is(err, snapshot.ErrTenantUnknown) {
			t.Errorf("TenantIDByExternalID(%q) err = %v, want ErrTenantUnknown", ext, err)
		}
	}
}
