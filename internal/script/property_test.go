package script

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/quotacore/quotacore/internal/cycle"
)

// The property tests of testing-strategy.md §3 that are about the scripts
// (IP-16 D10). A generator over valid operation sequences drives one balance
// hash through the real scripts against a real store, and a model that knows
// only the rules predicts every field after every step:
//
//   - 0 <= balance <= limit + bonus after every step (INV-X1, DR-019);
//   - cycle_index never decreases, whatever order the targets arrive in (C-3);
//   - a denial or a refusal changes nothing, byte for byte (T-08);
//   - the same Idempotency-Key never applies twice (DR-027);
//   - balance == opening + Σ(delta) within each cycle, where every delta is
//     the one the ledger would record from the request's own figures, so the
//     identity is not read back from the store it checks (DR-042).
//
// rapid shrinks a failure to its shortest sequence and prints it; testing-
// strategy §3 asks for that sequence to be committed as a named regression
// test. The per-commit run is rapid's default 100 sequences; the nightly run
// passes -rapid.checks for thousands.

var propertySeq atomic.Int64

// window i of the property's hourly calendar: [base + i h, base + (i+1) h).
var propertyBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func propertyWindow(i int64) cycle.Window {
	return fixedWindow(i, propertyBase.Add(time.Duration(i)*time.Hour), time.Hour)
}

// model is what the rules say the hash holds.
type model struct {
	balance, limit, bonus, index int64
	// planLimit is the limit the next rollover opens with: the plan's, which
	// an edit changes for the next cycle only (DR-014).
	planLimit int64
	// target is the window the caller computes now; it runs ahead of index
	// until a call rolls the store forward to it.
	target int64
	// opening and sum are the cycle's opening balance and the deltas the
	// ledger would record since, for DR-042.
	opening, sum int64
	// applied is each consume that applied, for replays.
	applied []Request
}

