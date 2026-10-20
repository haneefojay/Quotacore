package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Opening is one balance hash a new tenant starts with: the allowance of one
// entitlement in the window the tenant's anchor opens (DR-002).
type Opening struct {
	Feature string
	Limit   int64
	Index   int64
	Start   time.Time
	End     *time.Time // nil for an entitlement that never resets
}

// Provision writes a new tenant's balance hashes, each one whole: the six
// fields and the expiry in one MULTI/EXEC, so a hash is written with all six
// fields or not at all (data-model.md section 3.2). A failure part way leaves
// the hashes already written, which the caller's rollback makes unreachable:
// they name a tenant id that was never committed, and they expire at
// window_end + 24h like any other.
func (s *Store) Provision(ctx context.Context, tenantID string, openings []Opening) error {
	for _, o := range openings {
		key, err := BalanceKey(tenantID, o.Feature)
		if err != nil {
			return err
		}
		end := ""
		if o.End != nil {
			end = strconv.FormatInt(o.End.UnixMilli(), 10)
		}
		pipe := s.client.TxPipeline()
		pipe.HSet(ctx, key, "balance", o.Limit, "limit", o.Limit, "bonus", 0, "cycle_index", o.Index,
			"window_start", o.Start.UnixMilli(), "window_end", end)
		if exp, ok := KeyExpiry(o.End); ok {
			pipe.PExpireAt(ctx, key, exp)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("provision %s: %w", o.Feature, err)
		}
	}
	return nil
}

// Publish is the propagation step of a committed control-plane write
// (request-lifecycle.md section 4): one INCR of qc:cfg:version, the new
// version recorded for every tenant whose configuration changed, and the
// invalidation message. A write that changed no tenant (an API key) still
// bumps and publishes, because every instance's key cache reconciles on it.
func (s *Store) Publish(ctx context.Context, tenantIDs []string) (int64, error) {
	version, err := s.client.Incr(ctx, ConfigVersionKey()).Result()
	if err != nil {
		return 0, fmt.Errorf("bump config version: %w", err)
	}
	if len(tenantIDs) > 0 {
		fields := make([]any, 0, 2*len(tenantIDs))
		for _, id := range tenantIDs {
			fields = append(fields, id, version)
		}
		if err := s.client.HSet(ctx, ConfigTenantsKey(), fields...).Err(); err != nil {
			return version, fmt.Errorf("record tenant versions: %w", err)
		}
	}
	return version, s.PublishInvalidation(ctx, version)
}

// cursorKeyBytes is the length of the key that signs admin list cursors.
const cursorKeyBytes = 32

// CursorKey returns the key every instance signs admin list cursors with,
// creating it on first use with SET NX so two instances starting together
// agree on one (data-model.md section 3.1). Losing the store loses the key,
// which invalidates outstanding cursors: a 400 and a re-read, never a wrong
// page.
func (s *Store) CursorKey(ctx context.Context) ([]byte, error) {
	fresh := make([]byte, cursorKeyBytes)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	if err := s.client.SetNX(ctx, CursorKeyName(), fresh, 0).Err(); err != nil {
		return nil, fmt.Errorf("create cursor key: %w", err)
	}
	got, err := s.client.Get(ctx, CursorKeyName()).Bytes()
	if errors.Is(err, redis.Nil) || (err == nil && len(got) != cursorKeyBytes) {
		return nil, errors.New("store: the cursor key is missing or malformed")
	}
	if err != nil {
		return nil, fmt.Errorf("read cursor key: %w", err)
	}
	return got, nil
}
