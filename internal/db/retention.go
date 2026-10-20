package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/quotacore/quotacore/internal/observability"
)

// This file is the one place DR-044 and NFR-C6 permit a deletion from the
// append-only tables: the bounded background retention job. It is the single
// entry in source_test.go's retentionAllowed, and no route reaches it
// (TestNoRouteReachesRetention); cmd/quotacore starts it and nothing else calls
// it.

// retentionLockKey is the session advisory lock one pruning instance holds, so
// every instance can run the job and only one prunes at a time.
const retentionLockKey = 6067046871116144152

// RetentionOptions configure the job (deployment.md section 4.4). A retention
// of zero keeps that table's rows indefinitely.
type RetentionOptions struct {
	EventDays int
	AuditDays int
	Interval  time.Duration
	// FirstRun is the delay before the first tick. Zero means a minute.
	FirstRun time.Duration
	// BatchSize, StatementTimeout and TickBudget bound one tick. Zero means
	// 5 000 rows, 5 s and a minute.
	BatchSize        int
	StatementTimeout time.Duration
	TickBudget       time.Duration
	// ReplayLimit bounds the dead letters re-attempted per tick. Zero means
	// 100.
	ReplayLimit int
	Logger      *slog.Logger
	Metrics     *observability.Ledger
}

func (o *RetentionOptions) defaults() {
	if o.FirstRun <= 0 {
		o.FirstRun = time.Minute
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 5000
	}
	if o.StatementTimeout <= 0 {
		o.StatementTimeout = 5 * time.Second
	}
	if o.TickBudget <= 0 {
		o.TickBudget = time.Minute
	}
	if o.ReplayLimit <= 0 {
		o.ReplayLimit = 100
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Pruned is what one tick did.
type Pruned struct {
	Ran      bool // false when another instance held the lock
	Events   int64
	Audit    int64
	Replayed int
}

// RunRetention runs the job until ctx ends: a first tick after FirstRun, then
// one every Interval. A tick that fails is logged and the next one tries
// again; nothing here touches the request path.
func RunRetention(ctx context.Context, db *sql.DB, opt RetentionOptions) {
	opt.defaults()
	timer := time.NewTimer(opt.FirstRun)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		p, err := PruneOnce(ctx, db, time.Now(), opt)
		if err != nil && ctx.Err() == nil {
			opt.Logger.Error("the retention job failed; the next tick tries again", "error", err)
		} else if p.Ran {
			opt.Logger.Info("retention applied", "events_deleted", p.Events, "audit_deleted", p.Audit,
				"dead_letters_replayed", p.Replayed)
		}
		timer.Reset(opt.Interval)
	}
}

// PruneOnce is one tick: under the lock, delete the expired rows of each table
// in bounded batches until a batch comes back short or the tick's budget is
// spent, then re-attempt rejected dead letters and refresh the gauge.
func PruneOnce(ctx context.Context, db *sql.DB, now time.Time, opt RetentionOptions) (Pruned, error) {
	opt.defaults()
	var p Pruned
	// The lock is a session lock, so the connection that takes it must be the
	// one that releases it; a pooled unlock on another connection fails
	// silently (IP-08).
	conn, err := db.Conn(ctx)
	if err != nil {
		return p, err
	}
	defer conn.Close()
	var held bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, retentionLockKey).Scan(&held); err != nil {
		return p, fmt.Errorf("take the retention lock: %w", err)
	}
	if !held {
		return p, nil
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, retentionLockKey)
	p.Ran = true
	deadline := time.Now().Add(opt.TickBudget)
	if opt.EventDays > 0 {
		cutoff := now.Add(-time.Duration(opt.EventDays) * 24 * time.Hour)
		if p.Events, err = pruneTable(ctx, conn, deleteEvents, cutoff, opt, deadline); err != nil {
			return p, fmt.Errorf("prune usage_events: %w", err)
		}
	}
	if opt.AuditDays > 0 {
		cutoff := now.Add(-time.Duration(opt.AuditDays) * 24 * time.Hour)
		if p.Audit, err = pruneTable(ctx, conn, deleteAudit, cutoff, opt, deadline); err != nil {
			return p, fmt.Errorf("prune audit_log: %w", err)
		}
	}
	l := NewLedger(db)
	if p.Replayed, err = l.ReplayRejected(ctx, opt.ReplayLimit); err != nil {
		return p, fmt.Errorf("replay dead letters: %w", err)
	}
	if opt.Metrics != nil {
		counts, err := l.DeadLetterCounts(ctx)
		if err != nil {
			return p, err
		}
		for reason, n := range counts {
			opt.Metrics.DeadLetters.WithLabelValues(reason).Set(float64(n))
		}
		opt.Metrics.Written.Add(float64(p.Replayed))
	}
	return p, nil
}

// The two deletions, each one bounded batch by created_at, the column the
// BRIN index (usage_events) and the time index (audit_log) cover.
const (
	deleteEvents = `DELETE FROM usage_events
		WHERE id IN (SELECT id FROM usage_events WHERE created_at < $1 LIMIT $2)`
	deleteAudit = `DELETE FROM audit_log
		WHERE id IN (SELECT id FROM audit_log WHERE created_at < $1 LIMIT $2)`
)

func pruneTable(ctx context.Context, conn *sql.Conn, stmt string, cutoff time.Time, opt RetentionOptions,
	deadline time.Time) (int64, error) {
	var total int64
	for time.Now().Before(deadline) {
		n, err := pruneBatch(ctx, conn, stmt, cutoff, opt)
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(opt.BatchSize) {
			break
		}
	}
	return total, nil
}

// pruneBatch deletes one batch in its own short transaction, under a statement
// timeout, so a tick never holds a lock on the table for longer than one batch
// takes (data-model.md section 5).
func pruneBatch(ctx context.Context, conn *sql.Conn, stmt string, cutoff time.Time, opt RetentionOptions) (int64, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, opt.StatementTimeout.Milliseconds())); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, stmt, cutoff.UTC(), opt.BatchSize)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
