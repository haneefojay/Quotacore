package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/admin"
	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/snapshot"
)

// The plan lifecycle (IP-10): the plan edit, impact, apply-now and the plan
// change, the locked tenant writes behind grant, set, force-rollover and the
// overrides, and the pending re-anchors that carry a committed anchor to the
// balance hashes (D1, D2).
//
// Every transaction here that touches a tenant takes the tenant row before a
// pending_reanchors row, the order the drain takes them in too, so two of them
// can wait on each other but never deadlock.

// ---- the plan edit

type currentEntitlement struct {
	featureID string
	kind      string
	limit     *int64 // nil, with interval, for a boolean feature (DR-050)
	interval  *string
	window    *int64 // seconds, for a rolling feature only (DR-052)
}

func sameEntitlement(was currentEntitlement, e admin.Entitlement) bool {
	eq := func(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	es := func(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	return eq(was.limit, e.Limit) && es(was.interval, e.Interval) && eq(was.window, windowSeconds(e))
}

func (a *Admin) UpdatePlan(ctx context.Context, actor admin.Actor, id string, c admin.PlanChange) (api.Plan, []string, error) {
	var (
		out     api.Plan
		tenants []string
	)
	err := a.tx(ctx, func(tx *sql.Tx) error {
		before, err := getPlan(ctx, tx, id, "FOR UPDATE OF p")
		if err != nil {
			return err
		}
		if int64(before.Version) != c.Version {
			return conflict(int64(before.Version))
		}
		if before.State == api.ArchiveStateArchived {
			// An archived plan is no longer an offer, so it has nothing to
			// edit (P-2 is from active).
			return &Error{Code: api.ErrorCodeValidationFailed, Field: "id"}
		}
		planID := before.ID.String()
		if _, err := tx.ExecContext(ctx, `UPDATE plans
   SET name = coalesce($2, name), description = coalesce($3, description), version = version + 1, updated_at = now()
 WHERE id = $1`, planID, c.Name, c.Description); err != nil {
			return Classify(OpWrite, err)
		}
		if c.Entitlements != nil {
			if err := replaceEntitlements(ctx, tx, planID, c.Entitlements, c.At); err != nil {
				return err
			}
		}
		if tenants, err = planTenants(ctx, tx, planID); err != nil {
			return err
		}
		p, err := getPlan(ctx, tx, planID, "")
		if err != nil {
			return err
		}
		out = p
		return audit(ctx, tx, actor, "plan.update", "plan", planID, &before, &p, "")
	})
	return out, tenants, err
}

// replaceEntitlements makes the plan's set the given one. A kept feature's
// limit and cadence are updated; the offer changes, and no tenant's pin does.
// A removed feature is retired on every tenant pinned to it, until the end of
// the window at falls in under that tenant's own anchor and cadence (E-4,
// INV-E1). An added feature un-retires the pins still inside that window and
// drops the ones past it; a tenant with no pin gets the feature at its next
// re-anchor (E-1, DR-045).
func replaceEntitlements(ctx context.Context, tx *sql.Tx, planID string, ents []admin.Entitlement, at time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT f.key, e.feature_id, `+shapeKind+`, e.limit_value, e.interval, e.rolling_window_seconds
  FROM plan_entitlements e JOIN features f ON f.id = e.feature_id WHERE e.plan_id = $1`, planID)
	if err != nil {
		return Classify(OpWrite, err)
	}
	current := map[string]currentEntitlement{}
	if _, err := collect(rows, func(r rowScanner) (struct{}, error) {
		var (
			key string
			e   currentEntitlement
		)
		err := r.Scan(&key, &e.featureID, &e.kind, &e.limit, &e.interval, &e.window)
		current[key] = e
		return struct{}{}, err
	}); err != nil {
		return err
	}
	kept := map[string]bool{}
	for i, e := range ents {
		kept[e.Feature] = true
		if was, ok := current[e.Feature]; ok {
			// A feature already in the plan may stay in it archived: the
			// plan is edited as one offer, and an archived feature is
			// refused at the data plane anyway (DR-015).
			if err := entitlementKind(was.kind, e, i); err != nil {
				return err
			}
			if sameEntitlement(was, e) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE plan_entitlements SET limit_value = $3, interval = $4, rolling_window_seconds = $5
 WHERE plan_id = $1 AND feature_id = $2`, planID, was.featureID, e.Limit, e.Interval, windowSeconds(e)); err != nil {
				return entitlementError(err, i)
			}
			continue
		}
		field := "entitlements[" + strconv.Itoa(i) + "].feature_key"
		var featureID, state, kind string
		err := tx.QueryRowContext(ctx, `SELECT id, state, `+shapeKind+` FROM features f WHERE key = $1 FOR SHARE`, e.Feature).Scan(&featureID, &state, &kind)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && state == "archived") {
			return &Error{Code: api.ErrorCodeValidationFailed, Field: field}
		}
		if err != nil {
			return Classify(OpWrite, err)
		}
		if err := entitlementKind(kind, e, i); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_entitlements (plan_id, feature_id, limit_value, interval, rolling_window_seconds)
 VALUES ($1, $2, $3, $4, $5)`, planID, featureID, e.Limit, e.Interval, windowSeconds(e)); err != nil {
			return entitlementError(err, i)
		}
		// Re-added inside the window it was retired in: the tenant never
		// lost it. Past that window: the pin is stale, and the tenant gets
		// the feature again at its next re-anchor.
		if _, err := tx.ExecContext(ctx, `DELETE FROM tenant_entitlement_pins n USING tenants t
 WHERE t.id = n.tenant_id AND t.plan_id = $1 AND n.feature_id = $2 AND n.retired_until <= $3`,
			planID, featureID, at.UTC()); err != nil {
			return Classify(OpWrite, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tenant_entitlement_pins n SET retired_limit = NULL, retired_until = NULL
  FROM tenants t
 WHERE t.id = n.tenant_id AND t.plan_id = $1 AND n.feature_id = $2`, planID, featureID); err != nil {
			return Classify(OpWrite, err)
		}
	}
	for key, was := range current {
		if kept[key] {
			continue
		}
		// A boolean feature is never pinned, so it has nothing to retire: it
		// leaves every tenant with the plan row (DR-050).
		if was.limit == nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM plan_entitlements WHERE plan_id = $1 AND feature_id = $2`,
				planID, was.featureID); err != nil {
				return Classify(OpWrite, err)
			}
			continue
		}
		if err := retire(ctx, tx, planID, was, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plan_entitlements WHERE plan_id = $1 AND feature_id = $2`,
			planID, was.featureID); err != nil {
			return Classify(OpWrite, err)
		}
	}
	return nil
}

