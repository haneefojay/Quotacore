package dataplane

import (
	"context"
	"net/http"
	"time"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/observability"
	"github.com/quotacore/quotacore/internal/script"
)

// boolean answers consume, refund and check against a boolean entitlement
// (DR-050), once the gate has found it included. There is no counter, so
// there is no window to compute and no balance to read: an amount of 0 or 1
// is allowed, anything else asks for units the feature does not count and is
// feature_not_metered, and so is every refund. One script, one round trip, as
// for a metered call (IP-17 DoD 4). Nothing is written to the ledger, which
// records non-zero deltas and an inclusion has none (DR-042, Q-33).
func (s *Service) boolean(w http.ResponseWriter, r *http.Request, op kind, in metered, tenantID string,
	at time.Time, controlPlane time.Duration, fail func(envelope.Problem)) {
	ex := envelope.From(r.Context())
	if op == opRefund || in.Amount > 1 {
		fail(NotMetered(in.FeatureKey))
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), at.Add(s.storeBudget+controlPlane))
	defer cancel()
	req := script.Request{TenantID: tenantID, FeatureKey: in.FeatureKey, IdempotencyKey: in.ClientKey,
		Amount: in.Amount, At: at}
	if op == opCheck {
		if _, err := s.scripts.CheckBoolean(ctx, req); err != nil {
			fail(s.scriptError(ex, err))
			return
		}
		s.count(op, observability.OutcomeAllowed)
		ex.Outcome = observability.OutcomeAllowed
		envelope.JSON(w, r, http.StatusOK, &contract.CheckResponse{
			Allowed:   true,
			Kind:      contract.FeatureKindBoolean,
			Balance:   contract.NilAmount{Null: true},
			Limit:     contract.NilLimit{Null: true},
			Bonus:     contract.NilAmount{Null: true},
			Requested: contract.Amount(in.Amount),
			CycleEnd:  contract.NilTimestamp{Null: true},
		})
		return
	}
	d, err := s.scripts.ConsumeBoolean(ctx, req)
	if err != nil {
		fail(s.scriptError(ex, err))
		return
	}
	outcome := observability.OutcomeAllowed
	if d == script.Replayed {
		outcome = observability.OutcomeReplayed
		w.Header().Set("Idempotent-Replay", "true")
	}
	s.count(op, outcome)
	ex.Outcome = outcome
	envelope.JSON(w, r, http.StatusOK, &contract.MeteredResponse{
		TenantExternalID: contract.ExternalId(in.ExternalID),
		FeatureKey:       contract.FeatureKey(in.FeatureKey),
		Kind:             contract.FeatureKindBoolean,
		Balance:          contract.NilAmount{Null: true},
		Limit:            contract.NilLimit{Null: true},
		Bonus:            contract.NilAmount{Null: true},
		Consumed:         contract.Amount(in.Amount),
		CycleStart:       contract.NilTimestamp{Null: true},
		CycleEnd:         contract.NilTimestamp{Null: true},
		Replayed:         d == script.Replayed,
	})
}

// NotMetered is feature_not_metered: a counted operation asked of a feature
// with no counter. It names the mismatch rather than reporting insufficient
// units, which is the distinction a boolean entitlement exists to make
// (IP-17 DoD 2). The admin routes refuse a grant, a set, a rollover and an
// override on a boolean feature with the same problem.
func NotMetered(featureKey string) envelope.Problem {
	return envelope.Error(contract.ErrorCodeFeatureNotMetered,
		"the feature is boolean: it is included or not, and has no counter to "+
			"deduct more than one from, refund, grant or set").
		With("feature_key", featureKey).With("kind", string(contract.FeatureKindBoolean))
}
