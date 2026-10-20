package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/snapshot"
)

// The balance routes IP-10 serves: grant, set and force-rollover, and the
// override routes whose `immediate` mode re-limits the open cycle. Each runs
// under the tenant's lock after its pending re-anchor is drained, runs one
// script, and audits what the script reports in the same transaction (S4).

// adjustment is one balance route: the audit action, the ledger type, and the
// script it runs against the resolved entitlement.
type adjustment struct {
	action, event string
	// grant marks the one route whose ceiling is the representable bound
	// rather than limit plus bonus, because it raises the bonus with the
	// balance (D3).
	grant bool
	run   func(ctx context.Context, e held) (script.AdminOutcome, error)
}

// held is a tenant's resolved entitlement to one feature at one instant, with
// the window the cycle engine puts that instant in.
type held struct {
	tenant  *snapshot.Tenant
	feature string
	ent     snapshot.Entitlement
	loc     *time.Location
	window  cycle.Window
	at      time.Time
}

func (h held) request(amount int64) script.Request {
	return script.Request{TenantID: h.tenant.ID, FeatureKey: h.feature, Amount: amount,
		Window: h.window, Limit: h.ent.Limit, At: h.at}
}

// resolve is the effective entitlement of a freshly loaded tenant (S5). A
// feature the tenant does not hold, or one that is archived, is
// validation_failed naming feature_key (S1).
func (s *Service) resolve(t *snapshot.LoadedTenant, feature string) (held, error) {
	tenant := snapshot.NewTenant(t)
	at := s.now()
	e, ok := tenant.Entitlement(feature, at)
	if !ok || e.Archived {
		return held{}, invalidField("feature_key", "is not an entitlement the tenant holds, or is archived")
	}
	loc, err := time.LoadLocation(tenant.Timezone)
	if err != nil {
		return held{}, fmt.Errorf("load zone: %w", err)
	}
	return held{tenant: tenant, feature: feature, ent: e, loc: loc,
		window: cycle.CurrentWindow(tenant.Anchor, e.Interval, loc, at), at: at}, nil
}

// balanceAudit is what a balance route records before and after: the
// balance, and the cycle it is on.
type balanceAudit struct {
	FeatureKey string `json:"feature_key"`
	Balance    int64  `json:"balance"`
	Limit      int64  `json:"limit,omitempty"`
	Bonus      int64  `json:"bonus,omitempty"`
	CycleIndex int64  `json:"cycle_index"`
}

func (b *balanceAudit) MarshalJSON() ([]byte, error) {
	type plain balanceAudit
	return json.Marshal((*plain)(b))
}

func (s *Service) grant(w http.ResponseWriter, r *http.Request) {
	s.adjust(w, r, []string{"feature_key", "amount", "reason"}, &contract.GrantRequest{},
		func(b *body) adjustment {
			amount, _ := b.integer("amount", true, 0, limitCeiling)
			return adjustment{action: "tenant.grant", event: "granted", grant: true,
				run: func(ctx context.Context, h held) (script.AdminOutcome, error) {
					return s.scripts.Grant(ctx, h.request(amount))
				}}
		})
}

func (s *Service) setBalance(w http.ResponseWriter, r *http.Request) {
	s.adjust(w, r, []string{"feature_key", "value", "reason"}, &contract.SetRequest{},
		func(b *body) adjustment {
			value, _ := b.integer("value", true, 0, limitCeiling)
			return adjustment{action: "tenant.set", event: "set",
				run: func(ctx context.Context, h held) (script.AdminOutcome, error) {
					return s.scripts.Set(ctx, h.request(value))
				}}
		})
}

// forceRollover refills the computed current window of one feature (D5). A
// `never` entitlement has no cycle to roll (DR-008).
func (s *Service) forceRollover(w http.ResponseWriter, r *http.Request) {
	s.adjust(w, r, []string{"feature_key", "reason"}, &contract.ForceRolloverRequest{},
		func(*body) adjustment {
			return adjustment{action: "tenant.force_rollover", event: "force_rolled_over",
				run: func(ctx context.Context, h held) (script.AdminOutcome, error) {
					if h.ent.Interval == cycle.Never {
						return script.AdminOutcome{}, refuse(envelope.Error(contract.ErrorCodeResetNotSupported,
							"the feature never resets, so there is no cycle to roll over; grant instead").
							With("feature_key", h.feature).With("interval", cycle.Never.String()))
					}
					return s.scripts.Rollover(ctx, script.Rewrite{TenantID: h.tenant.ID, FeatureKey: h.feature,
						Window: h.window, Limit: h.ent.Limit})
				}}
		})
}

