package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/store"
)

func (s *Service) listTenants(w http.ResponseWriter, r *http.Request) {
	var planKey string
	l, ok := s.startList(w, r, "tenants", func(q *query) []string {
		planKey = q.str("plan_key", planKeyShape)
		return []string{planKey}
	})
	if !ok {
		return
	}
	rows, err := s.store.ListTenants(r.Context(), l.page, planKey)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	n, cur, err := s.next(r, l, len(rows), func(i int) Position { return pos(rows[i].CreatedAt, rows[i].ID) })
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	respond(w, r, http.StatusOK, &contract.TenantList{Tenants: rows[:n], NextCursor: cur})
}

// tenantID reads the path's tenant identifier: a UUID or an external_id
// (S1). One that is neither names no tenant, and is never echoed.
func tenantID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if !uuidShape.MatchString(id) && !externalIDShape.MatchString(id) {
		problem(w, r, notFound(contract.ErrorCodeTenantNotFound))
		return "", false
	}
	return id, true
}

func (s *Service) getTenant(w http.ResponseWriter, r *http.Request) {
	id, valid := tenantID(w, r)
	if !valid {
		return
	}
	t, err := s.store.GetTenant(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, r, http.StatusOK, &t)
}

// zoneIssue checks an IANA name against the embedded zone database. A name the
// database does not know is invalid_timezone, not validation_failed, so a
// client can tell a typo in a zone from a malformed body.
func zoneIssue(name string) bool {
	if name == "UTC" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err != nil
}

func invalidZone(w http.ResponseWriter, r *http.Request) {
	envelope.Write(w, r, envelope.Error(contract.ErrorCodeInvalidTimezone,
		"timezone is not an IANA zone name; abbreviations are refused").With("field", "timezone"))
}

// createTenant provisions a tenant (S3): the row and its audit row are
// inserted, the opening balance of every non-archived entitlement is written
// whole to the fast store, and only then is the transaction committed. A
// failed hash write rolls the tenant back and is 503 service_unavailable, so a
// committed tenant always has its balances (DR-045).
func (s *Service) createTenant(w http.ResponseWriter, r *http.Request) {
	b, err := readBody(r, "external_id", "display_name", "plan_key", "timezone", "metadata")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// external_id is checked before anything else touches it, and the issue
	// names the field without the value (DR-034, NFR-S2).
	in := NewTenant{Timezone: "UTC", Anchor: accepted(r).UTC()}
	if raw, present := b.fields["external_id"]; !present {
		b.add("external_id", "is required")
	} else if v, valid := externalIDValue(raw); !valid {
		b.add("external_id", "must be 1 to 128 of A-Z, a-z, 0-9, '.', '_', ':' and '-', and is never an email address")
	} else {
		in.ExternalID = v
	}
	in.DisplayName, _ = b.str("display_name", true, nil, 1, nameMax)
	in.PlanKey, _ = b.str("plan_key", true, planKeyShape, 1, 63)
	if tz, set := b.str("timezone", false, nil, 1, 64); set {
		in.Timezone = tz
	}
	meta, set, err := b.object("metadata", s.metadataMax)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	in.Metadata = []byte("{}")
	if set {
		in.Metadata = meta
	}
	if len(b.issues) == 0 && b.has("timezone") && (!timezoneShape.MatchString(in.Timezone) || zoneIssue(in.Timezone)) {
		invalidZone(w, r)
		return
	}
	if p := b.check(&contract.TenantCreate{}); p != nil {
		problem(w, r, p)
		return
	}
	t, err := s.store.CreateTenant(r.Context(), actor(r), in, s.open)
	if err != nil {
		var unavailable errProvision
		if errors.As(err, &unavailable) {
			s.log.Warn("could not write a new tenant's balances; the tenant was not created",
				"request_id", envelope.From(r.Context()).RequestID, "error", unavailable.err)
			envelope.Write(w, r, envelope.Error(contract.ErrorCodeServiceUnavailable,
				"the data store did not answer, so the tenant was not created").Retry(1000))
			return
		}
		s.fail(w, r, err)
		return
	}
	s.publish(r, []string{t.ID.String()})
	respond(w, r, http.StatusCreated, &t)
}

// errProvision is a failure to write the opening balances, which the route
// answers 503 rather than as the control plane failing.
type errProvision struct{ err error }

func (e errProvision) Error() string { return "provision balances: " + e.err.Error() }
func (e errProvision) Unwrap() error { return e.err }