func entitlementError(err error, i int) error {
	if c := Classify(OpWrite, err); errors.Is(c, ErrValidation) {
		return &Error{Code: api.ErrorCodeValidationFailed, Field: "entitlements[" + strconv.Itoa(i) + "]"}
	} else {
		return c
	}
}

// retire stamps every live pin of a removed feature on the plan's tenants
// with the limit it had and the end of the window at falls in.
func retire(ctx context.Context, tx *sql.Tx, planID string, was currentEntitlement, at time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT n.tenant_id, t.anchor_at, t.timezone, n.interval
  FROM tenant_entitlement_pins n JOIN tenants t ON t.id = n.tenant_id
 WHERE t.plan_id = $1 AND n.feature_id = $2 AND n.retired_limit IS NULL`, planID, was.featureID)
	if err != nil {
		return Classify(OpWrite, err)
	}
	type pinned struct {
		tenant   string
		anchor   time.Time
		zone     string
		interval string
	}
	pins, err := collect(rows, func(r rowScanner) (pinned, error) {
		var p pinned
		return p, r.Scan(&p.tenant, &p.anchor, &p.zone, &p.interval)
	})
	if err != nil {
		return err
	}
	var (
		finite []string
		until  []time.Time
		never  []string
	)
	for _, p := range pins {
		end, err := windowEnd(p.anchor, p.interval, p.zone, at)
		if err != nil {
			return err
		}
		if end == nil {
			never = append(never, p.tenant)
			continue
		}
		finite = append(finite, p.tenant)
		until = append(until, end.UTC())
	}
	if len(finite) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE tenant_entitlement_pins n SET retired_limit = $3, retired_until = u.until
  FROM unnest($1::uuid[], $2::timestamptz[]) AS u(tenant_id, until)
 WHERE n.tenant_id = u.tenant_id AND n.feature_id = $4`, finite, until, was.limit, was.featureID); err != nil {
			return Classify(OpWrite, err)
		}
	}
	if len(never) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE tenant_entitlement_pins SET retired_limit = $2
 WHERE tenant_id = ANY($1::uuid[]) AND feature_id = $3`, never, was.limit, was.featureID); err != nil {
			return Classify(OpWrite, err)
		}
	}
	return nil
}

// windowEnd is the end of the window at falls in, nil for `never`.
func windowEnd(anchor time.Time, interval, zone string, at time.Time) (*time.Time, error) {
	iv, err := parseInterval(interval)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("load zone: %w", err)
	}
	return cycle.CurrentWindow(anchor.UTC(), iv, loc, at).End, nil
}

func planTenants(ctx context.Context, q querier, planID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM tenants WHERE plan_id = $1 AND state <> 'deleted' ORDER BY id`, planID)
	if err != nil {
		return nil, Classify(OpWrite, err)
	}
	return collect(rows, func(r rowScanner) (string, error) {
		var id string
		return id, r.Scan(&id)
	})
}

