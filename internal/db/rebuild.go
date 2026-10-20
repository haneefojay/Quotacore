package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/quotacore/quotacore/internal/admin"
	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/reconcile"
)

// The reads `quotacore verify-all`, `verify-tenant` and `rebuild` make, and
// the rebuild's one transaction (IP-11 D2b, D7). Like the rest of the
// Reconciler they append and never rewrite a ledger row.

// Tenants lists the id of every tenant that is not deleted.
func (r *Reconciler) Tenants(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id::text FROM tenants WHERE state <> 'deleted' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Subjects lists every tenant-feature the ledger has a row or a dead letter
// for, of one tenant, or of every tenant that is not deleted when tenantID is
// empty.
func (r *Reconciler) Subjects(ctx context.Context, tenantID string) ([]reconcile.Subject, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT DISTINCT s.tenant_id, s.feature_key FROM (
  SELECT u.tenant_id::text AS tenant_id, f.key AS feature_key
    FROM usage_events u JOIN features f ON f.id = u.feature_id
  UNION
  SELECT d.payload->>'tenant_id', d.payload->>'feature_key' FROM usage_event_dead_letters d
) s JOIN tenants t ON t.id::text = s.tenant_id
 WHERE t.state <> 'deleted' AND ($1 = '' OR s.tenant_id = $1)
 ORDER BY 1, 2`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reconcile.Subject
	for rows.Next() {
		var s reconcile.Subject
		if err := rows.Scan(&s.TenantID, &s.Feature); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Losses reads what the ledger records it may have lost (IP-11 D8): every gap,
// and every writer session left open whose last sight is older than stale,
// which is a crash that may have lost everything it accepted from its
// settled instant on. A crash covers every tenant until its last sight plus
// stale, the longest it can have gone on accepting unseen.
func (r *Reconciler) Losses(ctx context.Context, now time.Time, stale time.Duration) ([]reconcile.Loss, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT coalesce(tenant_id::text, ''), coalesce(feature_key, ''), reason, first_at, last_at FROM usage_event_gaps
UNION ALL
SELECT '', '', 'crash', settled_through, last_seen_at + make_interval(secs => $2)
  FROM ledger_sessions WHERE closed_at IS NULL AND last_seen_at < $1
ORDER BY 4`, now.Add(-stale).UTC(), stale.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reconcile.Loss
	for rows.Next() {
		var l reconcile.Loss
		if err := rows.Scan(&l.TenantID, &l.Feature, &l.Reason, &l.From, &l.To); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Rejected counts the unresolved rejected dead letters by tenant-feature: the
// events the store applied and the ledger refused.
func (r *Reconciler) Rejected(ctx context.Context) (map[reconcile.Subject]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT payload->>'tenant_id', payload->>'feature_key', count(*)
		  FROM usage_event_dead_letters WHERE reason = 'rejected' AND resolved_at IS NULL GROUP BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[reconcile.Subject]int{}
	for rows.Next() {
		var s reconcile.Subject
		var n int
		if err := rows.Scan(&s.TenantID, &s.Feature, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// ConfigChanged is the last instant an audited change to the tenant's limits
// or to its plan was made, after the tenant's creation, or zero. A rebuild
// reads it for a feature with no rows, whose limit no row records (D7).
func (r *Reconciler) ConfigChanged(ctx context.Context, tenantID, planID string) (time.Time, error) {
	var at *time.Time
	err := r.db.QueryRowContext(ctx, `SELECT max(created_at) FROM audit_log
		WHERE (subject_type = 'tenant' AND subject_id::text = $1 AND action IN ('tenant.change_plan', 'tenant.update',
		        'tenant.override_set', 'tenant.override_clear'))
		   OR ($2 <> '' AND subject_type = 'plan' AND subject_id::text = $2 AND action IN ('plan.update', 'plan.apply_now'))`,
		tenantID, planID).Scan(&at)
	if err != nil || at == nil {
		return time.Time{}, err
	}
	return *at, nil
}

// rebuildActor names the command that wrote a hash, by host.
func rebuildActor(host string) string { return "cli:" + host }

// Rebuilt is one tenant-feature a rebuild wrote, for its audit row.
type Rebuilt struct {
	Feature string           `json:"feature_key"`
	Class   string           `json:"class"`
	Action  string           `json:"action"`
	State   *reconcile.State `json:"written"`
	// Estimate is the ledger's, which --set departs from.
	Estimate *reconcile.State `json:"estimate,omitempty"`
}

// RecordRebuild commits what a rebuild wrote for one tenant in one
// transaction: the tenant.rebuild audit row (S6), and for each --set the
// `set` row whose delta is the value less the ledger's estimate, so the cycle
// still reconciles (IP-11 D7). The store writes came first, and were
// create-only; this records them.
func (r *Reconciler) RecordRebuild(ctx context.Context, host, requestID, tenantID string, written []Rebuilt,
	reason string, at time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var events []dataplane.Event
	for _, w := range written {
		if w.Action != "set" || w.State == nil {
			continue
		}
		from := w.State.Limit
		if w.Estimate != nil {
			from = w.Estimate.Balance
		}
		delta := w.State.Balance - from
		if delta == 0 {
			continue
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		events = append(events, dataplane.Event{ID: id, RequestID: requestID, TenantID: tenantID, FeatureKey: w.Feature,
			Type: "set", Source: "admin", Amount: max(delta, -delta), Delta: delta, Balance: w.State.Balance,
			CycleIndex: w.State.Window.Index, CycleStart: w.State.Window.Start, OccurredAt: at})
	}
	if len(events) > 0 {
		out, err := appendIn(ctx, tx, events)
		if err != nil {
			return err
		}
		if out.Written != len(events) {
			return fmt.Errorf("the ledger took %d of the rebuild's %d set rows", out.Written, len(events))
		}
	}
	after, err := json.Marshal(map[string]any{"features": written})
	if err != nil {
		return err
	}
	if err := audit(ctx, tx, admin.Actor{Name: rebuildActor(host), RequestID: requestID}, "tenant.rebuild", "tenant",
		tenantID, nil, json.RawMessage(after), reason); err != nil {
		return err
	}
	return tx.Commit()
}
