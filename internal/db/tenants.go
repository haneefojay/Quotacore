package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/quotacore/quotacore/internal/snapshot"
)

var _ snapshot.IDResolver = (*Loader)(nil)

// TenantIDByExternalID is the first statement of a tenant's first sight on the
// data plane (IP-07, decision D3): one indexed read on the unique external_id.
// A deleted tenant reads as unknown, as it does to Load (INV-T1), and so does a
// string that cannot be an external_id.
func (l *Loader) TenantIDByExternalID(ctx context.Context, externalID string) (string, error) {
	if !ValidExternalID(externalID) {
		return "", snapshot.ErrTenantUnknown
	}
	var id string
	err := l.db.QueryRowContext(ctx,
		`SELECT id FROM tenants WHERE external_id = $1 AND state <> 'deleted'`, externalID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", snapshot.ErrTenantUnknown
	}
	if err != nil {
		return "", fmt.Errorf("resolve external_id: %w", err)
	}
	return id, nil
}
