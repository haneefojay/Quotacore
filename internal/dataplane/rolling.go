package dataplane

import (
	"context"
	"net/http"
	"time"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/observability"
	"github.com/quotacore/quotacore/internal/rolling"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/snapshot"
)

// windowMismatch is invalid_window for a request whose window does not match
// the feature's cadence (IP-19 DoD 7): a rolling feature must name the window
// its entitlement configures, and any other feature must name none. The two
// cadences are never silently mixed, because a silent mix makes the balance a
// lie (DR-052, UC-17).
func windowMismatch(res snapshot.Resolution, in metered) *envelope.Problem {
	var p envelope.Problem
	switch {
	case res.Rolling.IsZero() && in.Window == "":
		return nil
	case res.Rolling.IsZero():
		p = envelope.Error(contract.ErrorCodeInvalidWindow,
			"window: the feature is counted on an anchored cycle; omit window").
			With("field", "window").With("requested", in.Window).With("supported", "anchored")
	case in.Window == res.Rolling.Name():
		return nil
	case in.Window == "":
		p = envelope.Error(contract.ErrorCodeInvalidWindow,
			"window: the feature is counted over "+res.Rolling.Name()+"; name it").
			With("field", "window").With("requested", "anchored").With("supported", res.Rolling.Name())
	default:
		p = envelope.Error(contract.ErrorCodeInvalidWindow,
			"window: the feature is counted over "+res.Rolling.Name()+", not "+in.Window).
			With("field", "window").With("requested", in.Window).With("supported", res.Rolling.Name())
	}
	return &p
}

// NotAnchored is feature_not_anchored: a grant, a set, a rollover or an
// override asked of a rolling feature, which counts in a window and has no
// cycle balance to change (DR-052). Per-window overrides are deferred.
func NotAnchored(featureKey string) envelope.Problem {
	return envelope.Error(contract.ErrorCodeFeatureNotAnchored,
		"the feature is rolling: it is counted over a sliding window and has no cycle balance "+
			"to grant to, set, roll over or override").
		With("feature_key", featureKey).With("kind", string(contract.FeatureKindRolling))
}

