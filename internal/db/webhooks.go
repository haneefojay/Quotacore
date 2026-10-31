package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/admin"
)

// ---- webhooks (IP-21, DR-055)

const webhookRow = `w.id, t.external_id, w.url, w.subscriptions, w.secret_generation, w.created_at, w.updated_at`

const webhookFrom = ` FROM webhooks w JOIN tenants t ON t.id = w.tenant_id `

func scanWebhook(r rowScanner) (admin.StoredWebhook, error) {
	var (
		w                admin.StoredWebhook
		external         string
		subs             []byte
		created, updated time.Time
	)
	if err := r.Scan(&w.ID, &external, &w.Webhook.URL, &subs, &w.Webhook.SecretGeneration, &created, &updated); err != nil {
		return admin.StoredWebhook{}, err
	}
	w.Webhook.TenantExternalID = api.ExternalId(external)
	w.Webhook.CreatedAt, w.Webhook.UpdatedAt = ts(created), ts(updated)
	w.Webhook.Subscriptions = []api.WebhookSubscription{}
	if err := json.Unmarshal(subs, &w.Webhook.Subscriptions); err != nil {
		return admin.StoredWebhook{}, fmt.Errorf("decode webhook subscriptions: %w", err)
	}
	return w, nil
}

// webhookOf reads a tenant's webhook, locked when lock is set; a tenant with
// none is webhook_not_found.
func webhookOf(ctx context.Context, q querier, tenantID, lock string) (admin.StoredWebhook, error) {
	w, err := scanWebhook(q.QueryRowContext(ctx, `SELECT `+webhookRow+webhookFrom+` WHERE w.tenant_id = $1 `+lock, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return admin.StoredWebhook{}, ErrWebhookNotFound
	}
	if err != nil {
		return admin.StoredWebhook{}, Classify(OpWrite, err)
	}
	return w, nil
}

// GetWebhook reads the webhook of the tenant id names, by id or external_id.
// A deleted tenant has none: its delete removed it.
func (a *Admin) GetWebhook(ctx context.Context, id string) (admin.StoredWebhook, error) {
	t, err := getTenant(ctx, a.db, id, "")
	if err != nil {
		return admin.StoredWebhook{}, err
	}
	return webhookOf(ctx, a.db, t.ID.String(), "")
}

// PutWebhook creates the tenant's webhook or replaces its URL and
// subscription set, under the tenant's lock, with one audit row either way
// (DR-041). It reports whether it created the row. The secret generation is
// kept on a replace.
func (a *Admin) PutWebhook(ctx context.Context, actor admin.Actor, id string, in admin.WebhookInput) (admin.StoredWebhook, bool, error) {
	var (
		out     admin.StoredWebhook
		created bool
	)
	subs, err := json.Marshal(in.Subscriptions)
	if err != nil {
		return out, false, err
	}
	err = a.tx(ctx, func(tx *sql.Tx) error {
		t, err := lockTenant(ctx, tx, id)
		if err != nil {
			return err
		}
		before, err := webhookOf(ctx, tx, t.ID.String(), "FOR UPDATE OF w")
		switch {
		case errors.Is(err, ErrWebhookNotFound):
			created = true
			if _, err := tx.ExecContext(ctx, `INSERT INTO webhooks (tenant_id, url, subscriptions) VALUES ($1, $2, $3)`,
				t.ID, in.URL, string(subs)); err != nil {
				return Classify(OpWrite, err)
			}
		case err != nil:
			return err
		default:
			if _, err := tx.ExecContext(ctx, `UPDATE webhooks SET url = $2, subscriptions = $3, updated_at = now() WHERE id = $1`,
				before.ID, in.URL, string(subs)); err != nil {
				return Classify(OpWrite, err)
			}
		}
		out, err = webhookOf(ctx, tx, t.ID.String(), "")
		if err != nil {
			return err
		}
		if created {
			return audit(ctx, tx, actor, "webhook.create", "webhook", out.ID, nil, &out.Webhook, "")
		}
		return audit(ctx, tx, actor, "webhook.update", "webhook", out.ID, &before.Webhook, &out.Webhook, "")
	})
	return out, created, err
}

// RotateWebhookSecret moves the tenant's webhook to its next secret
// generation, audited with the generation on each side and never the secret.
func (a *Admin) RotateWebhookSecret(ctx context.Context, actor admin.Actor, id string) (admin.StoredWebhook, error) {
	var out admin.StoredWebhook
	err := a.tx(ctx, func(tx *sql.Tx) error {
		t, err := getTenant(ctx, tx, id, "FOR UPDATE OF t")
		if err != nil {
			return err
		}
		before, err := webhookOf(ctx, tx, t.ID.String(), "FOR UPDATE OF w")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE webhooks SET secret_generation = secret_generation + 1, updated_at = now()
 WHERE id = $1`, before.ID); err != nil {
			return Classify(OpWrite, err)
		}
		out, err = webhookOf(ctx, tx, t.ID.String(), "")
		if err != nil {
			return err
		}
		return audit(ctx, tx, actor, "webhook.rotate_secret", "webhook", out.ID, &before.Webhook, &out.Webhook, "")
	})
	return out, err
}

// DeleteWebhook removes the tenant's webhook, audited with what it was.
func (a *Admin) DeleteWebhook(ctx context.Context, actor admin.Actor, id string) error {
	return a.tx(ctx, func(tx *sql.Tx) error {
		t, err := getTenant(ctx, tx, id, "FOR UPDATE OF t")
		if err != nil {
			return err
		}
		return removeWebhook(ctx, tx, actor, t.ID.String(), "", true)
	})
}

// removeWebhook deletes a tenant's webhook in tx with its audit row. must
// decides whether a tenant with none is webhook_not_found or nothing, which
// is the tenant delete's case.
func removeWebhook(ctx context.Context, tx *sql.Tx, actor admin.Actor, tenantID, reason string, must bool) error {
	before, err := webhookOf(ctx, tx, tenantID, "FOR UPDATE OF w")
	if errors.Is(err, ErrWebhookNotFound) && !must {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM webhooks WHERE id = $1`, before.ID); err != nil {
		return Classify(OpWrite, err)
	}
	return audit(ctx, tx, actor, "webhook.delete", "webhook", before.ID, &before.Webhook, nil, reason)
}
