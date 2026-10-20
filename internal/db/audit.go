package db

import (
	"context"
	"database/sql"
	"encoding/json"

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
