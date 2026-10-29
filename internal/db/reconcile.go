package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/quotacore/quotacore/internal/admin"
	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/reconcile"
)

// Reconciler is the Postgres side of reconciliation (IP-11 D10): the reads
// the verifier and the rebuild make, the pass's checkpoint and lock, the
// findings, and the reversal's transaction. It appends to usage_events and
// never updates or deletes a row of it (TestNoAuditOrLedgerRowIsRewritten).
// It implements reconcile.Ledger.
type Reconciler struct {
	db *sql.DB
}

var _ reconcile.Ledger = (*Reconciler)(nil)

func NewReconciler(db *sql.DB) *Reconciler { return &Reconciler{db: db} }

// reconcileLockKey is the session advisory lock one pass holds, so every
// instance can run the pass and only one runs it at a time.
const reconcileLockKey = 6067046871116144153

// Lock takes the pass's lock on a connection of its own, which release
// returns (reconcile.Ledger).
func (r *Reconciler) Lock(ctx context.Context) (func(), bool, error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var held bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, reconcileLockKey).Scan(&held); err != nil {
		conn.Close()
		return nil, false, fmt.Errorf("take the reconciliation lock: %w", err)
	}
	if !held {
		conn.Close()
		return nil, false, nil
	}
	return func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, reconcileLockKey)
		conn.Close()
	}, true, nil
}

// Checkpoint reads how far the pass has read; nothing yet is zero
// (reconcile.Ledger).
func (r *Reconciler) Checkpoint(ctx context.Context) (reconcile.Checkpoint, error) {
	var c reconcile.Checkpoint
	err := r.db.QueryRowContext(ctx, `SELECT events_seq, gaps_id FROM reconcile_checkpoint WHERE id`).
		Scan(&c.EventsSeq, &c.GapsID)
	if errors.Is(err, sql.ErrNoRows) {
		return reconcile.Checkpoint{}, nil
	}
	return c, err
}

