package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/observability"
)

// ledgerFixture is a migrated database with one tenant on one feature.
func ledgerFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db := migratedDatabase(t)
	ctx := context.Background()
	var tenant string
	if err := db.QueryRowContext(ctx, `
WITH f AS (INSERT INTO features (key, name, unit) VALUES ('tokens', 'Tokens', 'token') RETURNING id),
     p AS (INSERT INTO plans (key, name) VALUES ('p', 'P') RETURNING id),
     e AS (INSERT INTO plan_entitlements (plan_id, feature_id, limit_value, interval)
           SELECT p.id, f.id, 1000, 'monthly' FROM p, f)
INSERT INTO tenants (external_id, display_name, plan_id, anchor_at)
SELECT 'acme', 'Acme', id, now() FROM p RETURNING id`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	return db, tenant
}

func newEvent(t *testing.T, tenant, typ, client string, delta, balance int64) dataplane.Event {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return dataplane.Event{ID: id, RequestID: "01J8Z6M2QK9V3X7B4C0N5TDEFA", TenantID: tenant, FeatureKey: "tokens",
		Type: typ, Source: "api", Amount: max(delta, -delta), Delta: delta, Balance: balance, CycleIndex: 2,
		CycleStart: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), ClientKey: client,
		OccurredAt: time.Date(2026, 10, 20, 6, 0, 0, 123_000_000, time.UTC)}
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestTheLedgerAppendsWhatItIsGivenOnce is the Postgres half of DoD 1 and 4:
// a batch lands as one row per event with the event's own id, instant and
// window, in queue order; the same batch again changes nothing; and a second
// deduction under one idempotency key is refused by the partial unique index
// and kept as a duplicate dead letter, never inserted and never lost.
func TestTheLedgerAppendsWhatItIsGivenOnce(t *testing.T) {
	db, tenant := ledgerFixture(t)
	ctx := context.Background()
	l := NewLedger(db)
	a := newEvent(t, tenant, "consumed", "k-1", -30, 970)
	b := newEvent(t, tenant, "refunded", "k-2", 10, 980)
	c := newEvent(t, tenant, "granted", "", 50, 1030)
	c.Source = "admin"
	out, err := l.Append(ctx, []dataplane.Event{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	if out.Written != 3 || out.Present != 0 || len(out.Dead) != 0 {
		t.Fatalf("first append = %+v, want 3 written", out)
	}
	rows, err := db.QueryContext(ctx, `SELECT id::text, delta, created_at, cycle_start, cycle_index,
		coalesce(idempotency_key, '') FROM usage_events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	want := []dataplane.Event{a, b, c}
	i := 0
	for rows.Next() {
		var id, client string
		var delta, index int64
		var at, start time.Time
		if err := rows.Scan(&id, &delta, &at, &start, &index, &client); err != nil {
			t.Fatal(err)
		}
		w := want[i]
		if id != w.ID.String() || delta != w.Delta || !at.Equal(w.OccurredAt) || !start.Equal(w.CycleStart) ||
			index != w.CycleIndex || client != w.ClientKey {
			t.Errorf("row %d = (%s, %d, %s, %s, %d, %q), want the event %+v", i, id, delta, at, start, index, client, w)
		}
		i++
	}
	rows.Close()
	if i != 3 {
		t.Fatalf("%d rows, want 3", i)
	}

	// Redelivery, in a batch and twice inside one batch.
	out, err = l.Append(ctx, []dataplane.Event{a, b, c, a})
	if err != nil {
		t.Fatal(err)
	}
	if out.Written != 0 || out.Present != 4 || len(out.Dead) != 0 {
		t.Errorf("redelivery = %+v, want 4 present and nothing else", out)
	}

	// A second deduction under k-1 with its own event_id: FS-22's shape.
	dup := newEvent(t, tenant, "consumed", "k-1", -30, 950)
	out, err = l.Append(ctx, []dataplane.Event{dup})
	if err != nil {
		t.Fatal(err)
	}
	if out.Written != 0 || len(out.Dead) != 1 || out.Dead[0].Reason != observability.DeadDuplicate {
		t.Fatalf("a second deduction = %+v, want one duplicate dead letter", out)
	}
	var errText string
	if err := db.QueryRowContext(ctx, `SELECT error FROM usage_event_dead_letters WHERE event_id = $1`,
		dup.ID.String()).Scan(&errText); err != nil {
		t.Fatal(err)
	}
	if want := "the first is event " + a.ID.String(); !contains(errText, want) {
		t.Errorf("dead letter error %q does not name the first deduction (%s)", errText, want)
	}
	// Delivered again, the dead letter is not listed or written twice.
	out, err = l.Append(ctx, []dataplane.Event{dup})
	if err != nil {
		t.Fatal(err)
	}
	if out.Written != 0 || len(out.Dead) != 0 {
		t.Errorf("a redelivered dead letter = %+v, want nothing new", out)
	}
	if n := countRows(t, db, `SELECT count(*) FROM usage_events`); n != 3 {
		t.Errorf("%d ledger rows, want 3", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM usage_event_dead_letters`); n != 1 {
		t.Errorf("%d dead letters, want 1", n)
	}
}

// TestARejectedEventIsKeptAndReplayed is the Postgres half of DoD 5: an event
// whose feature resolves to nothing is dead-lettered as rejected, a replay
// while the cause stands raises its attempts and appends nothing, and the
// first replay after the cause is fixed appends it once and removes the dead
// letter. A duplicate is never replayed.
func TestARejectedEventIsKeptAndReplayed(t *testing.T) {
	db, tenant := ledgerFixture(t)
	ctx := context.Background()
	l := NewLedger(db)
	orphan := newEvent(t, tenant, "consumed", "k-orphan", -5, 995)
	orphan.FeatureKey = "images"
	first := newEvent(t, tenant, "consumed", "k-dup", -5, 995)
	dup := newEvent(t, tenant, "consumed", "k-dup", -5, 990)
	if _, err := l.Append(ctx, []dataplane.Event{first}); err != nil {
		t.Fatal(err)
	}
	out, err := l.Append(ctx, []dataplane.Event{orphan, dup})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Dead) != 2 {
		t.Fatalf("append = %+v, want two dead letters", out)
	}
	reasons := map[string]string{}
	for _, d := range out.Dead {
		reasons[d.ID.String()] = d.Reason
	}
	if reasons[orphan.ID.String()] != observability.DeadRejected || reasons[dup.ID.String()] != observability.DeadDuplicate {
		t.Errorf("reasons = %v", reasons)
	}

	n, err := l.ReplayRejected(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("replayed %d while the cause stands", n)
	}
	if got := countRows(t, db, `SELECT attempts FROM usage_event_dead_letters WHERE event_id = $1`, orphan.ID.String()); got != 2 {
		t.Errorf("attempts = %d after one failed replay, want 2", got)
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO features (key, name, unit) VALUES ('images', 'Images', 'image')`); err != nil {
		t.Fatal(err)
	}
	if n, err = l.ReplayRejected(ctx, 10); err != nil || n != 1 {
		t.Fatalf("replay after the fix = %d, %v; want 1", n, err)
	}
	if n, err = l.ReplayRejected(ctx, 10); err != nil || n != 0 {
		t.Fatalf("a second replay = %d, %v; want 0", n, err)
	}
	if got := countRows(t, db, `SELECT count(*) FROM usage_events WHERE id = $1`, orphan.ID.String()); got != 1 {
		t.Errorf("the replayed event is in the ledger %d times", got)
	}
	counts, err := l.DeadLetterCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[observability.DeadRejected] != 0 || counts[observability.DeadDuplicate] != 1 {
		t.Errorf("dead letters after the replay = %v, want the duplicate alone", counts)
	}
}

// TestAPermanentRefusalIsNamedAsOne holds the writer's retry rule to the
// errors Postgres really returns: a malformed tenant id is permanent, so the
// writer bisects and dead-letters it, while a cancelled context is transient.
func TestAPermanentRefusalIsNamedAsOne(t *testing.T) {
	db, tenant := ledgerFixture(t)
	ctx := context.Background()
	l := NewLedger(db)
	bad := newEvent(t, tenant, "consumed", "k", -1, 1)
	bad.TenantID = "not-a-uuid"
	_, err := l.Append(ctx, []dataplane.Event{bad})
	var pg *pgconn.PgError
	if err == nil || !errors.As(err, &pg) || !l.Permanent(err) {
		t.Fatalf("a malformed tenant id = %v, want a permanent database refusal", err)
	}
	created, err := l.DeadLetter(ctx, bad, err)
	if err != nil || !created {
		t.Fatalf("dead-letter = %v, %v", created, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := l.Append(cancelled, []dataplane.Event{newEvent(t, tenant, "consumed", "k2", -1, 1)}); err == nil || l.Permanent(err) {
		t.Errorf("a cancelled append = %v, want a transient error", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
