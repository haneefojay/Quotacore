package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/admin"
	"github.com/quotacore/quotacore/internal/rolling"
)

// Admin is the Postgres implementation of admin.Store. Every write runs in
// one transaction with its audit row (DR-041, NFR-D4), and every versioned
// write locks its row, compares the version the caller read, and bumps it
// (S4, FS-12). A refusal is an *Error naming its catalogue code; any other
// error is the control plane failing to answer.
type Admin struct {
	db *sql.DB
	// beforeCommit runs after a write's statements and its audit row and
	// before the commit. Only the tests set it, to prove a commit that fails
	// leaves no audit row (IP-09 DoD 4).
	beforeCommit func(context.Context) error
}

var _ admin.Store = (*Admin)(nil)

func NewAdmin(db *sql.DB) *Admin { return &Admin{db: db} }

// WithBeforeCommit returns a copy whose writes run fn just before committing,
// and roll back if it fails.
func (a *Admin) WithBeforeCommit(fn func(context.Context) error) *Admin {
	return &Admin{db: a.db, beforeCommit: fn}
}

func (a *Admin) Ping(ctx context.Context) error { return a.db.PingContext(ctx) }

func (a *Admin) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin control-plane transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if a.beforeCommit != nil {
		if err := a.beforeCommit(ctx); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return Classify(OpWrite, err)
	}
	return nil
}

// querier is a *sql.DB or a *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// after is the keyset condition of a newest-first page: rows strictly older
// than the position, by (time, tiebreaker).
func after(p admin.Page) (any, any) {
	if p.After == nil {
		return nil, nil
	}
	return p.After.At, p.After.Tie
}

func conflict(current int64) error {
	return &Error{Code: api.ErrorCodeResourceVersionConflict, Current: &current}
}

func ts(t time.Time) api.Timestamp { return api.Timestamp(t.UTC()) }

func nilTS(t sql.NullTime) api.NilTimestamp {
	if !t.Valid {
		return api.NilTimestamp{Null: true}
	}
	return api.NewNilTimestamp(ts(t.Time))
}

// ---- keys

const keyRow = `id, name, prefix, scope,
 CASE WHEN state = 'active' AND expires_at IS NOT NULL AND expires_at <= now() THEN 'expired' ELSE state::text END,
 created_at, expires_at, last_used_at, revoked_at`

func scanAdminKey(r rowScanner) (api.Key, error) {
	var (
		k                      api.Key
		scope, state           string
		created                time.Time
		expires, used, revoked sql.NullTime
	)
	if err := r.Scan(&k.ID, &k.Name, &k.Prefix, &scope, &state, &created, &expires, &used, &revoked); err != nil {
		return api.Key{}, err
	}
	k.Scope, k.State, k.CreatedAt = api.KeyScope(scope), api.KeyState(state), ts(created)
	k.ExpiresAt, k.LastUsedAt, k.RevokedAt = nilTS(expires), nilTS(used), nilTS(revoked)
	return k, nil
}

func (a *Admin) ListKeys(ctx context.Context, p admin.Page) ([]api.Key, error) {
	t, tie := after(p)
	rows, err := a.db.QueryContext(ctx, `SELECT `+keyRow+` FROM api_keys
 WHERE $1::timestamptz IS NULL OR (created_at, id) < ($1, $2::uuid)
 ORDER BY created_at DESC, id DESC LIMIT $3`, t, tie, p.Limit+1)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	return collect(rows, scanAdminKey)
}

func (a *Admin) CreateKey(ctx context.Context, actor admin.Actor, in admin.NewKey) (api.Key, error) {
	var out api.Key
	err := a.tx(ctx, func(tx *sql.Tx) error {
		var expires any
		if in.ExpiresAt != nil {
			expires = in.ExpiresAt.UTC()
		}
		k, err := scanAdminKey(tx.QueryRowContext(ctx, `INSERT INTO api_keys (name, prefix, key_hash, scope, expires_at)
 VALUES ($1, $2, $3, $4, $5) RETURNING `+keyRow, in.Name, in.Prefix, in.Hash, string(in.Scope), expires))
		if err != nil {
			if c := Classify(OpWrite, err); errors.Is(c, ErrPrefixCollision) {
				return admin.ErrPrefixTaken
			} else {
				return c
			}
		}
		out = k
		return audit(ctx, tx, actor, "api_key.create", "api_key", k.ID.String(), nil, &k, "")
	})
	return out, err
}

