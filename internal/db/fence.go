package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// fenceKey is the advisory lock that makes "the fleet is read-only for the
// whole rebuild" a property a test can break rather than a runbook step
// (IP-11 D6). Every instance in normal mode holds it shared on a connection of
// its own for its life; a rebuild that writes takes it exclusive, so it cannot
// start while a normal instance runs, and a normal instance cannot start while
// it runs.
const fenceKey = 6067046871116144154

// ErrFenced is a normal start refused because a rebuild holds the fence.
var ErrFenced = errors.New("a rebuild holds the fence; start in QUOTACORE_STARTUP_MODE=readonly, or wait for it to finish")

// Fence is a normal instance's shared hold on the fence.
type Fence struct {
	db  *sql.DB
	log *slog.Logger

	mu   sync.Mutex
	conn *sql.Conn
	pid  int
}

// NewFence builds a fence over its own pool of one connection.
func NewFence(db *sql.DB, log *slog.Logger) *Fence {
	if log == nil {
		log = slog.Default()
	}
	return &Fence{db: db, log: log}
}

// Acquire takes the fence shared. It reports false, holding nothing, when a
// rebuild holds it exclusive.
func (f *Fence) Acquire(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conn != nil {
		return true, nil
	}
	conn, err := f.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	var held bool
	var pid int
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock_shared($1), pg_backend_pid()`, fenceKey).
		Scan(&held, &pid); err != nil {
		conn.Close()
		return false, fmt.Errorf("take the fence: %w", err)
	}
	if !held {
		conn.Close()
		return false, nil
	}
	f.conn, f.pid = conn, pid
	return true, nil
}

// Held reports whether the fence connection still answers, and drops it when
// it does not: a lock held by a connection that has gone is a lock no longer
// held.
func (f *Fence) Held(ctx context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conn == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var held bool
	err := f.conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory'
		AND pid = pg_backend_pid() AND mode = 'ShareLock' AND granted
		AND ((classid::bigint << 32) | objid::bigint) = $1)`, int64(fenceKey)).Scan(&held)
	if err != nil || !held {
		f.conn.Close()
		f.conn, f.pid = nil, 0
		return false
	}
	return true
}

// PID is the backend holding the fence, or zero; a test terminates it to
// prove that an instance that loses its fence stops writing.
func (f *Fence) PID() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pid
}

// Keep checks the hold every interval until ctx ends, re-acquiring it when it
// was lost, and reports every change to onChange: false the moment the hold
// is found gone, true when it is back. The instance serves read-only in
// between (IP-11 D6).
func (f *Fence) Keep(ctx context.Context, every time.Duration, onChange func(held bool)) {
	held := true
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := f.Held(ctx)
		if !now {
			if ok, err := f.Acquire(ctx); err == nil && ok {
				now = true
			}
		}
		if now != held {
			held = now
			if held {
				f.log.Warn("re-acquired the rebuild fence; writes are served again")
			} else {
				f.log.Error("lost the rebuild fence; serving read-only until it is re-acquired")
			}
			onChange(held)
		}
	}
}

// Release lets the fence go.
func (f *Fence) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conn != nil {
		// Unlocked explicitly: closing a pooled connection returns it to the
		// pool, still holding the lock.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = f.conn.ExecContext(ctx, `SELECT pg_advisory_unlock_shared($1)`, fenceKey)
		cancel()
		f.conn.Close()
		f.conn, f.pid = nil, 0
	}
}

// ExclusiveFence takes the fence exclusive for a rebuild that writes. It
// reports false, holding nothing, while any normal instance holds it shared;
// release lets it go.
func ExclusiveFence(ctx context.Context, db *sql.DB) (release func(), ok bool, err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var held bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, fenceKey).Scan(&held); err != nil {
		conn.Close()
		return nil, false, fmt.Errorf("take the fence: %w", err)
	}
	if !held {
		conn.Close()
		return nil, false, nil
	}
	return func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, fenceKey)
		conn.Close()
	}, true, nil
}

// FenceHolders counts the backends holding the fence shared: the normal
// instances a rebuild is waiting on.
func FenceHolders(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND mode = 'ShareLock'
		AND granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		AND ((classid::bigint << 32) | objid::bigint) = $1`, int64(fenceKey)).Scan(&n)
	return n, err
}
