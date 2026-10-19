package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/quotacore/quotacore/internal/auth"
)

// KeyLoader is the Postgres implementation of auth.KeyLoader: the indexed read
// by prefix a key-cache miss makes, the one-statement reconcile of every cached
// prefix, and the batched last_used_at write (ADR-0012, ADR-0018).
type KeyLoader struct{ db *sql.DB }

var _ auth.KeyLoader = (*KeyLoader)(nil)

func NewKeyLoader(db *sql.DB) *KeyLoader { return &KeyLoader{db: db} }

const keyColumns = `id, prefix, scope, state, expires_at, key_hash`

// LoadKey reads one key by its unique prefix.
func (l *KeyLoader) LoadKey(ctx context.Context, prefix string) (auth.Key, error) {
	row := l.db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE prefix = $1`, prefix)
	k, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.Key{}, auth.ErrKeyUnknown
	}
	if err != nil {
		return auth.Key{}, fmt.Errorf("load key: %w", err)
	}
	return k, nil
}

// ReconcileKeys reads every listed prefix in one statement.
func (l *KeyLoader) ReconcileKeys(ctx context.Context, prefixes []string) (map[string]auth.Key, error) {
	out := make(map[string]auth.Key, len(prefixes))
	if len(prefixes) == 0 {
		return out, nil
	}
	rows, err := l.db.QueryContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE prefix = ANY($1::text[])`, prefixes)
	if err != nil {
		return nil, fmt.Errorf("reconcile keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("reconcile keys: %w", err)
		}
		out[k.Prefix] = k
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reconcile keys: %w", err)
	}
	return out, nil
}

// TouchLastUsed writes last_used_at for each key in one statement. A value
// older than the one stored never moves it backwards.
func (l *KeyLoader) TouchLastUsed(ctx context.Context, used map[string]time.Time) error {
	if len(used) == 0 {
		return nil
	}
	ids := make([]string, 0, len(used))
	ats := make([]time.Time, 0, len(used))
	for id, at := range used {
		ids = append(ids, id)
		ats = append(ats, at.UTC())
	}
	_, err := l.db.ExecContext(ctx, `
UPDATE api_keys k
   SET last_used_at = greatest(k.last_used_at, u.at)
  FROM unnest($1::uuid[], $2::timestamptz[]) AS u(id, at)
 WHERE k.id = u.id`, ids, ats)
	if err != nil {
		return fmt.Errorf("touch last_used_at: %w", err)
	}
	return nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanKey(r rowScanner) (auth.Key, error) {
	var (
		k       auth.Key
		scope   string
		expires sql.NullTime
	)
	if err := r.Scan(&k.ID, &k.Prefix, &scope, &k.State, &expires, &k.Hash); err != nil {
		return auth.Key{}, err
	}
	k.Scope = auth.Scope(scope)
	if expires.Valid {
		t := expires.Time.UTC()
		k.ExpiresAt = &t
	}
	return k, nil
}