// ---- impact

func (a *Admin) PlanCadences(ctx context.Context, id string) (api.Plan, []admin.Cadence, error) {
	p, err := getPlan(ctx, a.db, id, "")
	if err != nil {
		return api.Plan{}, nil, err
	}
	rows, err := a.db.QueryContext(ctx, `
SELECT t.anchor_at, t.timezone,
       coalesce(array_agg(DISTINCT n.interval::text) FILTER (WHERE n.interval IS NOT NULL AND f.state = 'active'), '{}')
  FROM tenants t
  LEFT JOIN tenant_entitlement_pins n ON n.tenant_id = t.id
  LEFT JOIN features f ON f.id = n.feature_id
 WHERE t.plan_id = $1 AND t.state <> 'deleted'
 GROUP BY t.id, t.anchor_at, t.timezone`, p.ID)
	if err != nil {
		return api.Plan{}, nil, fmt.Errorf("plan cadences: %w", err)
	}
	out, err := collect(rows, func(r rowScanner) (admin.Cadence, error) {
		var (
			c         admin.Cadence
			intervals []string
		)
		if err := r.Scan(&c.Anchor, &c.Timezone, &intervals); err != nil {
			return c, err
		}
		c.Anchor = c.Anchor.UTC()
		c.Intervals = intervals
		return c, nil
	})
	return p, out, err
}

// ---- apply-now and the plan change