func (a *Admin) RevokeKey(ctx context.Context, actor admin.Actor, id string, at time.Time) (string, bool, error) {
	var (
		prefix  string
		changed bool
	)
	err := a.tx(ctx, func(tx *sql.Tx) error {
		before, err := scanAdminKey(tx.QueryRowContext(ctx, `SELECT `+keyRow+` FROM api_keys WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrKeyNotFound
		}
		if err != nil {
			return Classify(OpWrite, err)
		}
		prefix = before.Prefix
		if before.State == api.KeyStateRevoked {
			return nil
		}
		k, err := scanAdminKey(tx.QueryRowContext(ctx, `UPDATE api_keys SET state = 'revoked', revoked_at = $2
 WHERE id = $1 RETURNING `+keyRow, id, at.UTC()))
		if err != nil {
			return Classify(OpWrite, err)
		}
		changed = true
		return audit(ctx, tx, actor, "api_key.revoke", "api_key", id, &before, &k, "")
	})
	return prefix, changed, err
}

// ---- features

const featureRow = `id, key, name, unit, kind, description, state, version, created_at, updated_at, archived_at`

func scanFeature(r rowScanner) (api.Feature, error) {
	var (
		f                api.Feature
		key, unit, kind  string
		state            string
		version          int64
		created, updated time.Time
		archived         sql.NullTime
	)
	if err := r.Scan(&f.ID, &key, &f.Name, &unit, &kind, &f.Description, &state, &version, &created, &updated, &archived); err != nil {
		return api.Feature{}, err
	}
	f.FeatureKey, f.Unit, f.Kind, f.State = api.FeatureKey(key), api.Unit(unit), api.FeatureKind(kind), api.ArchiveState(state)
	f.Version, f.CreatedAt, f.UpdatedAt, f.ArchivedAt = api.Version(version), ts(created), ts(updated), nilTS(archived)
	return f, nil
}

func (a *Admin) ListFeatures(ctx context.Context, p admin.Page, kind string) ([]api.Feature, error) {
	t, tie := after(p)
	rows, err := a.db.QueryContext(ctx, `SELECT `+featureRow+` FROM features
 WHERE ($1::timestamptz IS NULL OR (created_at, id) < ($1, $2::uuid))
   AND ($4 = '' OR kind::text = $4)
 ORDER BY created_at DESC, id DESC LIMIT $3`, t, tie, p.Limit+1, kind)
	if err != nil {
		return nil, fmt.Errorf("list features: %w", err)
	}
	return collect(rows, scanFeature)
}

func (a *Admin) GetFeature(ctx context.Context, key string) (api.Feature, error) {
	return getFeature(ctx, a.db, key, "")
}

func getFeature(ctx context.Context, q querier, key, lock string) (api.Feature, error) {
	f, err := scanFeature(q.QueryRowContext(ctx, `SELECT `+featureRow+` FROM features WHERE key = $1 `+lock, key))
	if errors.Is(err, sql.ErrNoRows) {
		return api.Feature{}, ErrFeatureNotFound
	}
	if err != nil {
		return api.Feature{}, Classify(OpWrite, err)
	}
	return f, nil
}

func (a *Admin) CreateFeature(ctx context.Context, actor admin.Actor, in admin.NewFeature) (api.Feature, error) {
	var out api.Feature
	err := a.tx(ctx, func(tx *sql.Tx) error {
		kind := in.Kind
		if kind == "" {
			kind = "metered"
		}
		f, err := scanFeature(tx.QueryRowContext(ctx, `INSERT INTO features (key, name, unit, description, kind)
 VALUES ($1, $2, $3, $4, $5::feature_kind) RETURNING `+featureRow, in.Key, in.Name, in.Unit, in.Description, kind))
		if err != nil {
			return Classify(OpWrite, err)
		}
		out = f
		return audit(ctx, tx, actor, "feature.create", "feature", f.ID.String(), nil, &f, "")
	})
	return out, err
}

// featureReferenced is INV-F2's test: a plan entitlement, an override or a
// usage event names the feature.
func featureReferenced(ctx context.Context, q querier, featureID string) (bool, error) {
	var referenced bool
	err := q.QueryRowContext(ctx, `SELECT
			EXISTS (SELECT 1 FROM plan_entitlements WHERE feature_id = $1)
			OR EXISTS (SELECT 1 FROM tenant_entitlement_overrides WHERE feature_id = $1)
			OR EXISTS (SELECT 1 FROM tenant_entitlement_pins WHERE feature_id = $1)
			OR EXISTS (SELECT 1 FROM usage_events WHERE feature_id = $1)`, featureID).Scan(&referenced)
	return referenced, Classify(OpWrite, err)
}

func (a *Admin) UpdateFeature(ctx context.Context, actor admin.Actor, key string, c admin.FeatureChange) (api.Feature, error) {
	var out api.Feature
	err := a.tx(ctx, func(tx *sql.Tx) error {
		before, err := getFeature(ctx, tx, key, "FOR UPDATE")
		if err != nil {
			return err
		}
		if int64(before.Version) != c.Version {
			return conflict(int64(before.Version))
		}
		if c.Unit != nil && *c.Unit != string(before.Unit) {
			referenced, err := featureReferenced(ctx, tx, before.ID.String())
			if err != nil {
				return err
			}
			if referenced {
				return ErrFeatureUnitImmutable
			}
		}
		f, err := scanFeature(tx.QueryRowContext(ctx, `UPDATE features
   SET name = coalesce($2, name), description = coalesce($3, description), unit = coalesce($4, unit),
       version = version + 1, updated_at = now()
 WHERE id = $1 RETURNING `+featureRow, before.ID, c.Name, c.Description, c.Unit))
		if err != nil {
			return Classify(OpWrite, err)
		}
		out = f
		return audit(ctx, tx, actor, "feature.update", "feature", f.ID.String(), &before, &f, "")
	})
	return out, err
}

func (a *Admin) ArchiveFeature(ctx context.Context, actor admin.Actor, key string, version int64, reason string) (api.Feature, []string, error) {
	var (
		out     api.Feature
		tenants []string
	)
	err := a.tx(ctx, func(tx *sql.Tx) error {
		before, err := getFeature(ctx, tx, key, "FOR UPDATE")
		if err != nil {
			return err
		}
		if int64(before.Version) != version {
			return conflict(int64(before.Version))
		}
		out = before
		if before.State == api.ArchiveStateArchived {
			return nil // S5: archiving again changes nothing and is not audited
		}
		f, err := scanFeature(tx.QueryRowContext(ctx, `UPDATE features
   SET state = 'archived', archived_at = now(), version = version + 1, updated_at = now()
 WHERE id = $1 RETURNING `+featureRow, before.ID))
		if err != nil {
			return Classify(OpWrite, err)
		}
		out = f
		rows, err := tx.QueryContext(ctx, `
SELECT t.id FROM tenants t JOIN plan_entitlements e ON e.plan_id = t.plan_id
 WHERE e.feature_id = $1 AND t.state <> 'deleted'
UNION
SELECT o.tenant_id FROM tenant_entitlement_overrides o WHERE o.feature_id = $1
UNION
SELECT n.tenant_id FROM tenant_entitlement_pins n WHERE n.feature_id = $1`, f.ID)
		if err != nil {
			return Classify(OpWrite, err)
		}
		if tenants, err = collect(rows, func(r rowScanner) (string, error) {
			var id string
			return id, r.Scan(&id)
		}); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "feature.archive", "feature", f.ID.String(), &before, &f, reason)
	})
	return out, tenants, err
}

func (a *Admin) DeleteFeature(ctx context.Context, actor admin.Actor, key, reason string) error {
	return a.tx(ctx, func(tx *sql.Tx) error {
		before, err := getFeature(ctx, tx, key, "FOR UPDATE")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM features WHERE id = $1", before.ID); err != nil {
			return Classify(OpDeleteFeature, err)
		}
		return audit(ctx, tx, actor, "feature.delete", "feature", before.ID.String(), &before, nil, reason)
	})
}

// ---- plans

const planRow = `p.id, p.key, p.name, p.description, p.state, p.version, p.created_at, p.updated_at, p.archived_at,
 coalesce((SELECT jsonb_agg(jsonb_build_object('feature_key', f.key, 'limit', e.limit_value,
                  'reset_interval', e.interval) || ` + windowJSON + ` ORDER BY f.key)
             FROM plan_entitlements e JOIN features f ON f.id = e.feature_id
            WHERE e.plan_id = p.id), '[]'::jsonb)`

// windowJSON is a rolling entitlement's window as its wire name, merged into
// the entitlement's object, and nothing at all for any other kind, so a metered
// or boolean entitlement reads exactly as it did before IP-19 (DR-052).
const windowJSON = `CASE e.rolling_window_seconds
   WHEN 300 THEN '{"window":"rolling_5m"}'::jsonb WHEN 900 THEN '{"window":"rolling_15m"}'::jsonb
   WHEN 3600 THEN '{"window":"rolling_1h"}'::jsonb WHEN 21600 THEN '{"window":"rolling_6h"}'::jsonb
   WHEN 86400 THEN '{"window":"rolling_24h"}'::jsonb WHEN 604800 THEN '{"window":"rolling_7d"}'::jsonb
   WHEN 2592000 THEN '{"window":"rolling_30d"}'::jsonb ELSE '{}'::jsonb END`

func scanPlan(r rowScanner) (api.Plan, error) {
	var (
		p                api.Plan
		key, state       string
		version          int64
		created, updated time.Time
		archived         sql.NullTime
		ents             []byte
	)
	if err := r.Scan(&p.ID, &key, &p.Name, &p.Description, &state, &version, &created, &updated, &archived, &ents); err != nil {
		return api.Plan{}, err
	}
	p.PlanKey, p.State, p.Version = api.PlanKey(key), api.ArchiveState(state), api.Version(version)
	p.CreatedAt, p.UpdatedAt, p.ArchivedAt = ts(created), ts(updated), nilTS(archived)
	var raw []json.RawMessage
	if err := json.Unmarshal(ents, &raw); err != nil {
		return api.Plan{}, fmt.Errorf("decode entitlements: %w", err)
	}
	p.Entitlements = make([]api.PlanEntitlement, len(raw))
	for i, e := range raw {
		if err := p.Entitlements[i].UnmarshalJSON(e); err != nil {
			return api.Plan{}, fmt.Errorf("decode entitlement: %w", err)
		}
	}
	return p, nil
}

func (a *Admin) ListPlans(ctx context.Context, p admin.Page) ([]api.Plan, error) {
	t, tie := after(p)
	rows, err := a.db.QueryContext(ctx, `SELECT `+planRow+` FROM plans p
 WHERE $1::timestamptz IS NULL OR (p.created_at, p.id) < ($1, $2::uuid)
 ORDER BY p.created_at DESC, p.id DESC LIMIT $3`, t, tie, p.Limit+1)
	if err != nil {
		return nil, fmt.Errorf("list plans: %w", err)
	}
	return collect(rows, scanPlan)
}

func (a *Admin) GetPlan(ctx context.Context, id string) (api.Plan, error) {
	return getPlan(ctx, a.db, id, "")
}

// getPlan reads a plan by id or by key (S1): a UUID-shaped identifier is an
// id first and a key second, because a plan key may itself be UUID-shaped.
func getPlan(ctx context.Context, q querier, id, lock string) (api.Plan, error) {
	if uuidShape.MatchString(id) {
		p, err := scanPlan(q.QueryRowContext(ctx, `SELECT `+planRow+` FROM plans p WHERE p.id = $1 `+lock, id))
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return api.Plan{}, Classify(OpWrite, err)
		}
	}
	p, err := scanPlan(q.QueryRowContext(ctx, `SELECT `+planRow+` FROM plans p WHERE p.key = $1 `+lock, id))
	if errors.Is(err, sql.ErrNoRows) {
		return api.Plan{}, ErrPlanNotFound
	}
	if err != nil {
		return api.Plan{}, Classify(OpWrite, err)
	}
	return p, nil
}

func (a *Admin) CreatePlan(ctx context.Context, actor admin.Actor, in admin.NewPlan) (api.Plan, error) {
	var out api.Plan
	err := a.tx(ctx, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `INSERT INTO plans (key, name, description) VALUES ($1, $2, $3) RETURNING id`,
			in.Key, in.Name, in.Description).Scan(&id)
		if err != nil {
			return Classify(OpWrite, err)
		}
		for i, e := range in.Entitlements {
			field := "entitlements[" + strconv.Itoa(i) + "].feature_key"
			var (
				featureID, state, kind string
			)
			err := tx.QueryRowContext(ctx, `SELECT id, state, kind FROM features WHERE key = $1 FOR SHARE`, e.Feature).Scan(&featureID, &state, &kind)
			if errors.Is(err, sql.ErrNoRows) {
				return &Error{Code: api.ErrorCodeValidationFailed, Field: field}
			}
			if err != nil {
				return Classify(OpWrite, err)
			}
			if state == "archived" {
				return &Error{Code: api.ErrorCodeValidationFailed, Field: field}
			}
			if err := entitlementKind(kind, e, i); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO plan_entitlements (plan_id, feature_id, limit_value, interval, rolling_window_seconds)
 VALUES ($1, $2, $3, $4, $5)`, id, featureID, e.Limit, e.Interval, windowSeconds(e)); err != nil {
				if c := Classify(OpWrite, err); errors.Is(c, ErrValidation) {
					return &Error{Code: api.ErrorCodeValidationFailed, Field: "entitlements[" + strconv.Itoa(i) + "]"}
				} else {
					return c
				}
			}
		}
		p, err := getPlan(ctx, tx, id, "")
		if err != nil {
			return err
		}
		out = p
		return audit(ctx, tx, actor, "plan.create", "plan", id, nil, &p, "")
	})
	return out, err
}

