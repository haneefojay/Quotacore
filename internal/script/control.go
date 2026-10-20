package script

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/store"
)

// The control plane's scripts (IP-10). An admin route runs them inside the
// transaction that audits the change, never on a data-plane request, so the
// one-round-trip and latency rules of the data plane do not bind them; the
// atomicity rule does, which is why they are scripts rather than a read and a
// write from Go.
//
// Grant, set and relimit answer the data plane's nine fields and a tenth: the
// balance before the change, which the caller audits and hands the ledger.
// Reanchor and rollover run no shared transition and answer the eight fields of
// the state they wrote and a ninth, the balance they replaced.

// Moved is reanchor's answer when the stored window is no longer the one the
// caller read, so it wrote nothing (D2). It is a decision only reanchor gives.
const Moved Decision = "moved"

// ErrMoved is a reanchor whose expectation no longer held. The caller reads
// the hash again and decides again.
var ErrMoved = errors.New("the balance moved since it was read")

// AdminOutcome is a control-plane script's answer: the state it left behind,
// and the balance before the change. Before is nil when there was no balance
// to read, which only a re-anchor that materialises a hash can see.
type AdminOutcome struct {
	Outcome
	Before *int64
}

// Delta is the change to the balance, for the ledger. A re-anchor of a hash
// that was absent has no delta, because the balance it replaced is unknown.
func (o AdminOutcome) Delta() (int64, bool) {
	if o.Before == nil {
		return 0, false
	}
	return o.Balance - *o.Before, true
}

// Grant adds req.Amount to the balance and the bonus of the current cycle
// (DR-020, D3). A total the store cannot represent is RefusedCeiling.
func (r *Runner) Grant(ctx context.Context, req Request) (AdminOutcome, error) {
	return r.adjust(ctx, grantBody, req, req.Amount)
}

// Set makes req.Amount the current cycle's balance. Above limit + bonus is
// RefusedCeiling (DR-019, D4).
func (r *Runner) Set(ctx context.Context, req Request) (AdminOutcome, error) {
	return r.adjust(ctx, setBody, req, req.Amount)
}

// Relimit makes req.Limit the open cycle's limit and moves the balance by the
// same difference, floored at zero (D6).
func (r *Runner) Relimit(ctx context.Context, req Request) (AdminOutcome, error) {
	return r.adjust(ctx, relimitBody, req, req.Limit)
}

// adjust runs one of the three scripts that open with the shared transition.
// Their arguments are the data plane's first six, with the amount, the value
// or the new limit first.
func (r *Runner) adjust(ctx context.Context, b body, req Request, first int64) (AdminOutcome, error) {
	if first < 0 || first > bound {
		return AdminOutcome{}, fmt.Errorf("the %s script: %w: %d is negative or above 2^53", b.op, ErrInvalidArgument, first)
	}
	req.Amount = 0
	if err := req.validate(b.op); err != nil {
		return AdminOutcome{}, err
	}
	key, err := store.BalanceKey(req.TenantID, req.FeatureKey)
	if err != nil {
		return AdminOutcome{}, fmt.Errorf("the %s script: %w", b.op, err)
	}
	argv := args(Check, req)[:argCount-1]
	argv[0] = strconv.FormatInt(first, 10)
	return r.control(ctx, b, key, argv)
}

// Rewrite is a re-anchor or a force-rollover of one feature: the window the
// cycle engine computed under the tenant's anchor, and the limit it opens with.
type Rewrite struct {
	TenantID   string
	FeatureKey string
	Window     cycle.Window
	Limit      int64
	// Expect is what the caller read, for a re-anchor's compare-and-set. A
	// rollover overwrites whatever is stored and ignores it.
	Expect Expectation
}

// Expectation is the state a re-anchor was decided against: no hash, any hash
// (a corrupt one, which no request writes to), or a stored window by its index
// and start.
type Expectation struct {
	Absent bool
	Any    bool
	Index  int64
	Start  time.Time
}

func (e Expectation) args() (string, string) {
	switch {
	case e.Absent:
		return "absent", ""
	case e.Any:
		return "any", ""
	}
	return strconv.FormatInt(e.Index, 10), strconv.FormatInt(e.Start.UnixMilli(), 10)
}

