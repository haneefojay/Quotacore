package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"time"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/store"
)

// The plan routes IP-10 serves: the entitlement edit, impact, and apply-now
// with its confirmation (DR-014, DR-046, Q-04, Q-14).

var confirmationShape = regexp.MustCompile(`^cnf_[0-9A-HJKMNP-TV-Z]{26}$`)

// updatePlan edits a plan. It changes the offer, so new assignments get it at
// once; an assigned tenant keeps its cadence and its feature set until its
// next re-anchor and takes a new limit at its own next boundary, and no
// balance or boundary moves (DR-014, D1).
func (s *Service) updatePlan(w http.ResponseWriter, r *http.Request) {
	id, valid := planID(w, r)
	if !valid {
		return
	}
	b, err := readBody(r, "version", "name", "description", "entitlements")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	c := PlanChange{Version: b.version(), At: accepted(r)}
	if v, set := b.str("name", false, nil, 1, nameMax); set {
		c.Name = &v
	}
	if v, set := b.str("description", false, nil, 0, textMax); set {
		c.Description = &v
	}
	if b.has("entitlements") {
		c.Entitlements = b.entitlements()
	}
	if p := b.check(&contract.PlanUpdate{}); p != nil {
		problem(w, r, p)
		return
	}
	p, tenants, err := s.store.UpdatePlan(r.Context(), actor(r), id, c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.publish(r, tenants)
	respond(w, r, http.StatusOK, &p)
}

// impact is what an apply-now of the plan as saved would touch: every
// non-deleted tenant on it, suspended ones included, and the earliest end of
// their current windows over their non-`never` features, rendered in the zone
// of the tenant it belongs to (D9). The 409 and the impact route compute it
// with this one function, so they cannot disagree.
type impact struct {
	tenants  int64
	earliest *time.Time
}

func impactOf(cadences []Cadence, at time.Time) (impact, error) {
	out := impact{tenants: int64(len(cadences))}
	for _, c := range cadences {
		loc, err := time.LoadLocation(c.Timezone)
		if err != nil {
			return impact{}, fmt.Errorf("load zone: %w", err)
		}
		for _, label := range c.Intervals {
			iv, err := parseInterval(label)
			if err != nil {
				return impact{}, err
			}
			end := cycle.CurrentWindow(c.Anchor, iv, loc, at).End
			if end == nil {
				continue
			}
			if out.earliest == nil || end.Before(*out.earliest) {
				local := end.In(loc)
				out.earliest = &local
			}
		}
	}
	return out, nil
}

func (i impact) end() contract.NilTimestamp {
	if i.earliest == nil {
		return contract.NilTimestamp{Null: true}
	}
	return contract.NewNilTimestamp(envelope.Boundary(*i.earliest, i.earliest.Location()))
}

// planImpact answers UC-19: how many tenants an apply-now would re-anchor and
// the earliest boundary among them. It writes nothing and issues nothing. It
// takes no query parameter, so one sent is refused rather than read as a
// preview of something the route does not compute (api-conventions §10.2).
func (s *Service) planImpact(w http.ResponseWriter, r *http.Request) {
	id, valid := planID(w, r)
	if !valid {
		return
	}
	if q := r.URL.Query(); len(q) > 0 {
		names := make([]string, 0, len(q))
		for name := range q {
			names = append(names, name)
		}
		sort.Strings(names)
		issues := make([]issue, 0, len(names))
		for _, name := range names {
			issues = append(issues, issue{Field: name, Issue: "is not a parameter of this route"})
		}
		problem(w, r, reject(issues))
		return
	}
	p, cadences, err := s.store.PlanCadences(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	im, err := impactOf(cadences, accepted(r))
	if err != nil {
		s.fail(w, r, refuse(envelope.Error(contract.ErrorCodeInternalError, "a tenant's cycle could not be computed")))
		return
	}
	respond(w, r, http.StatusOK, &contract.PlanImpact{
		PlanKey:                      p.PlanKey,
		AffectedTenants:              im.tenants,
		EarliestCycleEnd:             im.end(),
		ExceedsConfirmationThreshold: im.tenants > s.threshold,
	})
}

// applyAttempts bounds how often apply-now counts again when the plan moved
// between its count and its write.
const applyAttempts = 3

// applyPlan is apply-now (P-3, DR-046, D7). It counts first. A token is
// honoured once, and only for the plan, version and reason it was issued for,
// and only while the count has not grown past the one it was issued against.
// Anything else is a fresh count: above the threshold a 409 carrying a new
// token, which writes nothing else and audits nothing; at or below it, the
// apply. The apply re-anchors every tenant on the plan in one transaction,
// bumps the plan's version, so no token can be replayed after it, and is
// audited once with its reason.
func (s *Service) applyPlan(w http.ResponseWriter, r *http.Request) {
	id, valid := planID(w, r)
	if !valid {
		return
	}
	b, err := readBody(r, "reason", "confirmation_token")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	reason := b.reason()
	token, hasToken := b.str("confirmation_token", false, confirmationShape, 30, 30)
	if p := b.check(&contract.PlanApplyRequest{}); p != nil {
		problem(w, r, p)
		return
	}
	at := accepted(r)
	for attempt := range applyAttempts {
		p, cadences, err := s.store.PlanCadences(r.Context(), id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if p.State == contract.ArchiveStateArchived {
			s.fail(w, r, invalidField("id", "names an archived plan, which cannot be applied"))
			return
		}
		im, err := impactOf(cadences, at)
		if err != nil {
			s.fail(w, r, refuse(envelope.Error(contract.ErrorCodeInternalError, "a tenant's cycle could not be computed")))
			return
		}
		max := s.threshold
		if hasToken && attempt == 0 {
			c, found, err := s.data.TakeConfirmation(r.Context(), token)
			if err != nil {
				s.fail(w, r, errDataStore{err: err})
				return
			}
			if found && c.PlanID == p.ID.String() && c.Version == int64(p.Version) &&
				c.ReasonHash == reasonHash(reason) && im.tenants <= c.Tenants {
				max = c.Tenants
			}
		}
		if im.tenants > max || attempt == applyAttempts-1 {
			s.confirm(w, r, p, im, reason)
			return
		}
		applied, err := s.store.ApplyPlan(r.Context(), actor(r), id, ApplyInput{
			Version: int64(p.Version), Max: max, Reason: reason, At: at,
		})
		if errors.Is(err, ErrApplyMoved) {
			continue
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.drainInline(r, applied.Tenants)
		s.publish(r, applied.Tenants)
		respond(w, r, http.StatusOK, &contract.PlanApplied{
			PlanKey:         applied.Plan.PlanKey,
			AffectedTenants: int64(len(applied.Tenants)),
			AppliedAt:       contract.Timestamp(at.UTC()),
			Version:         applied.Plan.Version,
		})
		return
	}
}

// confirm is the 409: a new single-use token, stored with what it was
// counted against, and nothing else written (D7).
func (s *Service) confirm(w http.ResponseWriter, r *http.Request, p contract.Plan, im impact, reason string) {
	token, err := newConfirmationToken()
	if err != nil {
		s.fail(w, r, refuse(envelope.Error(contract.ErrorCodeInternalError, "a confirmation token could not be minted")))
		return
	}
	c := store.Confirmation{PlanID: p.ID.String(), Version: int64(p.Version), Tenants: im.tenants, ReasonHash: reasonHash(reason)}
	if err := s.data.PutConfirmation(r.Context(), token, c, s.cnfTTL); err != nil {
		s.fail(w, r, errDataStore{err: err})
		return
	}
	var earliest any
	if im.earliest != nil {
		earliest = time.Time(envelope.Boundary(*im.earliest, im.earliest.Location())).Format(time.RFC3339)
	}
	envelope.Write(w, r, envelope.Error(contract.ErrorCodeConfirmationRequired,
		fmt.Sprintf("this apply would re-anchor %d tenants", im.tenants)).
		With("affected_tenants", im.tenants).
		With("earliest_cycle_end", earliest).
		With("confirmation_token", token).
		With("threshold", s.threshold).
		With("expires_in_s", int64(s.cnfTTL/time.Second)).
		With("plan_version", int64(p.Version)))
}

// crockford is the alphabet of ULIDs and of the contract's token pattern.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newConfirmationToken is cnf_ and 26 random Crockford base32 characters,
// 130 bits from crypto/rand.
func newConfirmationToken() (string, error) {
	var raw [26]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	out := make([]byte, 0, 30)
	out = append(out, "cnf_"...)
	for _, b := range raw {
		out = append(out, crockford[b&31])
	}
	return string(out), nil
}

// reasonHash binds a token to the reason it was issued for without storing
// the reason in the fast store.
func reasonHash(reason string) string {
	sum := sha256.Sum256([]byte(reason))
	return hex.EncodeToString(sum[:])
}