func (a *Admin) ApplyPlan(ctx context.Context, actor admin.Actor, id string, in admin.ApplyInput) (admin.Applied, error) {
	var out admin.Applied
	err := a.tx(ctx, func(tx *sql.Tx) error {
		before, err := getPlan(ctx, tx, id, "FOR UPDATE OF p")
		if err != nil {
			return err
		}
		if before.State == api.ArchiveStateArchived {
			return &Error{Code: api.ErrorCodeValidationFailed, Field: "id"}
		}
		if int64(before.Version) != in.Version {
			return admin.ErrApplyMoved
		}
		planID := before.ID.String()
		// Every non-deleted tenant on the plan, suspended ones included: a
		// suspended tenant resumes on the anchor it has (INV-T2).
		rows, err := tx.QueryContext(ctx, `UPDATE tenants
   SET anchor_at = greatest($2::timestamptz, anchor_at + interval '1 millisecond'), version = version + 1, updated_at = now()
 WHERE plan_id = $1 AND state <> 'deleted'
 RETURNING id`, planID, in.At.UTC())
		if err != nil {
			return Classify(OpWrite, err)
		}
		ids, err := collect(rows, func(r rowScanner) (string, error) {
			var id string
			return id, r.Scan(&id)
		})
		if err != nil {
			return err
		}
		if in.Max >= 0 && int64(len(ids)) > in.Max {
			return admin.ErrApplyMoved
		}
		if err := repin(ctx, tx, ids); err != nil {
			return err
		}
		if err := pend(ctx, tx, ids, "apply_now", actor.RequestID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plans SET version = version + 1, updated_at = now() WHERE id = $1`, planID); err != nil {
			return Classify(OpWrite, err)
		}
		p, err := getPlan(ctx, tx, planID, "")
		if err != nil {
			return err
		}
		out = admin.Applied{Plan: p, Tenants: ids}
		applied := &api.PlanApplied{PlanKey: p.PlanKey, AffectedTenants: int64(len(ids)), AppliedAt: ts(in.At), Version: p.Version}
		return audit(ctx, tx, actor, "plan.apply_now", "plan", planID, &before, applied, in.Reason)
	})
	return out, err
}

// repin replaces the listed tenants' pins with what their plan offers now,
// each feature at the cadence an override sets or the plan's: the moment a
// tenant is anchored is the moment it takes the plan's feature set and
// cadences (D1). A boolean feature has no cadence and is not pinned; its
// inclusion is read from the plan as it stands (DR-050).
func repin(ctx context.Context, tx *sql.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tenant_entitlement_pins WHERE tenant_id = ANY($1::uuid[])`, ids); err != nil {
		return Classify(OpWrite, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tenant_entitlement_pins (tenant_id, feature_id, interval)
SELECT t.id, e.feature_id, coalesce(o.interval, e.interval)
  FROM tenants t
  JOIN plan_entitlements e ON e.plan_id = t.plan_id
  LEFT JOIN tenant_entitlement_overrides o ON o.tenant_id = t.id AND o.feature_id = e.feature_id
 WHERE t.id = ANY($1::uuid[]) AND e.interval IS NOT NULL`, ids); err != nil {
		return Classify(OpWrite, err)
	}
	return nil
}

// pend records a committed re-anchor for each listed tenant, at the anchor
// its row now holds. A later re-anchor of a tenant whose earlier one is still
// pending replaces it: only the newest anchor is worth rewriting to.
func pend(ctx context.Context, tx *sql.Tx, ids []string, kind, requestID string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO pending_reanchors (tenant_id, anchor_at, kind, request_id)
SELECT id, anchor_at, $2, $3 FROM tenants WHERE id = ANY($1::uuid[])
ON CONFLICT (tenant_id) DO UPDATE
   SET anchor_at = EXCLUDED.anchor_at, kind = EXCLUDED.kind, request_id = EXCLUDED.request_id, created_at = now()`,
		ids, kind, requestID)
	return Classify(OpWrite, err)
}

// changePlan is a tenant PATCH's plan change, inside its transaction: the new
// plan, a new anchor, new pins and a pending re-anchor (T-5, DR-013, D2). The
// tenant row is already locked.
func changePlan(ctx context.Context, tx *sql.Tx, tenantID, planKey string, at time.Time, requestID string) error {
	var planID, state string
	err := tx.QueryRowContext(ctx, `SELECT id, state FROM plans WHERE key = $1 FOR SHARE`, planKey).Scan(&planID, &state)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && state == "archived") {
		// An unknown plan and an archived one are both a plan_key the
		// tenant cannot be put on (P-4).
		return &Error{Code: api.ErrorCodeValidationFailed, Field: "plan_key"}
	}
	if err != nil {
		return Classify(OpWrite, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenants
   SET plan_id = $2, anchor_at = greatest($3::timestamptz, anchor_at + interval '1 millisecond')
 WHERE id = $1`, tenantID, planID, at.UTC()); err != nil {
		return Classify(OpWrite, err)
	}
	if err := repin(ctx, tx, []string{tenantID}); err != nil {
		return err
	}
	return pend(ctx, tx, []string{tenantID}, "plan_change", requestID)
}

// ---- the drain

// PendingReanchors counts the re-anchors committed and not yet applied to the
// fast store, the value of quotacore_reanchors_pending (IP-13 D10).
func PendingReanchors(ctx context.Context, conn *sql.DB) (int64, error) {
	var n int64
	err := conn.QueryRowContext(ctx, `SELECT count(*) FROM pending_reanchors`).Scan(&n)
	return n, err
}

const pendingRow = `SELECT tenant_id, anchor_at, kind, request_id FROM pending_reanchors WHERE tenant_id = $1`

func scanPending(r rowScanner) (admin.Reanchor, error) {
	var p admin.Reanchor
	err := r.Scan(&p.TenantID, &p.Anchor, &p.Kind, &p.RequestID)
	p.Anchor = p.Anchor.UTC()
	return p, err
}

// drainLocked finishes the pending re-anchor of a tenant whose row this
// transaction holds, if it has one: the rewrite, then the row's delete, which
// commits with the transaction or not at all.
func drainLocked(ctx context.Context, tx *sql.Tx, tenantID string, drain admin.DrainFunc) error {
	p, err := scanPending(tx.QueryRowContext(ctx, pendingRow, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return Classify(OpWrite, err)
	}
	t, err := loadTenantRow(ctx, tx, tenantID, "")
	if err != nil && !errors.Is(err, snapshot.ErrTenantUnknown) {
		return err
	}
	// A tenant deleted since has no balance worth rewriting.
	if err == nil {
		if err := drain(ctx, p, t); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM pending_reanchors WHERE tenant_id = $1`, tenantID)
	return Classify(OpWrite, err)
}

func (a *Admin) DrainReanchors(ctx context.Context, tenantIDs []string, max int, drain admin.DrainFunc) (int, error) {
	done := 0
	for done < max {
		var found bool
		err := a.tx(ctx, func(tx *sql.Tx) error {
			// The tenant row first, as every other writer takes it, and one
			// another drain or a write already holds is skipped.
			var id string
			err := tx.QueryRowContext(ctx, `SELECT t.id FROM tenants t JOIN pending_reanchors p ON p.tenant_id = t.id
 WHERE $1::uuid[] IS NULL OR t.id = ANY($1::uuid[])
 ORDER BY p.created_at, t.id LIMIT 1 FOR UPDATE OF t SKIP LOCKED`, nilIfEmpty(tenantIDs)).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return Classify(OpWrite, err)
			}
			found = true
			return drainLocked(ctx, tx, id, drain)
		})
		if err != nil {
			return done, err
		}
		if !found {
			return done, nil
		}
		done++
	}
	return done, nil
}

func nilIfEmpty(ids []string) any {
	if ids == nil {
		return nil
	}
	return ids
}

// ---- locked tenant writes

// lockForWrite is the front of every grant, set, force-rollover and override
// write: the tenant locked and refused once deleted (T-7), the feature known
// (404 otherwise), and the tenant's pending re-anchor finished, so the write
// lands on the hash the tenant's anchor describes (S4).
func lockForWrite(ctx context.Context, tx *sql.Tx, id, feature string, drain admin.DrainFunc) (api.Tenant, error) {
	t, err := lockTenant(ctx, tx, id)
	if err != nil {
		return api.Tenant{}, err
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM features WHERE key = $1)`, feature).Scan(&exists); err != nil {
		return api.Tenant{}, Classify(OpWrite, err)
	}
	if !exists {
		return api.Tenant{}, ErrFeatureNotFound
	}
	if err := drainLocked(ctx, tx, t.ID.String(), drain); err != nil {
		return api.Tenant{}, err
	}
	return t, nil
}

func (a *Admin) AdjustBalance(ctx context.Context, actor admin.Actor, id, feature string, drain admin.DrainFunc,
	fn admin.TenantWrite) (string, error) {
	var tenantID string
	err := a.tx(ctx, func(tx *sql.Tx) error {
		t, err := lockForWrite(ctx, tx, id, feature, drain)
		if err != nil {
			return err
		}
		tenantID = t.ID.String()
		loaded, err := loadTenantRow(ctx, tx, tenantID, "")
		if err != nil {
			return err
		}
		rec, err := fn(ctx, loaded)
		if err != nil {
			return err
		}
		return audit(ctx, tx, actor, rec.Action, "tenant", tenantID, rec.Before, rec.After, rec.Reason)
	})
	return tenantID, err
}

const overrideRow = `o.id, f.key, o.limit_value, o.interval, o.apply_at, o.version, o.created_at, o.updated_at`

func scanOverride(r rowScanner) (api.Override, error) {
	var (
		o                api.Override
		key, applyAt     string
		limit            sql.NullInt64
		interval         sql.NullString
		version          int64
		created, updated time.Time
	)
	if err := r.Scan(&o.ID, &key, &limit, &interval, &applyAt, &version, &created, &updated); err != nil {
		return api.Override{}, err
	}
	o.FeatureKey, o.ApplyAt, o.Version = api.FeatureKey(key), api.OverrideApplyAt(applyAt), api.Version(version)
	o.CreatedAt, o.UpdatedAt = ts(created), ts(updated)
	o.LimitValue = api.NilLimit{Null: true}
	if limit.Valid {
		o.LimitValue = api.NewNilLimit(api.Limit(limit.Int64))
	}
	o.Interval = api.NilResetInterval{Null: true}
	if interval.Valid {
		o.Interval = api.NewNilResetInterval(api.ResetInterval(interval.String))
	}
	return o, nil
}

func getOverride(ctx context.Context, tx *sql.Tx, tenantID, feature string) (*api.Override, error) {
	o, err := scanOverride(tx.QueryRowContext(ctx, `SELECT `+overrideRow+`
  FROM tenant_entitlement_overrides o JOIN features f ON f.id = o.feature_id
 WHERE o.tenant_id = $1 AND f.key = $2 FOR UPDATE OF o`, tenantID, feature))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, Classify(OpWrite, err)
	}
	return &o, nil
}

func (a *Admin) UpsertOverride(ctx context.Context, actor admin.Actor, id string, in admin.OverrideInput,
	drain admin.DrainFunc, fn func(context.Context, *snapshot.LoadedTenant) error) (api.Override, string, error) {
	var (
		out      api.Override
		tenantID string
	)
	err := a.tx(ctx, func(tx *sql.Tx) error {
		t, err := lockForWrite(ctx, tx, id, in.Feature, drain)
		if err != nil {
			return err
		}
		tenantID = t.ID.String()
		// An override cannot add a feature the plan does not include, nor
		// set one that is archived (DR-012, DR-015, INV-TE1).
		var featureID, kind string
		err = tx.QueryRowContext(ctx, `SELECT f.id, f.kind FROM features f
  JOIN plan_entitlements e ON e.feature_id = f.id
  JOIN tenants t ON t.plan_id = e.plan_id
 WHERE t.id = $1 AND f.key = $2 AND f.state = 'active'`, tenantID, in.Feature).Scan(&featureID, &kind)
		if errors.Is(err, sql.ErrNoRows) {
			return &Error{Code: api.ErrorCodeValidationFailed, Field: "feature_key"}
		}
		if err != nil {
			return Classify(OpWrite, err)
		}
		// A boolean entitlement has no limit or cadence to override (DR-050),
		// and a rolling one has no cycle balance; per-window overrides are
		// deferred (DR-052).
		if kind == "boolean" {
			return &Error{Code: api.ErrorCodeFeatureNotMetered, Field: "feature_key"}
		}
		if kind == "rolling" {
			return &Error{Code: api.ErrorCodeFeatureNotAnchored, Field: "feature_key"}
		}
		before, err := getOverride(ctx, tx, tenantID, in.Feature)
		if err != nil {
			return err
		}
		if in.Version != nil {
			if before == nil {
				return &Error{Code: api.ErrorCodeResourceVersionConflict}
			}
			if int64(before.Version) != *in.Version {
				return conflict(int64(before.Version))
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tenant_entitlement_overrides (tenant_id, feature_id, limit_value, interval, apply_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (tenant_id, feature_id) DO UPDATE
   SET limit_value = EXCLUDED.limit_value, interval = EXCLUDED.interval, apply_at = EXCLUDED.apply_at,
       version = tenant_entitlement_overrides.version + 1, updated_at = now()`,
			tenantID, featureID, in.Limit, in.Interval, in.ApplyAt); err != nil {
			return Classify(OpWrite, err)
		}
		after, err := getOverride(ctx, tx, tenantID, in.Feature)
		if err != nil || after == nil {
			return errors.Join(err, errors.New("the override just written is not there"))
		}
		out = *after
		loaded, err := loadTenantRow(ctx, tx, tenantID, "")
		if err != nil {
			return err
		}
		if err := fn(ctx, loaded); err != nil {
			return err
		}
		var prior json.Marshaler
		if before != nil {
			prior = before
		}
		return audit(ctx, tx, actor, "tenant.override_set", "tenant", tenantID, prior, after, "")
	})
	return out, tenantID, err
}

func (a *Admin) ClearOverride(ctx context.Context, actor admin.Actor, id, feature, applyAt string, drain admin.DrainFunc,
	fn func(context.Context, *snapshot.LoadedTenant) error) (string, bool, error) {
	var (
		tenantID string
		cleared  bool
	)
	err := a.tx(ctx, func(tx *sql.Tx) error {
		t, err := lockForWrite(ctx, tx, id, feature, drain)
		if err != nil {
			return err
		}
		tenantID = t.ID.String()
		before, err := getOverride(ctx, tx, tenantID, feature)
		if err != nil || before == nil {
			return err // S9: clearing what is not there writes nothing
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tenant_entitlement_overrides WHERE id = $1`, before.ID); err != nil {
			return Classify(OpWrite, err)
		}
		cleared = true
		loaded, err := loadTenantRow(ctx, tx, tenantID, "")
		if err != nil {
			return err
		}
		if err := fn(ctx, loaded); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "tenant.override_clear", "tenant", tenantID, before, nil, "")
	})
	return tenantID, cleared, err
}