// SaveCheckpoint records how far the pass has read (reconcile.Ledger).
func (r *Reconciler) SaveCheckpoint(ctx context.Context, c reconcile.Checkpoint) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO reconcile_checkpoint (id, events_seq, gaps_id, updated_at)
		VALUES (true, $1, $2, now())
		ON CONFLICT (id) DO UPDATE SET events_seq = GREATEST(reconcile_checkpoint.events_seq, EXCLUDED.events_seq),
			gaps_id = GREATEST(reconcile_checkpoint.gaps_id, EXCLUDED.gaps_id), updated_at = now()`,
		c.EventsSeq, c.GapsID)
	return err
}

// changedRows bounds how many ledger rows one pass reads past the
// checkpoint, so a backlog is worked off over several passes.
const changedRows = 100000

// Changed lists the tenant-features to verify (reconcile.Ledger). The
// checkpoint advances only through a run of settled rows: a row created
// after settled stops it, so a row committed late, by an instance whose
// queue was behind, is read by a later pass rather than skipped.
func (r *Reconciler) Changed(ctx context.Context, from reconcile.Checkpoint, settled time.Time, limit int) (
	[]reconcile.Changed, reconcile.Checkpoint, error) {
	next := from
	var unsettled sql.NullInt64
	if err := r.db.QueryRowContext(ctx, `SELECT min(seq) FROM usage_events WHERE seq > $1 AND created_at >= $2`,
		from.EventsSeq, settled.UTC()).Scan(&unsettled); err != nil {
		return nil, from, err
	}
	var top sql.NullInt64
	if err := r.db.QueryRowContext(ctx, `SELECT max(seq) FROM (SELECT seq FROM usage_events WHERE seq > $1
		ORDER BY seq LIMIT $2) s`, from.EventsSeq, changedRows).Scan(&top); err != nil {
		return nil, from, err
	}
	upto := from.EventsSeq
	if top.Valid {
		upto = top.Int64
	}
	if unsettled.Valid && unsettled.Int64-1 < upto {
		upto = unsettled.Int64 - 1
	}
	seen := map[reconcile.Subject]int{}
	var out []reconcile.Changed
	add := func(sub reconcile.Subject, since time.Time) {
		if i, ok := seen[sub]; ok {
			if since.Before(out[i].Since) {
				out[i].Since = since
			}
			return
		}
		seen[sub] = len(out)
		out = append(out, reconcile.Changed{Subject: sub, Since: since})
	}
	if upto > from.EventsSeq {
		rows, err := r.db.QueryContext(ctx, `SELECT u.tenant_id::text, f.key, min(u.created_at)
			  FROM usage_events u JOIN features f ON f.id = u.feature_id
			 WHERE u.seq > $1 AND u.seq <= $2
			 GROUP BY u.tenant_id, f.key
			 ORDER BY min(u.created_at)
			 LIMIT $3`, from.EventsSeq, upto, limit)
		if err != nil {
			return nil, from, err
		}
		for rows.Next() {
			var sub reconcile.Subject
			var since time.Time
			if err := rows.Scan(&sub.TenantID, &sub.Feature, &since); err != nil {
				rows.Close()
				return nil, from, err
			}
			add(sub, since)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, from, err
		}
		next.EventsSeq = upto
	}
	// A gap names a tenant-feature that lost an event, which may have no
	// later row to bring it here (IP-11 D8).
	rows, err := r.db.QueryContext(ctx, `SELECT id, coalesce(tenant_id::text, ''), coalesce(feature_key, ''), first_at
		  FROM usage_event_gaps WHERE id > $1 AND created_at < $2 ORDER BY id LIMIT $3`, from.GapsID, settled.UTC(), limit)
	if err != nil {
		return nil, from, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var sub reconcile.Subject
		var since time.Time
		if err := rows.Scan(&id, &sub.TenantID, &sub.Feature, &since); err != nil {
			return nil, from, err
		}
		next.GapsID = id
		if sub.TenantID != "" && sub.Feature != "" {
			add(sub, since)
		}
	}
	return out, next, rows.Err()
}

// ledgerView is one tenant-feature's ledger as the verifier reads it: its
// rows, and the duplicates the store applied and the table refused, which
// are part of it until they are reversed (IP-11 D1). A duplicate sorts by
// the instant its request was accepted, with seq zero.
const ledgerView = `
SELECT id, seq, event_type, delta, balance_after, cycle_index, cycle_start, created_at FROM (
  SELECT u.id::text AS id, u.seq, u.event_type::text AS event_type, u.delta, u.balance_after, u.cycle_index,
         u.cycle_start, u.created_at
    FROM usage_events u
   WHERE u.tenant_id = $1::text::uuid AND u.feature_id = (SELECT id FROM features WHERE key = $2::text)
  UNION ALL
  SELECT d.event_id::text, 0, d.payload->>'event_type', (d.payload->>'delta')::bigint,
         (d.payload->>'balance_after')::bigint, (d.payload->>'cycle_index')::bigint,
         (d.payload->>'cycle_start')::timestamptz, (d.payload->>'occurred_at')::timestamptz
    FROM usage_event_dead_letters d
   WHERE d.reason = 'duplicate_idempotency_key' AND (d.resolution IS NULL OR d.resolution = 'referred')
     AND d.payload->>'tenant_id' = $1::text AND d.payload->>'feature_key' = $2::text
) v`

// Rows reads a tenant-feature's ledger from since on, and the row before
// since (reconcile.Ledger).
func (r *Reconciler) Rows(ctx context.Context, sub reconcile.Subject, since time.Time) ([]reconcile.Row, *reconcile.Row, error) {
	if since.IsZero() {
		rows, err := r.scan(ctx, ledgerView+` ORDER BY created_at, seq`, sub.TenantID, sub.Feature)
		return rows, nil, err
	}
	rows, err := r.scan(ctx, ledgerView+` WHERE created_at >= $3 ORDER BY created_at, seq`,
		sub.TenantID, sub.Feature, since.UTC())
	if err != nil {
		return nil, nil, err
	}
	prev, err := r.scan(ctx, ledgerView+` WHERE created_at < $3 ORDER BY created_at DESC, seq DESC LIMIT 1`,
		sub.TenantID, sub.Feature, since.UTC())
	if err != nil || len(prev) == 0 {
		return rows, nil, err
	}
	return rows, &prev[0], nil
}

func (r *Reconciler) scan(ctx context.Context, q string, args ...any) ([]reconcile.Row, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reconcile.Row
	for rows.Next() {
		var row reconcile.Row
		var start sql.NullTime
		if err := rows.Scan(&row.ID, &row.Seq, &row.Type, &row.Delta, &row.After, &row.CycleIndex, &start,
			&row.At); err != nil {
			return nil, err
		}
		if start.Valid {
			s := start.Time.UTC()
			row.CycleStart = &s
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Duplicates lists the unresolved duplicate dead letters, oldest first
// (reconcile.Ledger).
func (r *Reconciler) Duplicates(ctx context.Context, limit int) ([]reconcile.Duplicate, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT d.event_id::text, d.payload, coalesce(t.external_id, ''), t.id IS NULL OR t.state = 'deleted'
  FROM usage_event_dead_letters d
  LEFT JOIN tenants t ON t.id::text = d.payload->>'tenant_id'
 WHERE d.reason = 'duplicate_idempotency_key' AND d.resolved_at IS NULL
 ORDER BY d.created_at, d.event_id
 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reconcile.Duplicate
	for rows.Next() {
		var id string
		var body []byte
		var d reconcile.Duplicate
		if err := rows.Scan(&id, &body, &d.TenantExternal, &d.TenantDeleted); err != nil {
			return nil, err
		}
		var p payload
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("dead letter %s: %w", id, err)
		}
		d.EventID, d.TenantID, d.Feature, d.Type = id, p.TenantID, p.FeatureKey, p.Type
		d.Amount, d.ClientKey, d.At = p.Amount, p.ClientKey, p.OccurredAt
		out = append(out, d)
	}
	return out, rows.Err()
}

// reconcileActor is the audit actor of a reversal: the service itself, with
// no key (data-model.md section 2.8).
const reconcileActor = "system:reconcile"

// Reverse commits a reversal in one transaction (reconcile.Ledger, IP-11 D4):
// the duplicate appended with its key moved to reconciles_key, the opening
// the refund's transition wrote if it rolled, the system refund, the
// tenant.reverse_duplicate audit row, and the dead letter resolved. A crash
// before the commit leaves the dead letter unresolved, and the next pass's
// refund is a replay under the same key, so the reversal is issued once.
func (r *Reconciler) Reverse(ctx context.Context, rv reconcile.Reversal) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var body []byte
	var resolved sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT payload, resolved_at FROM usage_event_dead_letters
		WHERE event_id = $1::uuid AND reason = 'duplicate_idempotency_key' FOR UPDATE`, rv.EventID).Scan(&body, &resolved)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && resolved.Valid) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return false, fmt.Errorf("dead letter %s: %w", rv.EventID, err)
	}
	dup, err := p.event()
	if err != nil {
		return false, err
	}
	dup.ReconcilesKey, dup.ClientKey = dup.ClientKey, ""
	events := []dataplane.Event{dup}
	at := rv.Refunded.At
	if at.IsZero() {
		at = time.Now()
	}
	if ro := rv.Rolled; ro != nil {
		id, err := uuid.NewV7()
		if err != nil {
			return false, err
		}
		events = append(events, dataplane.Event{ID: id, RequestID: rv.RequestID, TenantID: rv.TenantID,
			FeatureKey: rv.Feature, Type: "cycle_rolled_over", Source: "system", Amount: max(ro.Delta, -ro.Delta),
			Delta: ro.Delta, Balance: ro.Balance, CycleIndex: ro.CycleIndex, CycleStart: ro.CycleStart, OccurredAt: at})
	}
	refundID, err := uuid.NewV7()
	if err != nil {
		return false, err
	}
	events = append(events, dataplane.Event{ID: refundID, RequestID: rv.RequestID, TenantID: rv.TenantID,
		FeatureKey: rv.Feature, Type: "refunded", Source: "system", Amount: rv.Amount, Delta: rv.Amount,
		Balance: rv.Balance, CycleIndex: rv.CycleIndex, CycleStart: rv.CycleStart, ReconcilesKey: rv.ClientKey,
		OccurredAt: at})
	out, err := appendIn(ctx, tx, events)
	if err != nil {
		return false, err
	}
	if out.Written != len(events) {
		return false, fmt.Errorf("the ledger took %d of the reversal's %d rows for dead letter %s", out.Written,
			len(events), rv.EventID)
	}
	after, err := json.Marshal(map[string]any{
		"duplicate_event_id": rv.EventID,
		"reversal_event_id":  refundID.String(),
		"idempotency_key":    rv.ClientKey,
		"feature_key":        rv.Feature,
		"amount":             rv.Amount,
		"refund":             rv.Outcome,
	})
	if err != nil {
		return false, err
	}
	if err := audit(ctx, tx, admin.Actor{Name: reconcileActor, RequestID: rv.RequestID}, "tenant.reverse_duplicate",
		"tenant", rv.TenantID, nil, json.RawMessage(after),
		"a second deduction under one idempotency key, reversed by reconciliation (DR-049)"); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usage_event_dead_letters SET resolved_at = now(), resolution = 'reversed'
		WHERE event_id = $1::uuid`, rv.EventID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Opened appends the opening a refused reversal's transition wrote
// (reconcile.Ledger).
func (r *Reconciler) Opened(ctx context.Context, d reconcile.Duplicate, requestID string, ro reconcile.RolledOver,
	at time.Time) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := appendIn(ctx, tx, []dataplane.Event{{ID: id, RequestID: requestID, TenantID: d.TenantID,
		FeatureKey: d.Feature, Type: "cycle_rolled_over", Source: "system", Amount: max(ro.Delta, -ro.Delta),
		Delta: ro.Delta, Balance: ro.Balance, CycleIndex: ro.CycleIndex, CycleStart: ro.CycleStart,
		OccurredAt: at}}); err != nil {
		return err
	}
	return tx.Commit()
}

// Resolve closes a dead letter without a reversal (reconcile.Ledger).
func (r *Reconciler) Resolve(ctx context.Context, eventID, resolution string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE usage_event_dead_letters SET resolved_at = now(), resolution = $2
		WHERE event_id = $1::uuid AND resolved_at IS NULL`, eventID, resolution)
	return err
}

