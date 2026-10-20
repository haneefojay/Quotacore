package store

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// Confirmation is a pending apply-now confirmation (IP-10, D7): the plan it
// was issued for, the plan version it was counted against, the number of
// tenants the operator was shown, and a hash of the reason, so a token is
// honoured only for the identical request.
type Confirmation struct {
	PlanID     string
	Version    int64
	Tenants    int64
	ReasonHash string
}

// PutConfirmation stores a confirmation under its token, expiring after ttl
// (QUOTACORE_CONFIRMATION_TTL), so an abandoned token does not accumulate in
// a store that never evicts (DR-048).
func (s *Store) PutConfirmation(ctx context.Context, token string, c Confirmation, ttl time.Duration) error {
	key, err := ConfirmationKey(token)
	if err != nil {
		return err
	}
	pipe := s.client.TxPipeline()
	pipe.HSet(ctx, key, "plan_id", c.PlanID, "resource_version", c.Version,
		"affected_tenants", c.Tenants, "reason_hash", c.ReasonHash)
	pipe.PExpire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("store a confirmation: %w", err)
	}
	return nil
}

// TakeConfirmation reads and deletes a confirmation in one MULTI/EXEC, so a
// token is honoured at most once whatever happens next (D7). found is false
// for a token that was never issued, has expired or was already taken.
func (s *Store) TakeConfirmation(ctx context.Context, token string) (c Confirmation, found bool, err error) {
	key, err := ConfirmationKey(token)
	if err != nil {
		return Confirmation{}, false, nil
	}
	pipe := s.client.TxPipeline()
	get := pipe.HGetAll(ctx, key)
	pipe.Del(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil {
		return Confirmation{}, false, fmt.Errorf("take a confirmation: %w", err)
	}
	m := get.Val()
	if len(m) == 0 {
		return Confirmation{}, false, nil
	}
	version, errV := strconv.ParseInt(m["resource_version"], 10, 64)
	tenants, errT := strconv.ParseInt(m["affected_tenants"], 10, 64)
	if errV != nil || errT != nil || m["plan_id"] == "" || m["reason_hash"] == "" {
		return Confirmation{}, false, nil
	}
	return Confirmation{PlanID: m["plan_id"], Version: version, Tenants: tenants, ReasonHash: m["reason_hash"]}, true, nil
}
