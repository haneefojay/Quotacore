package dataplane

import (
	"errors"
	"log/slog"
	"net/http"
	"sort"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/observability"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/store"
)

// Balance is GET /v1/balance. It reports the stored balance hashes verbatim,
// so balance, limit, bonus and consumed are one consistent set even when the
// stored window has ended and the next request will roll it
// (request-lifecycle.md section 2). It applies none of the gate's refusals: a
// suspended tenant and an archived feature are readable (DR-016).
func (s *Service) Balance(w http.ResponseWriter, r *http.Request) {
	ex := envelope.From(r.Context())
	fail := func(p envelope.Problem) {
		if envelope.Status(p.Code) >= 500 {
			ex.Outcome = observability.OutcomeRejected
		}
		envelope.Write(w, r, p)
	}

	q := r.URL.Query()
	var issues []issue
	externalID := q.Get("tenant_external_id")
	switch {
	case !q.Has("tenant_external_id"):
		issues = append(issues, issue{Field: "tenant_external_id", Issue: "is required"})
	case !externalIDShape.MatchString(externalID):
		issues = append(issues, issue{Field: "tenant_external_id", Issue: "does not match " + externalIDShape.String()})
	}
	featureKey := q.Get("feature_key")
	if q.Has("feature_key") && !featureKeyShape.MatchString(featureKey) {
		issues = append(issues, issue{Field: "feature_key", Issue: "does not match " + featureKeyShape.String()})
	}
	if len(issues) > 0 {
		fail(*reject(issues))
		return
	}

	t, _, problem := s.tenant(externalID)
	if problem != nil {
		fail(*problem)
		return
	}
	plan := t.Features()
	var features []string
	if q.Has("feature_key") {
		if _, ok := plan[featureKey]; !ok {
			fail(envelope.Error(contract.ErrorCodeFeatureNotFound, "the tenant's plan has no such feature").
				With("feature_key", featureKey))
			return
		}
		features = []string{featureKey}
	} else {
		for f := range plan {
			features = append(features, f)
		}
		sort.Strings(features)
	}
	loc, err := s.location(t.Timezone)
	if err != nil {
		ex.Add(slog.String("error", "tenant zone does not load"))
		fail(envelope.Error(contract.ErrorCodeInternalError, "the tenant's time zone could not be loaded"))
		return
	}

	stored, err := s.balances.ReadBalances(r.Context(), t.ID, features)
	if errors.Is(err, store.ErrCorruptBalance) {
		ex.Add(slog.String("error", short(err)))
		fail(envelope.Error(contract.ErrorCodeInternalError, "a stored balance could not be read"))
		return
	}
	if err != nil {
		fail(s.unavailable(ex, err))
		return
	}

	entries := make([]contract.BalanceEntry, 0, len(features))
	for i, f := range features {
		st := stored[i]
		if !st.Present {
			// An archived feature's hash expires a day after its last cycle,
			// and that is not a fault. Any other absence is DR-045's: the whole
			// answer is a 503, because a partial list would misreport the
			// tenant.
			if plan[f].Archived {
				continue
			}
			fail(s.unavailable(ex, script.ErrStateMissing))
			return
		}
		consumed := st.Limit + st.Bonus - st.Balance
		if consumed < 0 {
			consumed = 0
		}
		start, end := render(st.Start, st.End, loc)
		entries = append(entries, contract.BalanceEntry{
			FeatureKey: contract.FeatureKey(f),
			Balance:    contract.Amount(st.Balance),
			Limit:      contract.Limit(st.Limit),
			Bonus:      contract.Amount(st.Bonus),
			Consumed:   contract.Amount(consumed),
			CycleStart: start,
			CycleEnd:   end,
		})
	}
	ex.Outcome = observability.OutcomeAllowed
	envelope.JSON(w, r, http.StatusOK, &contract.BalanceResponse{
		TenantExternalID: contract.ExternalId(externalID),
		Entries:          entries,
	})
}
