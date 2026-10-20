package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/ledger"
	"github.com/quotacore/quotacore/internal/observability"
)

// Ledger is the Postgres side of the ledger writer: the batch insert into
// usage_events and the dead-letter table, and nothing else
// (TestTheWriterCanOnlyAppend). It implements ledger.Appender.
type Ledger struct {
	db *sql.DB
}

var _ ledger.Appender = (*Ledger)(nil)

func NewLedger(db *sql.DB) *Ledger { return &Ledger{db: db} }

// The batch insert. Every event is one element of each array, and the insert
// joins the feature by its key and the tenant by its id, so an event that names
// neither is left out rather than failing the statement on a foreign key. ON
// CONFLICT DO NOTHING covers both the primary key (a redelivered event_id, which
// changes nothing) and the partial unique index (a second deduction under one
// idempotency key, which is a detected duplicate, DR-030). The rows go in in
// the order they were queued, which is the order seq records.
const insertEvents = `
INSERT INTO usage_events (id, tenant_id, feature_id, cycle_index, delta, balance_after, source, event_type,
                          request_id, idempotency_key, metadata, created_at, cycle_start)
SELECT v.id, v.tenant_id, f.id, v.cycle_index, v.delta, v.balance_after, v.source::event_source,
       v.event_type::event_type, v.request_id, NULLIF(v.idempotency_key, ''),
       COALESCE(NULLIF(v.metadata, ''), '{}')::jsonb, v.created_at, v.cycle_start
  FROM unnest($1::uuid[], $2::uuid[], $3::text[], $4::bigint[], $5::bigint[], $6::bigint[], $7::text[],
              $8::text[], $9::text[], $10::text[], $11::text[], $12::timestamptz[], $13::timestamptz[])
       WITH ORDINALITY AS v(id, tenant_id, feature_key, cycle_index, delta, balance_after, source, event_type,
                            request_id, idempotency_key, metadata, created_at, cycle_start, ord)
  JOIN features f ON f.key = v.feature_key
  JOIN tenants t ON t.id = v.tenant_id
 WHERE v.delta <> 0
 ORDER BY v.ord
ON CONFLICT DO NOTHING
RETURNING id`

// Append writes the batch in one transaction (ledger.Appender).
func (l *Ledger) Append(ctx context.Context, events []dataplane.Event) (ledger.Appended, error) {
	var out ledger.Appended
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	out, err = appendIn(ctx, tx, events)
	if err != nil {
		return ledger.Appended{}, err
	}
	if err := tx.Commit(); err != nil {
		return ledger.Appended{}, err
	}
	return out, nil
}

