package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/quotacore/quotacore/internal/admin"
)

// audit writes the one audit row a mutation records, inside the mutation's
// transaction (NFR-D4, DR-041). before and after are the resource as the API
// renders it, so an api_key row never carries key_hash (S8). reason is the
// caller's, for the mutations that take one.
func audit(ctx context.Context, tx *sql.Tx, a admin.Actor, action, subjectType, subjectID string,
	before, after json.Marshaler, reason string) error {
	b, err := marshalOrNull(before)
	if err != nil {
		return err
	}
	af, err := marshalOrNull(after)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO audit_log (actor_key_id, actor, action, subject_type, subject_id, before, after, reason, request_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		nullable(a.KeyID), a.Name, action, subjectType, nullable(subjectID), b, af, nullable(reason), a.RequestID)
	if err == nil && a.Audited != nil {
		a.Audited(action, subjectType)
	}
	return Classify(OpWrite, err)
}

func marshalOrNull(v json.Marshaler) (any, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := v.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return string(raw), nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Note is an audit row about the installation rather than one resource
// (IP-16, Q-28): `quotacore audit note` records with it the truncation a
// Postgres restore leaves in the history (consistency-and-recovery.md section
// 4.2, step 6), so the gap is documented rather than discovered later.
type Note struct {
	ID        string    `json:"id"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Reason    string    `json:"reason"`
	RequestID string    `json:"request_id"`
	At        time.Time `json:"occurred_at"`
}

// WriteNote appends one system audit row with the reason given.
func WriteNote(ctx context.Context, db *sql.DB, actor, requestID, reason string) (Note, error) {
	n := Note{Actor: actor, Action: "system.note", Reason: reason, RequestID: requestID}
	err := db.QueryRowContext(ctx, `
INSERT INTO audit_log (actor, action, subject_type, reason, request_id)
VALUES ($1, $2, 'system', $3, $4) RETURNING id::text, created_at`, actor, n.Action, reason, requestID).Scan(&n.ID, &n.At)
	return n, Classify(OpWrite, err)
}

// TenantIDs is every tenant's id, deleted ones included, for a configuration
// bump that must reach every snapshot entry.
func TenantIDs(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id::text FROM tenants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