// adjust is the shared shape of grant, set and force-rollover.
func (s *Service) adjust(w http.ResponseWriter, r *http.Request, fields []string, generated interface {
	UnmarshalJSON([]byte) error
	Validate() error
}, read func(*body) adjustment) {
	id, valid := tenantID(w, r)
	if !valid {
		return
	}
	b, err := readBody(r, fields...)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	feature, _ := b.str("feature_key", true, featureKeyShape, 1, 64)
	a := read(b)
	reason := b.reason()
	if p := b.check(generated); p != nil {
		problem(w, r, p)
		return
	}
	var (
		out     script.AdminOutcome
		h       held
		evts    []dataplane.Event
		applied bool
	)
	tenant, err := s.store.AdjustBalance(r.Context(), actor(r), id, feature, s.rewrite,
		func(ctx context.Context, t *snapshot.LoadedTenant) (Audit, error) {
			var err error
			if h, err = s.resolve(t, feature); err != nil {
				return Audit{}, err
			}
			if out, err = a.run(ctx, h); err != nil {
				if errors.Is(err, script.ErrInvalidArgument) || isRefusal(err) {
					return Audit{}, err
				}
				return Audit{}, errDataStore{err: err}
			}
			if out.Decision == script.RefusedCeiling {
				return Audit{}, refuse(ceiling(feature, out, a.grant))
			}
			applied = true
			before := &balanceAudit{FeatureKey: feature, CycleIndex: out.Index}
			if out.Before != nil {
				before.Balance = *out.Before
			}
			after := &balanceAudit{FeatureKey: feature, Balance: out.Balance, Limit: out.Limit, Bonus: out.Bonus, CycleIndex: out.Index}
			if delta, known := out.Delta(); known && delta != 0 {
				evts = append(evts, dataplane.Event{
					RequestID: actor(r).RequestID, TenantID: h.tenant.ID, FeatureKey: feature, Type: a.event,
					Source: "admin", Amount: abs(delta), Delta: delta, Balance: out.Balance, CycleIndex: out.Index,
					OccurredAt: h.at,
				})
			}
			return Audit{Action: a.action, Before: before, After: after, Reason: reason}, nil
		})
	if err != nil {
		if applied {
			// The script ran and the transaction that audits it did not commit,
			// so a balance changed with no audit row. The reverse order would
			// audit a change that may not have happened (S4).
			s.log.Error("a balance change was applied and its audit row did not commit",
				"request_id", actor(r).RequestID, "action", a.action, "error", err)
		}
		s.fail(w, r, err)
		return
	}
	// The ledger is handed what committed, and only once it has (S6).
	for _, ev := range evts {
		s.events.Enqueue(ev)
	}
	s.publish(r, []string{tenant})
	respond(w, r, http.StatusOK, entry(feature, out, h.loc))
}

func isRefusal(err error) bool {
	var own *refusal
	return errors.As(err, &own)
}

// ceiling is grant_exceeds_ceiling with the state that refused it. For a set
// the ceiling is limit plus bonus (DR-019); for a grant it is the largest
// representable amount, because a grant raises the bonus with the balance and
// cannot break the other (D3).
func ceiling(feature string, out script.AdminOutcome, grant bool) envelope.Problem {
	c, msg := out.Limit+out.Bonus, "the balance would pass limit plus bonus; raise the limit with an override first"
	if grant {
		c, msg = limitCeiling, "the grant would take the balance or the bonus past 2^53 - 1"
	}
	return envelope.Error(contract.ErrorCodeGrantExceedsCeiling, msg).
		With("feature_key", feature).
		With("balance", out.Balance).
		With("limit", out.Limit).
		With("bonus", out.Bonus).
		With("ceiling", c)
}

// entry is the BalanceEntry a balance route answers, built as the data plane
// builds it, from the state the script left behind.
func entry(feature string, out script.AdminOutcome, loc *time.Location) *contract.BalanceEntry {
	consumed := out.Limit + out.Bonus - out.Balance
	if consumed < 0 {
		consumed = 0
	}
	e := &contract.BalanceEntry{
		FeatureKey: contract.FeatureKey(feature),
		Balance:    contract.Amount(out.Balance),
		Limit:      contract.Limit(out.Limit),
		Bonus:      contract.Amount(out.Bonus),
		Consumed:   contract.Amount(consumed),
		CycleStart: contract.NilTimestamp{Null: true},
		CycleEnd:   contract.NilTimestamp{Null: true},
	}
	if out.End != nil {
		e.CycleStart = contract.NewNilTimestamp(contract.Timestamp(out.Start.In(loc)))
		e.CycleEnd = contract.NewNilTimestamp(contract.Timestamp(out.End.In(loc)))
	}
	return e
}