// rolling answers consume, refund and check against a rolling-window
// entitlement (DR-052), once the gate has found it included and the window
// matches. One script, one round trip, as for a metered call (IP-19 DoD 6).
// Nothing is written to the ledger, whose identity is a cycle's opening plus
// its deltas, and a rolling window has neither (Q-37).
func (s *Service) rolling(w http.ResponseWriter, r *http.Request, op kind, in metered, tenantID string,
	res snapshot.Resolution, at time.Time, controlPlane time.Duration, fail func(envelope.Problem)) {
	ex := envelope.From(r.Context())
	ctx, cancel := context.WithDeadline(r.Context(), at.Add(s.storeBudget+controlPlane))
	defer cancel()
	req := script.RollingRequest{TenantID: tenantID, Binding: ex.KeyTenant, FeatureKey: in.FeatureKey, IdempotencyKey: in.ClientKey,
		Amount: in.Amount, Limit: res.Limit, Window: res.Rolling, At: at}
	var (
		out script.RollingOutcome
		err error
	)
	switch op {
	case opConsume:
		out, err = s.scripts.ConsumeRolling(ctx, req)
	case opRefund:
		out, err = s.scripts.RefundRolling(ctx, req)
	default:
		out, err = s.scripts.CheckRolling(ctx, req)
	}
	if err != nil {
		fail(s.scriptError(ex, err))
		return
	}
	window := contract.NewOptRollingWindow(contract.RollingWindow(res.Rolling.Name()))
	precision := contract.NewOptInt64(int64(res.Rolling.Precision() / time.Second))

	switch {
	case op == opCheck:
		outcome := observability.OutcomeAllowed
		if !out.Allowed() {
			outcome = observability.OutcomeDenied
		}
		s.count(op, outcome)
		ex.Outcome = outcome
		envelope.JSON(w, r, http.StatusOK, &contract.CheckResponse{
			Allowed:                out.Allowed(),
			Kind:                   contract.FeatureKindRolling,
			Balance:                contract.NewNilAmount(contract.Amount(out.Remaining)),
			Limit:                  contract.NewNilLimit(contract.Limit(out.Limit)),
			Bonus:                  contract.NilAmount{Null: true},
			Requested:              contract.Amount(in.Amount),
			CycleEnd:               contract.NilTimestamp{Null: true},
			Window:                 window,
			WindowPrecisionSeconds: precision,
		})

	case out.Decision == script.Denied:
		s.count(op, observability.OutcomeDenied)
		ex.Outcome = observability.OutcomeDenied
		var next any
		if out.NextRelease != nil {
			next = out.NextRelease.UTC().Format(time.RFC3339)
		}
		envelope.Write(w, r, envelope.Problem{
			Code:    contract.ErrorCodeQuotaExceeded,
			Message: "the window sum plus the amount would exceed the limit",
			Details: map[string]any{
				"feature_key":     in.FeatureKey,
				"balance":         out.Remaining,
				"requested":       in.Amount,
				"limit":           out.Limit,
				"window":          res.Rolling.Name(),
				"next_release_at": next,
			},
		})

	case out.Decision == script.RefusedCeiling:
		envelope.Write(w, r, envelope.Problem{
			Code:    contract.ErrorCodeRefundExceedsGrant,
			Message: "the refund is more than the window holds, and would leave more than the limit remaining",
			Details: map[string]any{
				"feature_key": in.FeatureKey,
				"balance":     out.Remaining,
				"requested":   in.Amount,
				"limit":       out.Limit,
				"window":      res.Rolling.Name(),
			},
		})

	default: // applied or replayed
		outcome := observability.OutcomeAllowed
		if out.Replayed() {
			outcome = observability.OutcomeReplayed
			w.Header().Set("Idempotent-Replay", "true")
		}
		s.count(op, outcome)
		ex.Outcome = outcome
		envelope.JSON(w, r, http.StatusOK, &contract.MeteredResponse{
			TenantExternalID:       contract.ExternalId(in.ExternalID),
			FeatureKey:             contract.FeatureKey(in.FeatureKey),
			Kind:                   contract.FeatureKindRolling,
			Balance:                contract.NewNilAmount(contract.Amount(out.Remaining)),
			Limit:                  contract.NewNilLimit(contract.Limit(out.Limit)),
			Bonus:                  contract.NilAmount{Null: true},
			Consumed:               contract.Amount(in.Amount),
			CycleStart:             contract.NilTimestamp{Null: true},
			CycleEnd:               contract.NilTimestamp{Null: true},
			Window:                 window,
			WindowPrecisionSeconds: precision,
			Replayed:               out.Replayed(),
		})
		if !out.Replayed() && op == opConsume && s.metrics != nil {
			s.metrics.ConsumeAmount.WithLabelValues(in.FeatureKey).Add(float64(in.Amount))
		}
	}
}

// rollingEntry is a rolling entitlement's balance entry: the window sum as
// consumed, the limit less it as the balance, no bonus and no cycle (DR-052).
func rollingEntry(feature string, w rolling.Window, out script.RollingOutcome) contract.BalanceEntry {
	return contract.BalanceEntry{
		FeatureKey:             contract.FeatureKey(feature),
		Kind:                   contract.FeatureKindRolling,
		Balance:                contract.NewNilAmount(contract.Amount(out.Remaining)),
		Limit:                  contract.NewNilLimit(contract.Limit(out.Limit)),
		Bonus:                  contract.NilAmount{Null: true},
		Consumed:               contract.NewNilAmount(contract.Amount(out.Sum)),
		CycleStart:             contract.NilTimestamp{Null: true},
		CycleEnd:               contract.NilTimestamp{Null: true},
		Window:                 contract.NewOptRollingWindow(contract.RollingWindow(w.Name())),
		WindowPrecisionSeconds: contract.NewOptInt64(int64(w.Precision() / time.Second)),
	}
}
