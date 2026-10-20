package db

import (
	"context"
	"database/sql"

	"github.com/google/uuid"

	"github.com/quotacore/quotacore/internal/admin"
)

// BootstrapOutcome is what Bootstrap did, for the startup log line.
type BootstrapOutcome int

const (
	// BootstrapInstalled: the table was empty and the key is now its only row.
	BootstrapInstalled BootstrapOutcome = iota
	// BootstrapIgnored: keys exist, so the supplied key was not installed.
	BootstrapIgnored
)

// Bootstrap installs the operator-supplied admin key on an empty api_keys
// table, named bootstrap-admin, with an audit row whose actor is "bootstrap"
// (ADR-0018, D3). No API request can write that actor: a request's actor is
// its key's name, and the key routes refuse the name "bootstrap". Only the
// prefix and the digest arrive here; the secret never does. The table is
// locked first, so replicas starting together install the key once and the
// rest find it present (INV-K2).
func Bootstrap(ctx context.Context, db *sql.DB, prefix string, hash []byte) (BootstrapOutcome, error) {
	outcome := BootstrapIgnored
	err := inTx(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "LOCK TABLE api_keys IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return Classify(OpWrite, err)
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM api_keys)").Scan(&exists); err != nil {
			return Classify(OpWrite, err)
		}
		if exists {
			return nil
		}
		k, err := scanAdminKey(tx.QueryRowContext(ctx, `INSERT INTO api_keys (name, prefix, key_hash, scope)
 VALUES ('bootstrap-admin', $1, $2, 'admin') RETURNING `+keyRow, prefix, hash))
		if err != nil {
			return Classify(OpWrite, err)
		}
		requestID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		outcome = BootstrapInstalled
		return audit(ctx, tx, admin.Actor{Name: "bootstrap", RequestID: requestID.String()},
			"api_key.bootstrap", "api_key", k.ID.String(), nil, &k, "")
	})
	return outcome, err
}

// HasKeys reports whether any API key exists, so a start with no bootstrap
// key and no keys can say that the admin surface has no way in.
func HasKeys(ctx context.Context, db *sql.DB) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM api_keys)").Scan(&exists)
	return exists, err
}