func (a *Admin) ArchivePlan(ctx context.Context, actor admin.Actor, id string, version int64, reason string) (api.Plan, error) {
	var out api.Plan
	err := a.tx(ctx, func(tx *sql.Tx) error {
		before, err := getPlan(ctx, tx, id, "FOR UPDATE OF p")
		if err != nil {
			return err
		}
		if int64(before.Version) != version {
			return conflict(int64(before.Version))
		}
		out = before
		if before.State == api.ArchiveStateArchived {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plans
   SET state = 'archived', archived_at = now(), version = version + 1, updated_at = now() WHERE id = $1`, before.ID); err != nil {
			return Classify(OpWrite, err)
		}
		p, err := getPlan(ctx, tx, before.ID.String(), "")
		if err != nil {
			return err
		}
		out = p
		return audit(ctx, tx, actor, "plan.archive", "plan", p.ID.String(), &before, &p, reason)
	})
	return out, err
}

func (a *Admin) DeletePlan(ctx context.Context, actor admin.Actor, id, reason string) error {
	return a.tx(ctx, func(tx *sql.Tx) error {
		before, err := getPlan(ctx, tx, id, "FOR UPDATE OF p")
		if err != nil {
			return err
		}
		// The entitlements go first because they reference the plan too, and
		// their foreign key would otherwise fire before the tenants' and
		// answer for the wrong reason (DeletePlan).
		if _, err := tx.ExecContext(ctx, "DELETE FROM plan_entitlements WHERE plan_id = $1", before.ID); err != nil {
			return Classify(OpDeletePlan, err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM plans WHERE id = $1", before.ID); err != nil {
			return Classify(OpDeletePlan, err)
		}
		return audit(ctx, tx, actor, "plan.delete", "plan", before.ID.String(), &before, nil, reason)
	})
}

// ---- tenants

const tenantRow = `t.id, t.external_id, t.display_name, p.key, t.timezone, t.state, t.anchor_at, t.version,
 t.created_at, t.updated_at, t.deleted_at, t.metadata`

const tenantFrom = ` FROM tenants t JOIN plans p ON p.id = t.plan_id `

func scanTenant(r rowScanner) (api.Tenant, error) {
	var (
		t                        api.Tenant
		external, planKey, state string
		version                  int64
		anchor, created, updated time.Time
		deleted                  sql.NullTime
		meta                     []byte
	)
	if err := r.Scan(&t.ID, &external, &t.DisplayName, &planKey, &t.Timezone, &state, &anchor, &version,
		&created, &updated, &deleted, &meta); err != nil {
		return api.Tenant{}, err
	}
	t.ExternalID, t.PlanKey, t.State, t.Version = api.ExternalId(external), api.PlanKey(planKey), api.TenantState(state), api.Version(version)
	t.AnchorAt, t.CreatedAt, t.UpdatedAt, t.DeletedAt = ts(anchor), ts(created), ts(updated), nilTS(deleted)
	t.Metadata = api.TenantMetadata{}
	if err := t.Metadata.UnmarshalJSON(meta); err != nil {
		return api.Tenant{}, fmt.Errorf("decode tenant metadata: %w", err)
	}
	return t, nil
}

func (a *Admin) ListTenants(ctx context.Context, p admin.Page, planKey string) ([]api.Tenant, error) {
	t, tie := after(p)
	rows, err := a.db.QueryContext(ctx, `SELECT `+tenantRow+tenantFrom+`
 WHERE ($1::timestamptz IS NULL OR (t.created_at, t.id) < ($1, $2::uuid)) AND ($4 = '' OR p.key = $4)
 ORDER BY t.created_at DESC, t.id DESC LIMIT $3`, t, tie, p.Limit+1, planKey)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	return collect(rows, scanTenant)
}

func (a *Admin) GetTenant(ctx context.Context, id string) (api.Tenant, error) {
	return getTenant(ctx, a.db, id, "")
}

// getTenant reads a tenant by id or by external_id (S1). A soft-deleted
// tenant is returned, with its deleted_at (S2).
func getTenant(ctx context.Context, q querier, id, lock string) (api.Tenant, error) {
	if uuidShape.MatchString(id) {
		t, err := scanTenant(q.QueryRowContext(ctx, `SELECT `+tenantRow+tenantFrom+` WHERE t.id = $1 `+lock, id))
		if err == nil {
			return t, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return api.Tenant{}, Classify(OpWrite, err)
		}
	}
	if !ValidExternalID(id) {
		return api.Tenant{}, ErrTenantNotFound
	}
	t, err := scanTenant(q.QueryRowContext(ctx, `SELECT `+tenantRow+tenantFrom+` WHERE t.external_id = $1 `+lock, id))
	if errors.Is(err, sql.ErrNoRows) {
		return api.Tenant{}, ErrTenantNotFound
	}
	if err != nil {
		return api.Tenant{}, Classify(OpWrite, err)
	}
	return t, nil
}

// lockTenant reads a tenant for a write: locked, and refused as
// tenant_deleted once soft-deleted, which is terminal (T-7).
func lockTenant(ctx context.Context, tx *sql.Tx, id string) (api.Tenant, error) {
	t, err := getTenant(ctx, tx, id, "FOR UPDATE OF t")
	if err != nil {
		return api.Tenant{}, err
	}
	if t.State == api.TenantStateDeleted {
		return api.Tenant{}, ErrTenantDeleted
	}
	return t, nil
}

func (a *Admin) CreateTenant(ctx context.Context, actor admin.Actor, in admin.NewTenant,
	open func(context.Context, admin.Opening) error) (api.Tenant, error) {
	var out api.Tenant
	err := a.tx(ctx, func(tx *sql.Tx) error {
		var planID, state string
		err := tx.QueryRowContext(ctx, `SELECT id, state FROM plans WHERE key = $1 FOR SHARE`, in.PlanKey).Scan(&planID, &state)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && state == "archived") {
			// An unknown plan and an archived one are both a plan_key the
			// tenant cannot be put on (P-4).
			return &Error{Code: api.ErrorCodeValidationFailed, Field: "plan_key"}
		}
		if err != nil {
			return Classify(OpWrite, err)
		}
		var id string
		err = tx.QueryRowContext(ctx, `INSERT INTO tenants (external_id, display_name, plan_id, timezone, anchor_at, metadata)
 VALUES ($1, $2, $3, $4, $5, $6::jsonb) RETURNING id`,
			in.ExternalID, in.DisplayName, planID, in.Timezone, in.Anchor, string(in.Metadata)).Scan(&id)
		if err != nil {
			return Classify(OpWrite, err)
		}
		// The tenant is anchored now, so it is pinned now: every feature of
		// its plan at the plan's cadence, an archived one included so a
		// request for it is still feature_archived (D1).
		if err := repin(ctx, tx, []string{id}); err != nil {
			return err
		}
		t, err := getTenant(ctx, tx, id, "")
		if err != nil {
			return err
		}
		out = t
		if err := audit(ctx, tx, actor, "tenant.create", "tenant", id, nil, &t, ""); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT f.key, e.limit_value, e.interval
  FROM plan_entitlements e JOIN features f ON f.id = e.feature_id
 WHERE e.plan_id = $1 AND f.state = 'active' AND e.interval IS NOT NULL ORDER BY f.key`, planID)
		if err != nil {
			return Classify(OpWrite, err)
		}
		ents, err := collect(rows, func(r rowScanner) (admin.Entitlement, error) {
			var e admin.Entitlement
			return e, r.Scan(&e.Feature, &e.Limit, &e.Interval)
		})
		if err != nil {
			return err
		}
		return open(ctx, admin.Opening{TenantID: id, Anchor: time.Time(t.AnchorAt), Timezone: t.Timezone, Entitlements: ents})
	})
	return out, err
}

