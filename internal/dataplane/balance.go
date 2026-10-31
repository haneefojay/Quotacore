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
	if problem := bound(ex, t, externalID); problem != nil {
		fail(*problem)
		return
	}
	plan := t.Features(ex.Accepted)
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

	// A boolean entitlement has no balance hash (DR-050): its entry is its
	// presence, so only the metered features are read, and a tenant whose
	// asked-for features are all boolean costs no round trip at all.
	// A rolling entitlement has no balance hash either (DR-052): its entry is
	// its window, read by the rolling check with an amount of zero, which
	// writes nothing.
	var counted []string
	for _, f := range features {
		if plan[f].Anchored() {
			counted = append(counted, f)
		}
	}
	var stored []store.Stored
	if len(counted) > 0 {
		stored, err = s.balances.ReadBalancesAs(r.Context(), ex.KeyTenant, t.ID, counted)
		if errors.Is(err, store.ErrForeignTenant) {
			fail(foreign(externalID))
			return
		}
		if errors.Is(err, store.ErrCorruptBalance) {
			ex.Add(slog.String("error", short(err)))
			fail(envelope.Error(contract.ErrorCodeInternalError, "a stored balance could not be read"))
			return
		}
		if err != nil {
			fail(s.unavailable(ex, err))
			return
		}
	}

	entries := make([]contract.BalanceEntry, 0, len(features))
	read := 0
	for _, f := range features {
		if plan[f].Boolean {
			entries = append(entries, booleanEntry(f))
			continue
		}
		if e := plan[f]; !e.Rolling.IsZero() {
			out, err := s.scripts.CheckRolling(r.Context(), script.RollingRequest{TenantID: t.ID, Binding: ex.KeyTenant, FeatureKey: f,
				Limit: e.Limit, Window: e.Rolling, At: ex.Accepted})
			if err != nil {
				fail(s.scriptError(ex, err))
				return
			}
			entries = append(entries, rollingEntry(f, e.Rolling, out))
			continue
		}
		st := stored[read]
		read++
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
		s.cycles.Ceiling(st.Balance, st.Limit, st.Bonus)
		consumed := st.Limit + st.Bonus - st.Balance
		if consumed < 0 {
			consumed = 0
		}
		start, end := render(st.Start, st.End, loc)
		entry := contract.BalanceEntry{
			FeatureKey: contract.FeatureKey(f),
			Kind:       contract.FeatureKindMetered,
			Balance:    contract.NewNilAmount(contract.Amount(st.Balance)),
			Limit:      contract.NewNilLimit(contract.Limit(st.Limit)),
			Bonus:      contract.NewNilAmount(contract.Amount(st.Bonus)),
			Consumed:   contract.NewNilAmount(contract.Amount(consumed)),
			CycleStart: start,
			CycleEnd:   end,
		}
		// An allocation cap says what it counts and how many there are now,
		// and no other entry carries the field (DR-054).
		if unit := plan[f].Cap; unit != "" {
			entry.Cap = contract.NewOptAllocationCap(contract.AllocationCap{
				Resource: contract.Unit(unit), Count: contract.Amount(consumed),
			})
		}
		entries = append(entries, entry)
	}
	ex.Outcome = observability.OutcomeAllowed
	envelope.JSON(w, r, http.StatusOK, &contract.BalanceResponse{
		TenantExternalID: contract.ExternalId(externalID),
		Entries:          entries,
	})
}

// booleanEntry is a boolean entitlement's balance entry: present, with every
// counter field null, because an inclusion has no balance, no ceiling, no
// bonus and no cycle (DR-050).
func booleanEntry(feature string) contract.BalanceEntry {
	return contract.BalanceEntry{
		FeatureKey: contract.FeatureKey(feature),
		Kind:       contract.FeatureKindBoolean,
		Balance:    contract.NilAmount{Null: true},
		Limit:      contract.NilLimit{Null: true},
		Bonus:      contract.NilAmount{Null: true},
		Consumed:   contract.NilAmount{Null: true},
		CycleStart: contract.NilTimestamp{Null: true},
		CycleEnd:   contract.NilTimestamp{Null: true},
	}
}
