package script

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/store"
)

// bound is 2^53-1, the largest integer a double represents exactly. A balance,
// a limit and a bonus all sit below it, and the scripts refuse anything above
// it, because a number that cannot be represented exactly is a number that will
// not survive a round trip through Lua and come back changed (DR-022).
const bound = int64(1<<53 - 1)

// Runner executes the three scripts against the fast store. It is safe for
// concurrent use: the bodies are immutable, the digest is fixed at build time
// and nothing here is written to after construction.
type Runner struct {
	client  redis.Scripter
	timeout time.Duration
}

// Options are the two things a Runner needs. The client is an interface rather
// than a concrete type so that a test can count the calls a request makes, which
// is the only way to assert the one-round-trip rule instead of assuming it
// (Definition of Done item 9).
type Options struct {
	Client redis.Scripter
	// Timeout is the hard budget for one call, from the first byte to the last,
	// and it covers the reload that follows a flushed cache rather than adding
	// to it: 250 ms as NFR-L4 requires. It must be positive, because a runner
	// with no budget would wait forever on the one thing a request path may not
	// do.
	Timeout time.Duration
}

// New returns a Runner, or an error if it could not enforce anything.
func New(o Options) (*Runner, error) {
	if o.Client == nil {
		return nil, errors.New("script: no client, so there is nothing to run the scripts against")
	}
	if o.Timeout <= 0 {
		return nil, fmt.Errorf("script: the script timeout must be positive, got %s", o.Timeout)
	}
	return &Runner{client: o.Client, timeout: o.Timeout}, nil
}

// Client is the store this runner enforces against, for the one caller that has
// a reason to touch the store directly rather than through a decision: startup,
// which loads the three scripts and must load them through the same client the
// runner will use, so a startup success is a statement about the runner's
// connection rather than about a second one.
func (r *Runner) Client() redis.Scripter { return r.client }

// Request is everything one call needs, and nothing else. The window and the
// limit are values the caller has already computed, because cycle arithmetic
// lives in internal/cycle and nowhere else; this package performs none of it.
//
// At is the instant the caller chose its window with, and the same instant must
// be passed as the script's clock, because the two must agree: the scripts use
// At to decide whether a boundary has closed, and a Window computed from a
// different instant would be the rollover of a cycle the request is not in. The
// scripts defend against a mismatch rather than trusting it, so the failure
// mode is a stale verdict and a state that did not move, never a wrong grant.
type Request struct {
	TenantID   string
	FeatureKey string
	// Amount is the debit or the credit, never signed: a negative amount is a
	// sign error on the wrong endpoint, not a refund (DR-023).
	Amount int64
	Window cycle.Window
	// Limit is the effective limit for that window, after any reduction the plan
	// applies (DR-018). It is the figure a rollover re-grants, so passing the
	// nominal limit here would silently undo a reduction.
	Limit int64
	At    time.Time
}

// Consume debits the amount, or reports that the balance could not cover it.
// A denial is not an error here: it is an Outcome whose Balance is unchanged,
// and mapping it to 429 quota_exceeded with the cycle end beside it is the
// caller's job (DR-025).
func (r *Runner) Consume(ctx context.Context, req Request) (Outcome, error) {
	return r.run(ctx, consumeBody, req)
}

// Refund credits the amount, or refuses it whole if it would break the ceiling.
// The ceiling is the limit plus the bonus, and a refund is never clamped
// (DR-020, ADR-0005).
func (r *Runner) Refund(ctx context.Context, req Request) (Outcome, error) {
	return r.run(ctx, refundBody, req)
}

// Check reports whether the amount would fit, and the state either way, without
// spending anything. It is the one call of the three whose answer is a 200
// (DR-026).
func (r *Runner) Check(ctx context.Context, req Request) (Outcome, error) {
	return r.run(ctx, checkBody, req)
}