func (a *Admin) UpdateTenant(ctx context.Context, actor admin.Actor, id string, c admin.TenantChange) (api.Tenant, error) {
	var out api.Tenant
	err := a.tx(ctx, func(tx *sql.Tx) error {
		before, err := lockTenant(ctx, tx, id)
		if err != nil {
			return err
		}
		if int64(before.Version) != c.Version {
			return conflict(int64(before.Version))
		}
		var meta any
		if c.Metadata != nil {
			meta = string(c.Metadata)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tenants
   SET display_name = coalesce($2, display_name), timezone = coalesce($3, timezone),
       metadata = coalesce($4::jsonb, metadata), version = version + 1, updated_at = now()
 WHERE id = $1`, before.ID, c.DisplayName, c.Timezone, meta); err != nil {
			return Classify(OpWrite, err)
		}
		// The tenant's own plan_key is not a plan change (S1).
		action := "tenant.update"
		if c.PlanKey != nil && *c.PlanKey != string(before.PlanKey) {
			if err := changePlan(ctx, tx, before.ID.String(), *c.PlanKey, c.At, actor.RequestID); err != nil {
				return err
			}
			action = "tenant.change_plan"
		}
		t, err := getTenant(ctx, tx, before.ID.String(), "")
		if err != nil {
			return err
		}
		out = t
		return audit(ctx, tx, actor, action, "tenant", t.ID.String(), &before, &t, "")
	})
	return out, err
}

func (a *Admin) SetTenantState(ctx context.Context, actor admin.Actor, id string, version int64, reason, to string) (api.Tenant, error) {
	action := "tenant.suspend"
	if to == "active" {
		action = "tenant.resume"
	}
	var out api.Tenant
	err := a.tx(ctx, func(tx *sql.Tx) error {
		before, err := lockTenant(ctx, tx, id)
		if err != nil {
			return err
		}
		if int64(before.Version) != version {
			return conflict(int64(before.Version))
		}
		out = before
		if string(before.State) == to {
			return nil // S5: suspending a suspended tenant changes nothing
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tenants SET state = $2, version = version + 1, updated_at = now() WHERE id = $1`,
			before.ID, to); err != nil {
			return Classify(OpWrite, err)
		}
		t, err := getTenant(ctx, tx, before.ID.String(), "")
		if err != nil {
			return err
		}
		out = t
		return audit(ctx, tx, actor, action, "tenant", t.ID.String(), &before, &t, reason)
	})
	return out, err
}

