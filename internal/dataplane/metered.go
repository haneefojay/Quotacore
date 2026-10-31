package dataplane

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/observability"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/snapshot"
)

// Consume is POST /v1/consume.
func (s *Service) Consume(w http.ResponseWriter, r *http.Request) { s.metered(w, r, opConsume) }

// Refund is POST /v1/refund.
func (s *Service) Refund(w http.ResponseWriter, r *http.Request) { s.metered(w, r, opRefund) }

// Check is POST /v1/check.
func (s *Service) Check(w http.ResponseWriter, r *http.Request) { s.metered(w, r, opCheck) }

// metered is steps 4 to 9 of request-lifecycle.md section 1 for the three
// routes that share a body. Everything that can fail runs before the script;
// after it, only formatting runs, which is what keeps "a 500 on a mutation
// means nothing was applied" true.
func (s *Service) metered(w http.ResponseWriter, r *http.Request, op kind) {
	ex := envelope.From(r.Context())
	at := ex.Accepted
	fail := func(p envelope.Problem) {
		if envelope.Status(p.Code) >= 500 {
			s.count(op, observability.OutcomeRejected)
			ex.Outcome = observability.OutcomeRejected
		}
		envelope.Write(w, r, p)
	}

	in, problem := decodeMetered(r, op, s.metadataMax)
	if problem != nil {
		fail(*problem)
		return
	}
	ex.Add(slog.Int64("amount", in.Amount))

	t, controlPlane, problem := s.tenant(in.ExternalID)
	if problem != nil {
		fail(*problem)
		return
	}
	if problem := bound(ex, t, in.ExternalID); problem != nil {
		fail(*problem)
		return
	}
	res := s.tenants.Decide(t, in.FeatureKey, at)
	if problem := gate(res, in.FeatureKey); problem != nil {
		fail(*problem)
		return
	}
	if problem := windowMismatch(res, in); problem != nil {
		fail(*problem)
		return
	}
	if res.Boolean {
		s.boolean(w, r, op, in, t.ID, at, controlPlane, fail)
		return
	}
	if !res.Rolling.IsZero() {
		s.rolling(w, r, op, in, t.ID, res, at, controlPlane, fail)
		return
	}
	loc, err := s.location(res.Timezone)
	if err != nil {
		ex.Add(slog.String("error", "tenant zone does not load"))
		fail(envelope.Error(contract.ErrorCodeInternalError, "the tenant's time zone could not be loaded"))
		return
	}
	window := cycle.CurrentWindow(res.Anchor, res.Interval, loc, at)
	ex.Add(slog.String("interval", res.Interval.String()), slog.Int64("limit", res.Limit))

	ctx, cancel := context.WithDeadline(r.Context(), at.Add(s.storeBudget+controlPlane))
	defer cancel()
	req := script.Request{
		TenantID:       t.ID,
		FeatureKey:     in.FeatureKey,
		IdempotencyKey: in.ClientKey,
		Amount:         in.Amount,
		Window:         window,
		Interval:       res.Interval,
		Limit:          res.Limit,
		At:             at,
		Binding:        ex.KeyTenant,
	}
	var out script.Outcome
	switch op {
	case opConsume:
		out, err = s.scripts.Consume(ctx, req)
	case opRefund:
		out, err = s.scripts.Refund(ctx, req)
	default:
		out, err = s.scripts.Check(ctx, req)
	}
	if err != nil {
		fail(s.scriptError(ex, err))
		return
	}
	ex.Add(slog.Int64("balance", out.Balance), slog.Int64("cycle_index", out.Index),
		slog.Bool("replayed", out.Replayed()), slog.Bool("rolled_over", out.Rolled()))

	// A rollover is recorded whatever the request's own decision was: a check,
	// a denial or a refused refund that crossed a boundary still re-granted the
	// allowance, and the ledger would otherwise have no opening for the cycle
	// (IP-12 D3). It goes to the sink before the request's own event.
	rolled := false
	rollover := func() {
		if rolled {
			return
		}
		rolled = true
		if ev, ok := Rollover(ex.RequestID, t.ID, in.FeatureKey, out, at); ok {
			s.events.Enqueue(ev)
		}
	}
	defer rollover()

	start, end := render(out.Start, out.End, loc)
	switch {
	case op == opCheck:
		outcome := observability.OutcomeAllowed
		if !out.Allowed() {
			outcome = observability.OutcomeDenied
		}
		s.count(op, outcome)
		ex.Outcome = outcome
		envelope.JSON(w, r, http.StatusOK, &contract.CheckResponse{
			Allowed:   out.Allowed(),
			Kind:      contract.FeatureKindMetered,
			Balance:   contract.NewNilAmount(contract.Amount(out.Balance)),
			Limit:     contract.NewNilLimit(contract.Limit(out.Limit)),
			Bonus:     contract.NewNilAmount(contract.Amount(out.Bonus)),
			Requested: contract.Amount(in.Amount),
			CycleEnd:  end,
		})

	case out.Decision == script.Denied:
		s.count(op, observability.OutcomeDenied)
		ex.Outcome = observability.OutcomeDenied
		var cycleEnd any
		if !end.Null {
			cycleEnd = time.Time(end.Value).Format(time.RFC3339)
		}
		envelope.Write(w, r, envelope.Problem{
			Code:    contract.ErrorCodeQuotaExceeded,
			Message: "the balance is less than the amount requested",
			Details: map[string]any{
				"feature_key": in.FeatureKey,
				"balance":     out.Balance,
				"requested":   in.Amount,
				"limit":       out.Limit,
				"bonus":       out.Bonus,
				"cycle_end":   cycleEnd,
			},
		})

	case out.Decision == script.RefusedCeiling:
		envelope.Write(w, r, envelope.Problem{
			Code:    contract.ErrorCodeRefundExceedsGrant,
			Message: "the refund would raise the balance above limit plus bonus",
			Details: map[string]any{
				"feature_key": in.FeatureKey,
				"balance":     out.Balance,
				"requested":   in.Amount,
				"limit":       out.Limit,
				"bonus":       out.Bonus,
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
			TenantExternalID: contract.ExternalId(in.ExternalID),
			FeatureKey:       contract.FeatureKey(in.FeatureKey),
			Kind:             contract.FeatureKindMetered,
			Balance:          contract.NewNilAmount(contract.Amount(out.Balance)),
			Limit:            contract.NewNilLimit(contract.Limit(out.Limit)),
			Bonus:            contract.NewNilAmount(contract.Amount(out.Bonus)),
			Consumed:         contract.Amount(in.Amount),
			CycleStart:       start,
			CycleEnd:         end,
			Replayed:         out.Replayed(),
		})
		if out.Replayed() {
			return
		}
		if op == opConsume && s.metrics != nil {
			s.metrics.ConsumeAmount.WithLabelValues(in.FeatureKey).Add(float64(in.Amount))
		}
		if in.Amount > 0 {
			typ, delta := "consumed", -in.Amount
			if op == opRefund {
				typ, delta = "refunded", in.Amount
			}
			rollover()
			s.events.Enqueue(Event{
				RequestID:  ex.RequestID,
				TenantID:   t.ID,
				FeatureKey: in.FeatureKey,
				Type:       typ,
				Source:     "api",
				Amount:     in.Amount,
				Delta:      delta,
				Balance:    out.Balance,
				CycleIndex: out.Index,
				CycleStart: out.Start,
				ClientKey:  in.ClientKey,
				Metadata:   in.Metadata,
				OccurredAt: at,
			})
		}
	}
}

// gate is step 6's refusals, in the order the snapshot decided them:
// archived, then suspended, then not in the plan (DR-015). A feature the
// catalogue does not have is feature_not_in_plan too, so the data plane does
// not disclose the catalogue (FS-17).
func gate(res snapshot.Resolution, featureKey string) *envelope.Problem {
	var p envelope.Problem
	switch {
	case res.Archived:
		p = envelope.Error(contract.ErrorCodeFeatureArchived, "the feature is archived").With("feature_key", featureKey)
	case res.Suspended:
		p = envelope.Error(contract.ErrorCodeTenantSuspended, "the tenant is suspended")
	case !res.InPlan:
		p = envelope.Error(contract.ErrorCodeFeatureNotInPlan, "the tenant's plan does not include the feature").
			With("feature_key", featureKey)
	default:
		return nil
	}
	return &p
}

// scriptError maps a failed script call. A reused key is the caller's; a
// missing balance and a store that did not answer are 503; a request the
// script calls invalid passed validation, so it is a defect and a 500, and the
// script changed nothing.
func (s *Service) scriptError(ex *envelope.Exchange, err error) envelope.Problem {
	switch {
	case errors.Is(err, script.ErrForeignTenant):
		// The script refused a bound key's request for another tenant, which
		// the check before it should already have answered (DR-053).
		ex.Add(slog.String("error", "binding refused in the script"))
		return envelope.Error(contract.ErrorCodeTenantNotFound, "no tenant has that tenant_external_id")
	case errors.Is(err, script.ErrIdempotencyKeyReuse):
		return envelope.Error(contract.ErrorCodeIdempotencyKeyReuse,
			"this Idempotency-Key was used for a different operation within 24 hours")
	case errors.Is(err, script.ErrInvalidArgument), errors.Is(err, script.ErrMissingIdempotencyKey):
		ex.Add(slog.String("error", short(err)))
		return envelope.Error(contract.ErrorCodeInternalError, "the request could not be enforced")
	default:
		return s.unavailable(ex, err)
	}
}

// count increments the operation's outcome counter. check has no rejected
// series (observability.md section 1.1).
func (s *Service) count(op kind, outcome string) {
	if s.metrics == nil {
		return
	}
	switch op {
	case opConsume:
		s.metrics.Consume.WithLabelValues(outcome).Inc()
	case opRefund:
		s.metrics.Refund.WithLabelValues(outcome).Inc()
	default:
		if outcome != observability.OutcomeRejected {
			s.metrics.Check.WithLabelValues(outcome).Inc()
		}
	}
}

// render writes a window in the tenant's zone. A window with no end is a
// `never` entitlement, whose two fields are null together (DR-008, INV-C6).
func render(start time.Time, end *time.Time, loc *time.Location) (contract.NilTimestamp, contract.NilTimestamp) {
	if end == nil {
		return contract.NilTimestamp{Null: true}, contract.NilTimestamp{Null: true}
	}
	return contract.NewNilTimestamp(envelope.Boundary(start, loc)),
		contract.NewNilTimestamp(envelope.Boundary(*end, loc))
}
