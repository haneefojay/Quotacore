package dataplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/snapshot"
)

// ErrNotHeld is a reversal for a feature the tenant no longer holds, so there
// is no window to refund into.
var ErrNotHeld = errors.New("dataplane: the tenant does not hold the feature")

// Reverse is the refund reconciliation issues for a detected duplicate
// (DR-049, IP-11 D4): the refund script, for the duplicate's amount, under
// key, in the tenant's current window at its effective limit, whatever the
// tenant's state, because suspension preserves the balance (DR-032). It hands
// nothing to the ledger's sink: the reconciler writes the rows itself, in the
// transaction that resolves the dead letter (S7). A replay under the same key
// answers what the first attempt recorded.
func (s *Service) Reverse(ctx context.Context, externalID, featureKey, key string, amount int64, at time.Time) (
	script.Outcome, error) {
	t, err := s.tenants.Tenant(externalID)
	if errors.Is(err, snapshot.ErrStaleSnapshot) {
		t, err = s.tenants.Tenant(externalID)
	}
	if err != nil {
		return script.Outcome{}, err
	}
	e, held := t.Entitlement(featureKey, at)
	// Only an anchored balance has ledger rows to reverse (DR-050, DR-052).
	if !held || !e.Anchored() {
		return script.Outcome{}, fmt.Errorf("%w: %s", ErrNotHeld, featureKey)
	}
	loc, err := s.location(t.Timezone)
	if err != nil {
		return script.Outcome{}, err
	}
	return s.scripts.Refund(ctx, script.Request{
		TenantID:       t.ID,
		FeatureKey:     featureKey,
		IdempotencyKey: key,
		Amount:         amount,
		Window:         cycle.CurrentWindow(t.Anchor, e.Interval, loc, at),
		Interval:       e.Interval,
		Limit:          e.Limit,
		At:             at,
	})
}