func appendIn(ctx context.Context, tx *sql.Tx, events []dataplane.Event) (ledger.Appended, error) {
	var out ledger.Appended
	n := len(events)
	var (
		ids, tenants, keys, sources, types, requests, clients, metas = make([]string, n), make([]string, n),
			make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
		indexes, deltas, balances = make([]int64, n), make([]int64, n), make([]int64, n)
		at, starts                = make([]time.Time, n), make([]pgtype.Timestamptz, n)
	)
	for i, e := range events {
		ids[i], tenants[i], keys[i], sources[i], types[i] = e.ID.String(), e.TenantID, e.FeatureKey, e.Source, e.Type
		requests[i], clients[i], metas[i] = e.RequestID, e.ClientKey, string(e.Metadata)
		indexes[i], deltas[i], balances[i], at[i] = e.CycleIndex, e.Delta, e.Balance, e.OccurredAt.UTC()
		if !e.CycleStart.IsZero() {
			starts[i] = pgtype.Timestamptz{Time: e.CycleStart.UTC(), Valid: true}
		}
		if at[i].IsZero() {
			at[i] = time.Now().UTC()
		}
	}
	rows, err := tx.QueryContext(ctx, insertEvents, ids, tenants, keys, indexes, deltas, balances, sources, types,
		requests, clients, metas, at, starts)
	if err != nil {
		return out, err
	}
	inserted := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		inserted[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.Written = len(inserted)
	if out.Written == n {
		return out, nil
	}
	var missing []dataplane.Event
	var missingIDs []string
	for _, e := range events {
		if !inserted[e.ID.String()] {
			missing = append(missing, e)
			missingIDs = append(missingIDs, e.ID.String())
		}
	}
	present, err := idSet(ctx, tx, `SELECT id::text FROM usage_events WHERE id = ANY($1::uuid[])`, missingIDs)
	if err != nil {
		return out, err
	}
	seen := map[string]bool{}
	for _, e := range missing {
		id := e.ID.String()
		if present[id] || seen[id] {
			// A redelivery, or the same event twice in one batch.
			out.Present++
			seen[id] = true
			continue
		}
		seen[id] = true
		reason, why, err := refusal(ctx, tx, e)
		if err != nil {
			return out, err
		}
		created, err := deadLetterIn(ctx, tx, e, reason, why)
		if err != nil {
			return out, err
		}
		if created {
			out.Dead = append(out.Dead, ledger.Dead{ID: e.ID, Reason: reason})
		}
	}
	return out, nil
}

func idSet(ctx context.Context, tx *sql.Tx, query string, ids []string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, query, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// refusal explains why an event the insert left out was refused. A tenant and
// a feature that both resolve, with nothing else wrong, leave one cause: the
// partial unique index, which is a second deduction under one idempotency key
// (FS-22) and is named with the first deduction's event_id for reconciliation.
func refusal(ctx context.Context, tx *sql.Tx, e dataplane.Event) (reason, why string, err error) {
	if e.Delta == 0 {
		return observability.DeadRejected, "the delta is zero (usage_events_delta_nonzero)", nil
	}
	var feature, tenant bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM features WHERE key = $1),
		EXISTS (SELECT 1 FROM tenants WHERE id = $2::uuid)`, e.FeatureKey, e.TenantID).Scan(&feature, &tenant); err != nil {
		return "", "", err
	}
	switch {
	case !tenant:
		return observability.DeadRejected, "tenant_id resolves to no tenant", nil
	case !feature:
		return observability.DeadRejected, "feature_key resolves to no feature", nil
	}
	if e.ClientKey != "" && (e.Type == "consumed" || e.Type == "refunded") {
		var first string
		err := tx.QueryRowContext(ctx, `SELECT id::text FROM usage_events
			WHERE tenant_id = $1::uuid AND idempotency_key = $2 AND event_type IN ('consumed', 'refunded')`,
			e.TenantID, e.ClientKey).Scan(&first)
		switch {
		case err == nil:
			return observability.DeadDuplicate,
				"a second " + e.Type + " under this idempotency key; the first is event " + first, nil
		case !errors.Is(err, sql.ErrNoRows):
			return "", "", err
		}
	}
	return observability.DeadRejected, "the ledger did not take the row", nil
}

// payload is a dead letter's copy of its event, everything needed to append it
// later exactly as it would have been appended now.
type payload struct {
	ID         string          `json:"event_id"`
	RequestID  string          `json:"request_id"`
	TenantID   string          `json:"tenant_id"`
	FeatureKey string          `json:"feature_key"`
	Type       string          `json:"event_type"`
	Source     string          `json:"source"`
	Amount     int64           `json:"amount"`
	Delta      int64           `json:"delta"`
	Balance    int64           `json:"balance_after"`
	CycleIndex int64           `json:"cycle_index"`
	CycleStart *time.Time      `json:"cycle_start,omitempty"`
	ClientKey  string          `json:"idempotency_key,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	OccurredAt time.Time       `json:"occurred_at"`
}

func toPayload(e dataplane.Event) payload {
	p := payload{ID: e.ID.String(), RequestID: e.RequestID, TenantID: e.TenantID, FeatureKey: e.FeatureKey,
		Type: e.Type, Source: e.Source, Amount: e.Amount, Delta: e.Delta, Balance: e.Balance,
		CycleIndex: e.CycleIndex, ClientKey: e.ClientKey, Metadata: e.Metadata, OccurredAt: e.OccurredAt.UTC()}
	if !e.CycleStart.IsZero() {
		cs := e.CycleStart.UTC()
		p.CycleStart = &cs
	}
	if len(p.Metadata) > 0 && !json.Valid(p.Metadata) {
		p.Metadata = nil
	}
	return p
}

func (p payload) event() (dataplane.Event, error) {
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return dataplane.Event{}, err
	}
	e := dataplane.Event{ID: id, RequestID: p.RequestID, TenantID: p.TenantID, FeatureKey: p.FeatureKey,
		Type: p.Type, Source: p.Source, Amount: p.Amount, Delta: p.Delta, Balance: p.Balance,
		CycleIndex: p.CycleIndex, ClientKey: p.ClientKey, Metadata: p.Metadata, OccurredAt: p.OccurredAt}
	if p.CycleStart != nil {
		e.CycleStart = *p.CycleStart
	}
	return e, nil
}