func (a *Admin) DeleteTenant(ctx context.Context, actor admin.Actor, id, reason string, at time.Time) (api.Tenant, error) {
	var out api.Tenant
	err := a.tx(ctx, func(tx *sql.Tx) error {
		before, err := lockTenant(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tenants
   SET state = 'deleted', deleted_at = $2, version = version + 1, updated_at = now() WHERE id = $1`,
			before.ID, at.UTC()); err != nil {
			return Classify(OpWrite, err)
		}
		t, err := getTenant(ctx, tx, before.ID.String(), "")
		if err != nil {
			return err
		}
		out = t
		return audit(ctx, tx, actor, "tenant.delete", "tenant", t.ID.String(), &before, &t, reason)
	})
	return out, err
}

// ---- audit and events

func scanAudit(r rowScanner) (api.AuditEntry, error) {
	var (
		e                api.AuditEntry
		keyID, subjectID sql.NullString
		subjectType      string
		before, after    []byte
		reason           sql.NullString
		requestID        string
		created          time.Time
	)
	if err := r.Scan(&e.ID, &e.Actor, &keyID, &e.Action, &subjectType, &subjectID, &before, &after, &reason, &requestID, &created); err != nil {
		return api.AuditEntry{}, err
	}
	e.SubjectType, e.RequestID, e.OccurredAt = api.AuditEntrySubjectType(subjectType), api.RequestId(requestID), ts(created)
	e.ActorKeyID, e.SubjectID = nilUUID(keyID), nilUUID(subjectID)
	e.Reason = api.NilString{Null: true}
	if reason.Valid {
		e.Reason = api.NewNilString(reason.String)
	}
	e.Before = api.NilAuditEntryBefore{Null: true}
	if before != nil {
		e.Before = api.NilAuditEntryBefore{Value: api.AuditEntryBefore{}}
		if err := e.Before.Value.UnmarshalJSON(before); err != nil {
			return api.AuditEntry{}, fmt.Errorf("decode audit before: %w", err)
		}
	}
	e.After = api.NilAuditEntryAfter{Null: true}
	if after != nil {
		e.After = api.NilAuditEntryAfter{Value: api.AuditEntryAfter{}}
		if err := e.After.Value.UnmarshalJSON(after); err != nil {
			return api.AuditEntry{}, fmt.Errorf("decode audit after: %w", err)
		}
	}
	return e, nil
}

func nilUUID(s sql.NullString) api.NilUUID {
	if !s.Valid {
		return api.NilUUID{Null: true}
	}
	u, err := uuid.Parse(s.String)
	if err != nil {
		return api.NilUUID{Null: true}
	}
	return api.NewNilUUID(u)
}

func (a *Admin) ListAudit(ctx context.Context, f admin.AuditFilter, p admin.Page) ([]api.AuditEntry, error) {
	t, tie := after(p)
	rows, err := a.db.QueryContext(ctx, `
SELECT id, actor, actor_key_id, action, subject_type, subject_id, before, after, reason, request_id, created_at
  FROM audit_log
 WHERE ($1::timestamptz IS NULL OR (created_at, id) < ($1, $2::uuid))
   AND ($4 = '' OR (subject_type = 'tenant' AND subject_id = (SELECT id FROM tenants WHERE external_id = $4)))
   AND ($5::timestamptz IS NULL OR created_at >= $5)
   AND ($6::timestamptz IS NULL OR created_at < $6)
 ORDER BY created_at DESC, id DESC LIMIT $3`, t, tie, p.Limit+1, f.TenantExternalID, timeOrNil(f.From), timeOrNil(f.To))
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	return collect(rows, scanAudit)
}

func timeOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func (a *Admin) ListEvents(ctx context.Context, id string, f admin.EventFilter, p admin.Page) (admin.EventPage, error) {
	tn, err := getTenant(ctx, a.db, id, "")
	if err != nil {
		return admin.EventPage{}, err
	}
	out := admin.EventPage{Anchor: time.Time(tn.AnchorAt), Timezone: tn.Timezone}
	var seq any
	if p.After != nil {
		n, err := strconv.ParseInt(p.After.Tie, 10, 64)
		if err != nil {
			return admin.EventPage{}, ErrValidation
		}
		seq = n
	}
	var at any
	if p.After != nil {
		at = p.After.At
	}
	rows, err := a.db.QueryContext(ctx, `
SELECT u.id, f.key, u.cycle_index, u.delta, u.balance_after, u.source, u.event_type, u.request_id,
       coalesce(u.idempotency_key, u.reconciles_key), u.created_at, u.seq, n.interval::text, u.cycle_start
  FROM usage_events u
  JOIN features f ON f.id = u.feature_id
  LEFT JOIN tenant_entitlement_pins n ON n.tenant_id = u.tenant_id AND n.feature_id = u.feature_id
 WHERE u.tenant_id = $1
   AND ($2::timestamptz IS NULL OR (u.created_at, u.seq) < ($2, $3::bigint))
   AND ($5 = '' OR f.key = $5)
   AND ($6 = '' OR u.event_type::text = $6)
   AND ($7 = '' OR u.source::text = $7)
   AND ($8 = '' OR u.idempotency_key = $8 OR u.reconciles_key = $8)
   AND ($9::timestamptz IS NULL OR u.created_at >= $9)
   AND ($10::timestamptz IS NULL OR u.created_at < $10)
 ORDER BY u.created_at DESC, u.seq DESC LIMIT $4`,
		tn.ID, at, seq, p.Limit+1, f.FeatureKey, f.EventType, f.Source, f.ClientKey, timeOrNil(f.From), timeOrNil(f.To))
	if err != nil {
		return admin.EventPage{}, fmt.Errorf("list events: %w", err)
	}
	external := tn.ExternalID
	out.Events, err = collect(rows, func(r rowScanner) (admin.Event, error) {
		var (
			e                   admin.Event
			key, source, kind   string
			requestID           string
			clientKey, interval sql.NullString
			created             time.Time
			start               sql.NullTime
			delta, balanceAfter int64
		)
		if err := r.Scan(&e.ID, &key, &e.Index, &delta, &balanceAfter, &source, &kind, &requestID, &clientKey,
			&created, &e.Seq, &interval, &start); err != nil {
			return e, err
		}
		e.TenantExternalID, e.FeatureKey, e.CycleIndex, e.Delta = external, api.FeatureKey(key), e.Index, delta
		e.BalanceAfter, e.Source, e.EventType = api.Amount(balanceAfter), api.EventSource(source), api.EventType(kind)
		e.RequestID = api.NewNilRequestId(api.RequestId(requestID))
		e.IdempotencyKey = api.NilIdempotencyKey{Null: true}
		if clientKey.Valid {
			e.IdempotencyKey = api.NewNilIdempotencyKey(api.IdempotencyKey(clientKey.String))
		}
		e.OccurredAt = ts(created)
		e.Interval = interval.String
		if start.Valid {
			e.Start = start.Time
		}
		return e, nil
	})
	return out, err
}

// collect scans every row and closes the set.
func collect[T any](rows *sql.Rows, scan func(rowScanner) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// entitlementKind refuses an entitlement whose shape is not its feature's
// kind: a boolean feature takes no limit and no interval, and a metered one
// takes both (DR-050). A value supplied where none belongs is refused rather
// than ignored, naming the field, which is the rule the contract's
// PlanEntitlement states and the schema alone cannot (IP-17 DoD 7).
func entitlementKind(kind string, e admin.Entitlement, i int) error {
	prefix := "entitlements[" + strconv.Itoa(i) + "]."
	boolean, rolling := kind == "boolean", kind == "rolling"
	switch {
	case boolean && e.Limit != nil, !boolean && e.Limit == nil:
		return &Error{Code: api.ErrorCodeValidationFailed, Field: prefix + "limit"}
	case (boolean || rolling) && e.Interval != nil, !boolean && !rolling && e.Interval == nil:
		return &Error{Code: api.ErrorCodeValidationFailed, Field: prefix + "reset_interval"}
	case !rolling && e.Window != nil, rolling && e.Window == nil:
		// A window belongs to a rolling feature and to nothing else, and a
		// rolling feature has one (DR-052).
		return &Error{Code: api.ErrorCodeValidationFailed, Field: prefix + "window"}
	}
	return nil
}

// windowSeconds is the stored form of an entitlement's window, nil when it has
// none. The name was checked against the seven when the body was read.
func windowSeconds(e admin.Entitlement) *int64 {
	if e.Window == nil {
		return nil
	}
	w, ok := rolling.Parse(*e.Window)
	if !ok {
		return nil
	}
	s := w.Seconds()
	return &s
}