// Reanchor writes the six fields of w's window, unless the stored window has
// moved from w.Expect, which is ErrMoved (D2).
func (r *Runner) Reanchor(ctx context.Context, w Rewrite) (AdminOutcome, error) {
	key, err := w.validate(Reanchor)
	if err != nil {
		return AdminOutcome{}, err
	}
	index, start := w.Expect.args()
	return r.control(ctx, reanchorBody, key, []any{
		strconv.FormatInt(w.Limit, 10),
		strconv.FormatInt(w.Window.Index, 10),
		strconv.FormatInt(w.Window.Start.UnixMilli(), 10),
		windowEndArg(w.Window),
		index, start,
	})
}

// Rollover refills w's window whatever is stored (D5). A `never` window has
// nothing to roll and is refused before the store is asked.
func (r *Runner) Rollover(ctx context.Context, w Rewrite) (AdminOutcome, error) {
	if w.Window.End == nil {
		return AdminOutcome{}, fmt.Errorf("the rollover script: %w: a `never` entitlement has no cycle to roll (DR-008)", ErrInvalidArgument)
	}
	key, err := w.validate(Rollover)
	if err != nil {
		return AdminOutcome{}, err
	}
	return r.control(ctx, rolloverBody, key, []any{
		strconv.FormatInt(w.Limit, 10),
		strconv.FormatInt(w.Window.Index, 10),
		strconv.FormatInt(w.Window.Start.UnixMilli(), 10),
		windowEndArg(w.Window),
	})
}

func (w Rewrite) validate(op Operation) (string, error) {
	switch {
	case w.Limit < 0 || w.Limit > bound:
		return "", fmt.Errorf("the %s script: %w: limit %d is negative or above 2^53", op, ErrInvalidArgument, w.Limit)
	case w.Window.Index < 0 || w.Window.Start.IsZero():
		return "", fmt.Errorf("the %s script: %w: the window was not computed", op, ErrInvalidArgument)
	case w.Window.End == nil && w.Window.Index != 0:
		return "", fmt.Errorf("the %s script: %w: an entitlement that never resets has index zero (DR-008)", op, ErrInvalidArgument)
	}
	key, err := store.BalanceKey(w.TenantID, w.FeatureKey)
	if err != nil {
		return "", fmt.Errorf("the %s script: %w", op, err)
	}
	return key, nil
}

// control runs one control-plane script, reloading the scripts once on
// NOSCRIPT like the data plane's.
func (r *Runner) control(ctx context.Context, b body, key string, argv []any) (AdminOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	raw, err := r.client.EvalSha(ctx, b.digest, []string{key}, argv...).Result()
	if isNoScript(err) {
		if loadErr := Load(ctx, r.client); loadErr != nil {
			return AdminOutcome{}, fmt.Errorf("the %s script was not in the store and could not be put back: %w", b.op, loadErr)
		}
		raw, err = r.client.EvalSha(ctx, b.digest, []string{key}, argv...).Result()
	}
	if err != nil {
		return AdminOutcome{}, err
	}
	return decodeControl(b.op, raw)
}

// decodeControl reads a control-plane reply: the state, decoded by the data
// plane's rules, then the balance before the change.
func decodeControl(op Operation, raw any) (AdminOutcome, error) {
	want := replyFields + 1
	if op == Reanchor || op == Rollover {
		want = stateFields + 1
	}
	table, ok := raw.([]any)
	if !ok || len(table) != want {
		return AdminOutcome{}, fmt.Errorf("the %s script answered %v rather than %d strings", op, raw, want)
	}
	fields, err := asStrings(op, table)
	if err != nil {
		return AdminOutcome{}, err
	}
	before := fields[want-1]
	// The state is nine fields; reanchor's and rollover's eight get an empty
	// closed balance, because they run no shared transition.
	state := append(fields[:want-1:want-1], "")[:replyFields]
	if Decision(fields[0]) == Moved {
		if op != Reanchor {
			return AdminOutcome{}, fmt.Errorf("the %s script answered moved, which only a re-anchor answers", op)
		}
		if err := requireEmpty(op, state); err != nil || before != "" {
			return AdminOutcome{}, fmt.Errorf("the %s script answered moved with state", op)
		}
		return AdminOutcome{}, fmt.Errorf("the %s script: %w", op, ErrMoved)
	}
	out, err := decodeFields(op, state)
	if err != nil {
		return AdminOutcome{}, err
	}
	switch out.Decision {
	case Applied, RefusedCeiling:
	default:
		return AdminOutcome{}, fmt.Errorf("the %s script answered the decision %q, which a control-plane script does not give", op, out.Decision)
	}
	res := AdminOutcome{Outcome: out}
	if before != "" {
		n, err := parseCount(op, before)
		if err != nil {
			return AdminOutcome{}, err
		}
		res.Before = &n
	}
	return res, nil
}