func (r *Runner) run(ctx context.Context, b body, req Request) (Outcome, error) {
	if err := req.validate(b.op); err != nil {
		return Outcome{}, err
	}
	// The key is built by internal/store and nowhere else, so a feature key
	// carrying a brace or a colon cannot split a tenant across slots by being
	// concatenated somewhere else (data-model.md §3.1).
	key, err := store.BalanceKey(req.TenantID, req.FeatureKey)
	if err != nil {
		return Outcome{}, fmt.Errorf("the %s script: %w", b.op, err)
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	out, err := r.eval(ctx, b, key, req)
	if !isNoScript(err) {
		return out, err
	}
	// The store's script cache was flushed, or the store restarted, and the
	// digest is gone. Put the three back and repeat the call once. The reload
	// and the retry share the budget above, so recovering costs a request
	// nothing in latency that it would not have spent anyway (ADR-0002).
	if loadErr := Load(ctx, r.client); loadErr != nil {
		return Outcome{}, fmt.Errorf("the %s script was not in the store and could not be put back: %w", b.op, loadErr)
	}
	return r.eval(ctx, b, key, req)
}

// eval runs one script. There is exactly one command here and no second call
// for the result: the reply carries the state the decision left behind, because
// reading it back would be a second round trip that could disagree with the
// decision it is meant to describe (performance.md §3).
func (r *Runner) eval(ctx context.Context, b body, key string, req Request) (Outcome, error) {
	// EVALSHA, addressed by the digest of the embedded bytes. It is never EVAL
	// with a body attached: a flush costs a reload, whereas a body on the wire
	// would be a second copy of the rules on every request (NFR-S8, ADR-0002).
	raw, err := r.client.EvalSha(ctx, b.digest, []string{key}, args(req)...).Result()
	if err != nil {
		return Outcome{}, err
	}
	return decode(b.op, raw)
}

// isNoScript reports the one error that is worth retrying, and it accepts both
// forms the client library hands back: the exported sentinel when the message
// is exactly the one the library knows, and the raw server prefix when it is
// not. Comparing the message alone would make a version of the server that
// words it differently look like a store failure.
func isNoScript(err error) bool {
	return errors.Is(err, redis.ErrNoScript) || redis.HasErrorPrefix(err, "NOSCRIPT")
}

// argCount is how many arguments the three scripts read, and the scripts and
// this constant are asserted equal to one another by source_test.go. It is a
// constant rather than len(args(...)) because a slice length is what the
// implementation happens to return today, and the number the Lua reads is the
// thing that has to match.
const argCount = 6

// args is ARGV for all three scripts, in the order the Lua reads them. Six
// values, each one a number or the empty string, none of them assembled from
// anything a caller sent as text: the key is KEYS[1] and the tenant, the feature
// and the amount are already numbers by the time they get here. source_test.go
// asserts the arity against the scripts themselves, because six on this side and
// five in the Lua would fail open rather than loudly.
func args(req Request) []any {
	return []any{
		strconv.FormatInt(req.Amount, 10),
		strconv.FormatInt(req.Window.Index, 10),
		strconv.FormatInt(req.Window.Start.UnixMilli(), 10),
		windowEndArg(req.Window),
		strconv.FormatInt(req.Limit, 10),
		strconv.FormatInt(req.At.UnixMilli(), 10),
	}
}

// windowEndArg is ARGV[4], and it is the only argument that may be empty: an
// entitlement that never resets has no boundary, and the empty string is how
// that crosses into Lua without a sentinel number a caller could collide with.
func windowEndArg(w cycle.Window) string {
	if w.End == nil {
		return ""
	}
	return strconv.FormatInt(w.End.UnixMilli(), 10)
}

// validate refuses a request this package can tell is wrong, before a byte goes
// to the store. The scripts repeat the amount and limit checks, because this
// package is not the only thing that can call them and a second caller must not
// be able to skip them; the duplication is deliberate, and source_test.go
// asserts the two copies of the bound are the same number.
func (r Request) validate(op Operation) error {
	switch {
	case r.Amount < 0 || r.Amount > bound:
		return fmt.Errorf("the %s script: %w: amount %d is negative or above 2^53", op, ErrInvalidArgument, r.Amount)
	case r.Limit < 0 || r.Limit > bound:
		return fmt.Errorf("the %s script: %w: limit %d is negative or above 2^53", op, ErrInvalidArgument, r.Limit)
	case r.Window.Index < 0:
		return fmt.Errorf("the %s script: %w: cycle index %d is negative", op, ErrInvalidArgument, r.Window.Index)
	case r.Window.Start.IsZero() || r.At.IsZero():
		return fmt.Errorf("the %s script: %w: the window and the instant must both have been computed", op, ErrInvalidArgument)
	case r.Window.End == nil && r.Window.Index != 0:
		return fmt.Errorf("the %s script: %w: an entitlement that never resets has index zero (DR-008)", op, ErrInvalidArgument)
	case r.Window.End != nil && !r.Window.End.After(r.Window.Start):
		return fmt.Errorf("the %s script: %w: the window ends at or before it starts", op, ErrInvalidArgument)
	}
	return nil
}