// nullable reads a field the contract makes required and nullable: absent is
// an issue, null is (nil, true).
func (b *body) nullable(name string) (json.RawMessage, bool) {
	raw, ok := b.fields[name]
	if !ok {
		b.add(name, "is required, and null means inherit from the plan")
		return nil, false
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, true
	}
	return raw, true
}

// applyAt reads an override's mode, immediate unless sent.
func (b *body) applyAt(required bool) string {
	v, set := b.str("apply_at", required, nil, 1, 16)
	switch {
	case !set:
		return "immediate"
	case v != "immediate" && v != "next_boundary":
		b.add("apply_at", "must be immediate or next_boundary")
	}
	return v
}

// upsertOverride sets or replaces a tenant's override for one feature
// (UC-13). Its limit binds the open cycle at once with `immediate` and from
// the next boundary with `next_boundary`; its interval reaches the tenant at
// its next re-anchor, because a live interval would renumber the open cycle
// (D1, D6).
func (s *Service) upsertOverride(w http.ResponseWriter, r *http.Request) {
	id, valid := tenantID(w, r)
	if !valid {
		return
	}
	b, err := readBody(r, "feature_key", "limit_value", "interval", "apply_at", "version")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	in := OverrideInput{}
	in.Feature, _ = b.str("feature_key", true, featureKeyShape, 1, 64)
	if raw, present := b.nullable("limit_value"); present && raw != nil {
		if v, ok := b.integer("limit_value", true, 0, limitCeiling); ok {
			in.Limit = &v
		}
	}
	if raw, present := b.nullable("interval"); present && raw != nil {
		if v, ok := b.str("interval", true, nil, 1, 16); ok {
			if !intervals[v] {
				b.add("interval", "must be hourly, daily, weekly, monthly, yearly or never")
			}
			in.Interval = &v
		}
	}
	if len(b.issues) == 0 && in.Limit == nil && in.Interval == nil {
		b.add("body", "an override sets limit_value, interval or both")
	}
	in.ApplyAt = b.applyAt(false)
	if b.has("version") {
		v := b.version()
		in.Version = &v
	}
	if p := b.check(&contract.OverrideUpsert{}); p != nil {
		problem(w, r, p)
		return
	}
	o, tenant, err := s.store.UpsertOverride(r.Context(), actor(r), id, in, s.rewrite, s.relimit(in.Feature, in.ApplyAt))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.publish(r, []string{tenant})
	respond(w, r, http.StatusOK, &o)
}

// clearOverride removes a tenant's override for one feature, so the plan's
// value applies again, at once or from the next boundary (UC-13, D6).
// Clearing an override that does not exist changes nothing (S9).
func (s *Service) clearOverride(w http.ResponseWriter, r *http.Request) {
	id, valid := tenantID(w, r)
	if !valid {
		return
	}
	feature, valid := featureKey(w, r)
	if !valid {
		return
	}
	b, err := readBody(r, "apply_at")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	applyAt := b.applyAt(true)
	if p := b.check(&contract.OverrideClearRequest{}); p != nil {
		problem(w, r, p)
		return
	}
	tenant, cleared, err := s.store.ClearOverride(r.Context(), actor(r), id, feature, applyAt, s.rewrite,
		s.relimit(feature, applyAt))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if cleared {
		s.publish(r, []string{tenant})
	}
	noContent(w, r)
}

// relimit is what an override write does to the open cycle once its row is
// written: with `immediate`, the tenant's effective limit now binds the open
// cycle and the balance moves by the same difference (D6). A feature the
// tenant does not hold yet, which a plan reaches only at a re-anchor, has no
// open cycle to re-limit.
func (s *Service) relimit(feature, applyAt string) func(context.Context, *snapshot.LoadedTenant) error {
	return func(ctx context.Context, t *snapshot.LoadedTenant) error {
		if applyAt != "immediate" {
			return nil
		}
		h, err := s.resolve(t, feature)
		if isRefusal(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := s.scripts.Relimit(ctx, h.request(0)); err != nil {
			return errDataStore{err: err}
		}
		return nil
	}
}