func deadLetterIn(ctx context.Context, tx *sql.Tx, e dataplane.Event, reason, why string) (bool, error) {
	body, err := json.Marshal(toPayload(e))
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO usage_event_dead_letters (event_id, reason, payload, error)
		VALUES ($1::uuid, $2, $3::jsonb, $4) ON CONFLICT (event_id) DO NOTHING`, e.ID.String(), reason, string(body), why)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeadLetter keeps one event whose insert the database refused outright
// (ledger.Appender).
func (l *Ledger) DeadLetter(ctx context.Context, e dataplane.Event, cause error) (bool, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	why := "refused"
	if cause != nil {
		why = cause.Error()
	}
	created, err := deadLetterIn(ctx, tx, e, observability.DeadRejected, why)
	if err != nil {
		return false, err
	}
	return created, tx.Commit()
}

// Permanent reports whether err is the database refusing the statement, which
// a retry would meet again: a data exception (class 22), an integrity
// violation (23) or a syntax or access error (42). Anything else, a lost
// connection or a timeout among them, is transient (ledger.Appender).
func (l *Ledger) Permanent(err error) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || len(pg.Code) < 2 {
		return false
	}
	switch pg.Code[:2] {
	case "22", "23", "42":
		return true
	}
	return false
}

// ReplayRejected re-attempts up to limit rejected dead letters, oldest first,
// each in its own transaction: one the ledger now takes is appended and its
// dead letter deleted together, one found already present is deleted, and one
// still refused has its attempts raised and its reason and error brought up to
// date. A duplicate is never replayed: it is reconciliation's input (DR-049).
// It reports how many were appended.
func (l *Ledger) ReplayRejected(ctx context.Context, limit int) (int, error) {
	// Fewest attempts first, so a row that keeps failing does not starve the
	// ones behind it; each is attempted once per call.
	rows, err := l.db.QueryContext(ctx, `SELECT event_id::text FROM usage_event_dead_letters
		WHERE reason = 'rejected' ORDER BY attempts, created_at LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	replayed := 0
	for _, id := range ids {
		done, _, err := l.replayOne(ctx, id)
		if err != nil {
			return replayed, err
		}
		if done {
			replayed++
		}
	}
	return replayed, nil
}

func (l *Ledger) replayOne(ctx context.Context, id string) (appended, found bool, err error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var body []byte
	// SKIP LOCKED: a row another instance is replaying is that instance's.
	err = tx.QueryRowContext(ctx, `SELECT payload FROM usage_event_dead_letters
		WHERE event_id = $1::uuid AND reason = 'rejected' FOR UPDATE SKIP LOCKED`, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return false, true, l.bump(ctx, tx, id, observability.DeadRejected, "unreadable payload: "+err.Error())
	}
	e, err := p.event()
	if err != nil {
		return false, true, l.bump(ctx, tx, id, observability.DeadRejected, "unreadable payload: "+err.Error())
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT replay`); err != nil {
		return false, true, err
	}
	out, err := appendIn(ctx, tx, []dataplane.Event{e})
	if err != nil {
		if !l.Permanent(err) {
			return false, true, err
		}
		if _, rerr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT replay`); rerr != nil {
			return false, true, rerr
		}
		return false, true, l.bump(ctx, tx, id, observability.DeadRejected, err.Error())
	}
	if out.Written == 1 || out.Present == 1 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM usage_event_dead_letters WHERE event_id = $1::uuid`, id); err != nil {
			return false, true, err
		}
		return out.Written == 1, true, tx.Commit()
	}
	// Still refused: appendIn found the row already dead-lettered and left
	// it; record the attempt with today's reason.
	reason, why, err := refusal(ctx, tx, e)
	if err != nil {
		return false, true, err
	}
	return false, true, l.bump(ctx, tx, id, reason, why)
}

func (l *Ledger) bump(ctx context.Context, tx *sql.Tx, id, reason, why string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE usage_event_dead_letters
		SET attempts = attempts + 1, reason = $2, error = $3 WHERE event_id = $1::uuid`, id, reason, why); err != nil {
		return err
	}
	return tx.Commit()
}

// DeadLetterCounts is the dead letters outstanding by reason, for the gauge.
func (l *Ledger) DeadLetterCounts(ctx context.Context) (map[string]int, error) {
	out := map[string]int{observability.DeadDuplicate: 0, observability.DeadRejected: 0}
	rows, err := l.db.QueryContext(ctx, `SELECT reason, count(*) FROM usage_event_dead_letters GROUP BY reason`)
	if err != nil {
		return nil, fmt.Errorf("count dead letters: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			return nil, err
		}
		out[reason] = n
	}
	return out, rows.Err()
}
