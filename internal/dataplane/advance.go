package dataplane

import (
	"context"
	"time"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/snapshot"
)

// Advance is the reset worker's one operation (Q-13, state-machines.md section
// 11): the transition a request applies, for one tenant-feature, at an
// instant, through the same path. It resolves the feature as a check would and
// runs the check script for an amount of zero, which writes only what a
// transition writes and only when a boundary has closed (check.lua). A rolled
// cycle is handed to the sink exactly as a request's rollover is, attributed
// to the system and to no request. A feature the tenant does not hold, an
// archived one and a suspended tenant are left alone, as a request would be
// refused before the script. It reports whether the cycle rolled.
//
// There is no second implementation: a stale verdict is the request path
// having won the boundary, and is success (C-3).
func (s *Service) Advance(ctx context.Context, t *snapshot.Tenant, featureKey string, at time.Time) (bool, error) {
	res := s.tenants.Decide(t, featureKey, at)
	if !res.InPlan || res.Archived || res.Suspended {
		return false, nil
	}
	loc, err := s.location(res.Timezone)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.storeBudget)
	defer cancel()
	out, err := s.scripts.Check(ctx, script.Request{
		TenantID:   t.ID,
		FeatureKey: featureKey,
		Window:     cycle.CurrentWindow(res.Anchor, res.Interval, loc, at),
		Interval:   res.Interval,
		Limit:      res.Limit,
		At:         at,
	})
	if err != nil {
		return false, err
	}
	if ev, ok := Rollover("", t.ID, featureKey, out, at); ok {
		s.events.Enqueue(ev)
	}
	return out.Rolled(), nil
}
