package admin

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	contract "github.com/quotacore/quotacore/api"
)

var intervals = map[string]bool{"hourly": true, "daily": true, "weekly": true, "monthly": true, "yearly": true, "never": true}

func (s *Service) listPlans(w http.ResponseWriter, r *http.Request) {
	l, ok := s.startList(w, r, "plans", nil)
	if !ok {
		return
	}
	rows, err := s.store.ListPlans(r.Context(), l.page)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	n, cur, err := s.next(r, l, len(rows), func(i int) Position { return pos(rows[i].CreatedAt, rows[i].ID) })
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	respond(w, r, http.StatusOK, &contract.PlanList{Plans: rows[:n], NextCursor: cur})
}

// planID reads the path's plan identifier: a UUID or a plan key (S1). One
// that is neither names no plan.
func planID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if !uuidShape.MatchString(id) && !planKeyShape.MatchString(id) {
		problem(w, r, notFound(contract.ErrorCodePlanNotFound))
		return "", false
	}
	return id, true
}

func (s *Service) getPlan(w http.ResponseWriter, r *http.Request) {
	id, valid := planID(w, r)
	if !valid {
		return
	}
	p, err := s.store.GetPlan(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, r, http.StatusOK, &p)
}

// createPlan creates a plan and its 1 to 100 entitlements in one transaction.
// A feature named twice is validation_failed naming it (INV-E2), and an
// unknown or archived feature is feature_not_found.
func (s *Service) createPlan(w http.ResponseWriter, r *http.Request) {
	b, err := readBody(r, "plan_key", "name", "description", "entitlements")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	in := NewPlan{}
	in.Key, _ = b.str("plan_key", true, planKeyShape, 1, 63)
	in.Name, _ = b.str("name", true, nil, 1, nameMax)
	in.Description, _ = b.str("description", false, nil, 0, textMax)
	in.Entitlements = b.entitlements()
	if p := b.check(&contract.PlanCreate{}); p != nil {
		problem(w, r, p)
		return
	}
	p, err := s.store.CreatePlan(r.Context(), actor(r), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.publish(r, nil)
	respond(w, r, http.StatusCreated, &p)
}

func (b *body) entitlements() []Entitlement {
	raw, present := b.fields["entitlements"]
	if !present {
		b.add("entitlements", "is required")
		return nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		b.add("entitlements", "must be an array")
		return nil
	}
	if len(rows) < 1 || len(rows) > entitlementsN {
		b.add("entitlements", fmt.Sprintf("must hold 1 to %d entitlements", entitlementsN))
		return nil
	}
	out := make([]Entitlement, 0, len(rows))
	seen := map[string]bool{}
	for i, row := range rows {
		prefix := fmt.Sprintf("entitlements[%d].", i)
		var fields map[string]json.RawMessage
		dec := json.NewDecoder(bytesReader(row))
		dec.UseNumber()
		if err := dec.Decode(&fields); err != nil || fields == nil {
			b.add(fmt.Sprintf("entitlements[%d]", i), "must be an object")
			continue
		}
		sub := &body{fields: fields}
		for name := range fields {
			switch name {
			case "feature_key", "limit", "reset_interval":
			default:
				sub.add(name, "is not a field of an entitlement")
			}
		}
		e := Entitlement{}
		e.Feature, _ = sub.str("feature_key", true, featureKeyShape, 1, 64)
		e.Limit, _ = sub.integer("limit", true, 0, limitCeiling)
		e.Interval, _ = sub.str("reset_interval", true, nil, 1, 16)
		if e.Interval != "" && !intervals[e.Interval] {
			sub.add("reset_interval", "must be hourly, daily, weekly, monthly, yearly or never")
		}
		if e.Feature != "" {
			if seen[e.Feature] {
				sub.add("feature_key", "names a feature already in this plan")
			}
			seen[e.Feature] = true
		}
		for _, is := range sub.issues {
			b.add(prefix+is.Field, is.Issue)
		}
		out = append(out, e)
	}
	return out
}

// archivePlan stops new assignments. No assigned tenant changes (INV-P1), so
// no tenant is re-versioned.
func (s *Service) archivePlan(w http.ResponseWriter, r *http.Request) {
	id, valid := planID(w, r)
	if !valid {
		return
	}
	b, err := readBody(r, "version", "reason")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	version, reason := b.version(), b.reason()
	if p := b.check(&contract.ArchiveRequest{}); p != nil {
		problem(w, r, p)
		return
	}
	p, err := s.store.ArchivePlan(r.Context(), actor(r), id, version, reason)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.publish(r, nil)
	respond(w, r, http.StatusOK, &p)
}

// deletePlan hard-deletes a plan no tenant, including a soft-deleted one, is
// assigned to (P-6, DR-035), and is 409 plan_in_use otherwise.
func (s *Service) deletePlan(w http.ResponseWriter, r *http.Request) {
	id, valid := planID(w, r)
	if !valid {
		return
	}
	b, err := readBody(r, "reason")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	reason := b.reason()
	if p := b.check(&contract.DeleteRequest{}); p != nil {
		problem(w, r, p)
		return
	}
	if err := s.store.DeletePlan(r.Context(), actor(r), id, reason); err != nil {
		s.fail(w, r, err)
		return
	}
	s.publish(r, nil)
	noContent(w, r)
}