// Record inserts a finding and reports whether it is new (reconcile.Ledger).
func (r *Reconciler) Record(ctx context.Context, f reconcile.Finding, foundBy string) (bool, error) {
	var tenant any
	if _, err := uuid.Parse(f.TenantID); err == nil {
		tenant = f.TenantID
	}
	var index, expected, actual any
	var start pgtype.Timestamptz
	if f.Cycle != nil {
		index = f.Cycle.Index
		start = pgtype.Timestamptz{Time: f.Cycle.Start.UTC(), Valid: true}
	}
	if f.Expected != nil {
		expected = *f.Expected
	}
	if f.Actual != nil {
		actual = *f.Actual
	}
	res, err := r.db.ExecContext(ctx, `INSERT INTO reconcile_findings (fingerprint, kind, tenant_id, feature_key,
		cycle_index, cycle_start, expected, actual, detail, found_by)
		VALUES ($1, $2, $3::uuid, NULLIF($4, ''), $5, $6, $7, $8, $9, $10) ON CONFLICT (fingerprint) DO NOTHING`,
		f.Fingerprint(), f.Kind, tenant, f.Feature, index, start, expected, actual, f.Detail, foundBy)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// StoredFinding is one reconcile_findings row, for reconcile-report.
type StoredFinding struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	TenantID   *string    `json:"tenant_id"`
	FeatureKey *string    `json:"feature_key"`
	CycleIndex *int64     `json:"cycle_index"`
	CycleStart *time.Time `json:"cycle_start"`
	Expected   *int64     `json:"expected"`
	Actual     *int64     `json:"actual"`
	Detail     string     `json:"detail"`
	FoundBy    string     `json:"found_by"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Findings lists the findings since an instant, newest first (S5).
func (r *Reconciler) Findings(ctx context.Context, since time.Time, limit int) ([]StoredFinding, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id::text, kind, tenant_id::text, feature_key, cycle_index, cycle_start,
		expected, actual, detail, found_by, created_at FROM reconcile_findings
		WHERE created_at >= $1 ORDER BY created_at DESC, id LIMIT $2`, since.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredFinding
	for rows.Next() {
		var f StoredFinding
		if err := rows.Scan(&f.ID, &f.Kind, &f.TenantID, &f.FeatureKey, &f.CycleIndex, &f.CycleStart, &f.Expected,
			&f.Actual, &f.Detail, &f.FoundBy, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