func TestPropertyTheScriptsKeepEveryInvariant(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()

	rapid.Check(t, func(rt *rapid.T) {
		feature := featureKey(t) + "-" + strconv.FormatInt(propertySeq.Add(1), 10) + "-" + strconv.FormatInt(runNonce, 36)
		key := keyFor(t, feature)
		defer s.Client().Del(ctx, key)

		limit := rapid.Int64Range(0, 1000).Draw(rt, "limit")
		m := &model{balance: limit, limit: limit, planLimit: limit, opening: limit}
		w := propertyWindow(0)
		seed(t, s, key, state{Balance: limit, Limit: limit, Index: 0, Start: w.Start, End: w.End})
		seq := 0

		// request is a call in the caller's current target window.
		request := func(amount int64) Request {
			seq++
			tw := propertyWindow(m.target)
			return Request{TenantID: testTenant, FeatureKey: feature, Amount: amount, Limit: m.planLimit,
				Window: tw, Interval: cycle.Hourly, At: tw.Start.Add(30 * time.Minute),
				IdempotencyKey: feature + "-op-" + strconv.Itoa(seq)}
		}
		// roll applies to the model what the shared transition does when the
		// caller's target is ahead of the store: the allowance is re-granted
		// once at the plan's limit and the bonus ends with the cycle (DR-045).
		roll := func(out Outcome) {
			if m.target > m.index {
				if out.Transition != Rolled {
					rt.Fatalf("the target %d is ahead of the store's %d and the script answered %q", m.target, m.index, out.Transition)
				}
				m.balance, m.limit, m.bonus, m.index = m.planLimit, m.planLimit, 0, m.target
				m.opening, m.sum = m.balance, 0
			} else if out.Transition == Rolled {
				rt.Fatalf("the script rolled with the target %d not ahead of the store's %d", m.target, m.index)
			}
		}
		apply := func(delta int64) {
			m.balance += delta
			m.sum += delta
		}
		var before map[string]string
		unchanged := func(what string) {
			if after := fields(t, s, key); !equal(before, after) {
				rt.Fatalf("%s changed the hash: %v became %v", what, before, after)
			}
		}

		rt.Repeat(map[string]func(*rapid.T){
			"consume": func(rt *rapid.T) {
				req := request(rapid.Int64Range(0, 2*m.limit+2).Draw(rt, "amount"))
				before = fields(t, s, key)
				out, err := runner.Consume(ctx, req)
				if err != nil {
					rt.Fatalf("consume: %v", err)
				}
				rolled := m.target > m.index
				roll(out)
				switch {
				case req.Amount <= m.balance:
					if out.Decision != Applied {
						rt.Fatalf("consume %d of %d: %q", req.Amount, m.balance, out.Decision)
					}
					apply(-req.Amount)
					m.applied = append(m.applied, req)
				default:
					if out.Decision != Denied {
						rt.Fatalf("consume %d of %d: %q", req.Amount, m.balance, out.Decision)
					}
					if !rolled {
						unchanged("a denied consume")
					}
				}
			},
			"refund": func(rt *rapid.T) {
				req := request(rapid.Int64Range(0, m.limit+2).Draw(rt, "amount"))
				before = fields(t, s, key)
				out, err := runner.Refund(ctx, req)
				if err != nil {
					rt.Fatalf("refund: %v", err)
				}
				rolled := m.target > m.index
				roll(out)
				if m.balance+req.Amount <= m.limit+m.bonus {
					if out.Decision != Applied {
						rt.Fatalf("refund %d onto %d under %d+%d: %q", req.Amount, m.balance, m.limit, m.bonus, out.Decision)
					}
					apply(req.Amount)
					return
				}
				if out.Decision != RefusedCeiling {
					rt.Fatalf("refund %d onto %d under %d+%d: %q", req.Amount, m.balance, m.limit, m.bonus, out.Decision)
				}
				if !rolled {
					unchanged("a refused refund")
				}
			},
			"grant": func(rt *rapid.T) {
				req := request(rapid.Int64Range(0, 500).Draw(rt, "amount"))
				out, err := runner.Grant(ctx, req)
				if err != nil {
					rt.Fatalf("grant: %v", err)
				}
				roll(out.Outcome)
				if out.Decision != Applied {
					rt.Fatalf("grant %d: %q", req.Amount, out.Decision)
				}
				m.bonus += req.Amount
				apply(req.Amount)
			},
			"set": func(rt *rapid.T) {
				req := request(0)
				value := rapid.Int64Range(0, m.limit+m.bonus+500).Draw(rt, "value")
				req.Amount = value
				before = fields(t, s, key)
				out, err := runner.Set(ctx, req)
				if err != nil {
					rt.Fatalf("set: %v", err)
				}
				rolled := m.target > m.index
				roll(out.Outcome)
				if value <= m.limit+m.bonus {
					if out.Decision != Applied || out.Before == nil || *out.Before != m.balance {
						rt.Fatalf("set %d over %d: %q, before %v", value, m.balance, out.Decision, out.Before)
					}
					apply(value - m.balance)
					return
				}
				if out.Decision != RefusedCeiling {
					rt.Fatalf("set %d above %d+%d: %q", value, m.limit, m.bonus, out.Decision)
				}
				if !rolled {
					unchanged("a refused set")
				}
			},
			"override": func(rt *rapid.T) {
				// An override applied now moves the open cycle's limit and its
				// balance by the same difference, floored at zero (IP-10 D6).
				req := request(0)
				req.Limit = rapid.Int64Range(0, 1000).Draw(rt, "limit")
				out, err := runner.Relimit(ctx, req)
				if err != nil {
					rt.Fatalf("relimit: %v", err)
				}
				// The transition runs first, at the limit the request carries.
				m.planLimit = req.Limit
				roll(out.Outcome)
				moved := max(0, m.balance+req.Limit-m.limit)
				if out.Decision != Applied {
					rt.Fatalf("relimit to %d: %q", req.Limit, out.Decision)
				}
				m.limit = req.Limit
				apply(moved - m.balance)
			},
			"plan edit": func(rt *rapid.T) {
				// A plan's limit edit reaches the tenant at its next roll and
				// changes no current balance (DR-014).
				m.planLimit = rapid.Int64Range(0, 1000).Draw(rt, "plan limit")
			},
			"boundary passes": func(rt *rapid.T) {
				m.target += rapid.Int64Range(1, 3).Draw(rt, "cycles")
			},
			"stale caller": func(rt *rapid.T) {
				// A caller behind the store, as a delayed worker or a slow
				// instance is: a check for nothing against an old window.
				if m.index == 0 {
					return
				}
				back := rapid.Int64Range(1, m.index).Draw(rt, "behind")
				tw := propertyWindow(m.index - back)
				before = fields(t, s, key)
				out, err := runner.Check(ctx, Request{TenantID: testTenant, FeatureKey: feature, Limit: m.planLimit,
					Window: tw, Interval: cycle.Hourly, At: tw.Start.Add(30 * time.Minute)})
				if err != nil {
					rt.Fatalf("check: %v", err)
				}
				if out.Transition != Stale {
					rt.Fatalf("a caller %d behind the store: %q", back, out.Transition)
				}
				unchanged("a stale caller")
			},
			"replay": func(rt *rapid.T) {
				if len(m.applied) == 0 {
					return
				}
				req := rapid.SampledFrom(m.applied).Draw(rt, "replayed")
				before = fields(t, s, key)
				out, err := runner.Consume(ctx, req)
				if err != nil {
					rt.Fatalf("replay: %v", err)
				}
				if out.Decision != Replayed {
					rt.Fatalf("a repeated key: %q", out.Decision)
				}
				unchanged("a replay")
			},
			"": func(rt *rapid.T) {
				h := fields(t, s, key)
				got := func(f string) int64 {
					n, err := strconv.ParseInt(h[f], 10, 64)
					if err != nil {
						rt.Fatalf("%s = %q", f, h[f])
					}
					return n
				}
				balance, lim, bonus, index := got("balance"), got("limit"), got("bonus"), got("cycle_index")
				if balance < 0 || balance > lim+bonus {
					rt.Fatalf("balance %d outside 0..%d+%d", balance, lim, bonus)
				}
				if index != m.index {
					rt.Fatalf("cycle_index %d, the model says %d", index, m.index)
				}
				if balance != m.balance || lim != m.limit || bonus != m.bonus {
					rt.Fatalf("hash %d/%d/%d, the model says %d/%d/%d", balance, lim, bonus, m.balance, m.limit, m.bonus)
				}
				if balance != m.opening+m.sum {
					rt.Fatalf("DR-042: balance %d != opening %d + deltas %d", balance, m.opening, m.sum)
				}
				if ttl := s.Client().PTTL(ctx, key).Val(); ttl != -1 {
					rt.Fatalf("the balance hash has an expiry of %v (INV-C3)", ttl)
				}
			},
		})
	})
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