// open computes each entitlement's first window from the anchor and writes
// the opening balances. The window is the engine's: the anchor is the start of
// window 0 (T-1, DR-002).
func (s *Service) open(ctx context.Context, o Opening) error {
	loc, err := time.LoadLocation(o.Timezone)
	if err != nil {
		return fmt.Errorf("load zone: %w", err)
	}
	openings := make([]store.Opening, 0, len(o.Entitlements))
	for _, e := range o.Entitlements {
		if e.Limit == nil || e.Interval == nil {
			// A boolean entitlement has no balance to open (DR-050).
			continue
		}
		iv, err := parseInterval(*e.Interval)
		if err != nil {
			return err
		}
		win := cycle.CurrentWindow(o.Anchor, iv, loc, o.Anchor)
		openings = append(openings, store.Opening{Feature: e.Feature, Limit: *e.Limit, Index: win.Index, Start: win.Start, End: win.End})
	}
	if err := s.data.Provision(ctx, o.TenantID, openings); err != nil {
		return errProvision{err: err}
	}
	return nil
}

func parseInterval(label string) (cycle.Interval, error) {
	for iv := cycle.Hourly; iv <= cycle.Never; iv++ {
		if iv.String() == label {
			return iv, nil
		}
	}
	return 0, fmt.Errorf("reset interval %q is not one this binary knows", label)
}

// updateTenant changes display_name, metadata, timezone and plan_key. A
// timezone change does not re-anchor: the next boundary is computed in the new
// zone (T-6, DR-033). A plan change does: it is a hard reset at the request's
// instant, committed with the tenant's new pins and a pending re-anchor that
// the request then drains inline (T-5, DR-013, D2).
func (s *Service) updateTenant(w http.ResponseWriter, r *http.Request) {
	id, valid := tenantID(w, r)
	if !valid {
		return
	}
	b, err := readBody(r, "version", "display_name", "plan_key", "timezone", "metadata")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	c := TenantChange{Version: b.version(), At: accepted(r)}
	if v, set := b.str("display_name", false, nil, 1, nameMax); set {
		c.DisplayName = &v
	}
	if v, set := b.str("plan_key", false, planKeyShape, 1, 63); set {
		c.PlanKey = &v
	}
	if v, set := b.str("timezone", false, nil, 1, 64); set {
		c.Timezone = &v
	}
	meta, set, err := b.object("metadata", s.metadataMax)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if set {
		c.Metadata = meta
	}
	if len(b.issues) == 0 && c.Timezone != nil && (!timezoneShape.MatchString(*c.Timezone) || zoneIssue(*c.Timezone)) {
		invalidZone(w, r)
		return
	}
	if p := b.check(&contract.TenantUpdate{}); p != nil {
		problem(w, r, p)
		return
	}
	if c.PlanKey != nil && s.readOnly() {
		// A plan change re-anchors the tenant's balances (IP-10), which
		// read-only mode forbids (IP-11 D5).
		readOnly(w, r)
		return
	}
	t, err := s.store.UpdateTenant(r.Context(), actor(r), id, c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if c.PlanKey != nil {
		s.drainInline(r, []string{t.ID.String()})
	}
	s.publish(r, []string{t.ID.String()})
	respond(w, r, http.StatusOK, &t)
}

func (s *Service) suspendTenant(w http.ResponseWriter, r *http.Request) {
	s.setState(w, r, "suspended")
}

func (s *Service) resumeTenant(w http.ResponseWriter, r *http.Request) {
	s.setState(w, r, "active")
}

// setState suspends or resumes. Neither touches a balance hash: suspension is
// state, so resuming continues from the same balance (DR-032, INV-T2).
func (s *Service) setState(w http.ResponseWriter, r *http.Request, to string) {
	id, valid := tenantID(w, r)
	if !valid {
		return
	}
	b, err := readBody(r, "version", "reason")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	version, reason := b.version(), b.reason()
	if p := b.check(&contract.ReasonRequest{}); p != nil {
		problem(w, r, p)
		return
	}
	t, err := s.store.SetTenantState(r.Context(), actor(r), id, version, reason, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.publish(r, []string{t.ID.String()})
	respond(w, r, http.StatusOK, &t)
}

// deleteTenant soft-deletes, which is terminal (T-7). The balance hashes are
// left to expire, and the data plane answers tenant_not_found within the
// propagation bound (INV-T1).
func (s *Service) deleteTenant(w http.ResponseWriter, r *http.Request) {
	id, valid := tenantID(w, r)
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
	t, err := s.store.DeleteTenant(r.Context(), actor(r), id, reason, accepted(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.publish(r, []string{t.ID.String()})
	respond(w, r, http.StatusOK, &t)
}
