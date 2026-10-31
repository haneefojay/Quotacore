package script

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/observability"
	"github.com/quotacore/quotacore/internal/store"
)

// bound is 2^53-1, the largest integer a double represents exactly. A balance,
// a limit and a bonus all sit below it, and the scripts refuse anything above
// it, because a number that cannot be represented exactly is a number that will
// not survive a round trip through Lua and come back changed (DR-022).
const bound = int64(1<<53 - 1)

// Runner executes the scripts against the fast store. It is safe for
// concurrent use: the bodies are immutable, the digest is fixed at build time
// and nothing here is written to after construction.
type Runner struct {
	client  redis.Scripter
	timeout time.Duration
	cycles  *observability.Cycles
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
	// Cycles counts what each execution's transition saw (IP-13 D2, D3). The
	// runner is the one place every script outcome passes, so no caller can
	// count one twice; nil counts nothing.
	Cycles *observability.Cycles
}

// New returns a Runner, or an error if it could not enforce anything.
func New(o Options) (*Runner, error) {
	if o.Client == nil {
		return nil, errors.New("script: no client, so there is nothing to run the scripts against")
	}
	if o.Timeout <= 0 {
		return nil, fmt.Errorf("script: the script timeout must be positive, got %s", o.Timeout)
	}
	return &Runner{client: o.Client, timeout: o.Timeout, cycles: o.Cycles}, nil
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
	// IdempotencyKey is the client's Idempotency-Key header, verbatim. It is
	// required for Consume and Refund and ignored by Check, because a check
	// changes nothing and has nothing to replay (DR-026). It is the one field a
	// caller sends as free text, so it reaches the store only through
	// store.IdempotencyKey, which refuses a brace or a colon rather than letting
	// a key split the tenant's hash slot (request-lifecycle.md §6).
	IdempotencyKey string
	// Amount is the debit or the credit, never signed: a negative amount is a
	// sign error on the wrong endpoint, not a refund (DR-023).
	Amount int64
	Window cycle.Window
	// Interval is the entitlement's cadence, which only labels the transition
	// counter; the window already carries everything the script reads.
	Interval cycle.Interval
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
	// The keys are built by internal/store and nowhere else, so a feature key or
	// an idempotency key carrying a brace or a colon cannot split a tenant
	// across slots by being concatenated somewhere else (data-model.md §3.1).
	key, err := store.BalanceKey(req.TenantID, req.FeatureKey)
	if err != nil {
		return Outcome{}, fmt.Errorf("the %s script: %w", b.op, err)
	}
	keys := []string{key}
	if b.op != Check {
		// The record lives in the same tenant's hash slot as the balance, which is
		// what keeps the script single-slot: the compare and the write have to be
		// one execution, and a record in another slot could not be (ADR-0002, A-11).
		idem, err := store.IdempotencyKey(req.TenantID, req.IdempotencyKey)
		if err != nil {
			return Outcome{}, fmt.Errorf("the %s script: %w", b.op, err)
		}
		keys = append(keys, idem)
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	out, err := r.eval(ctx, b, keys, req)
	if !isNoScript(err) {
		r.observe(req, out, err)
		return out, err
	}
	// The store's script cache was flushed, or the store restarted, and the
	// digest is gone. Put the scripts back and repeat the call once. The reload
	// and the retry share the budget above, so recovering costs a request
	// nothing in latency that it would not have spent anyway (ADR-0002).
	if loadErr := Load(ctx, r.client); loadErr != nil {
		return Outcome{}, fmt.Errorf("the %s script was not in the store and could not be put back: %w", b.op, loadErr)
	}
	out, err = r.eval(ctx, b, keys, req)
	r.observe(req, out, err)
	return out, err
}

// observe hands one execution's transition to the counters. A failed call
// decided nothing and an invalid or missing state transitioned nothing.
func (r *Runner) observe(req Request, out Outcome, err error) {
	if r.cycles == nil || err != nil || out.Transition == "" {
		return
	}
	r.cycles.Observe(observability.Transition{
		Interval: req.Interval.String(),
		Rolled:   out.Rolled(),
		Stale:    out.Transition == Stale,
		Replayed: out.Replayed(),
		Target:   req.Window.Index,
		Index:    out.Index,
		Start:    out.Start,
		At:       req.At,
		Balance:  out.Balance,
		Limit:    out.Limit,
		Bonus:    out.Bonus,
	})
}

// eval runs one script. There is exactly one command here and no second call
// for the result: the reply carries the state the decision left behind, because
// reading it back would be a second round trip that could disagree with the
// decision it is meant to describe (performance.md §3).
func (r *Runner) eval(ctx context.Context, b body, keys []string, req Request) (Outcome, error) {
	// EVALSHA, addressed by the digest of the embedded bytes. It is never EVAL
	// with a body attached: a flush costs a reload, whereas a body on the wire
	// would be a second copy of the rules on every request (NFR-S8, ADR-0002).
	raw, err := r.client.EvalSha(ctx, b.digest, keys, args(b.op, req)...).Result()
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
//
// Check reads seven like the other two and ignores the seventh, rather than the
// number being read from the operation. One arity for all three scripts means
// source_test.go can assert one number against all three bodies, and a script
// that reads fewer arguments than it is sent is the failure mode that is hard to
// see rather than hard to write.
const argCount = 7

// args is ARGV for all three scripts, in the order the Lua reads them. Seven
// values, each one a number, a hex fingerprint or the empty string, none of them
// assembled from anything a caller sent as text: the key is KEYS[1] and the
// tenant, the feature and the amount are already numbers by the time they get
// here. source_test.go asserts the arity against the scripts themselves, because
// seven on this side and six in the Lua would fail open rather than loudly.
//
// ARGV[7] is the fingerprint, and it is empty for Check, which has no record to
// compare against. Computing it for a check would be a SHA-256 on the request
// path for a value nothing reads (NFR-L5).
func args(op Operation, req Request) []any {
	fingerprint := ""
	if op != Check {
		fingerprint = Fingerprint(op, req.TenantID, req.FeatureKey, req.Amount)
	}
	return []any{
		strconv.FormatInt(req.Amount, 10),
		strconv.FormatInt(req.Window.Index, 10),
		strconv.FormatInt(req.Window.Start.UnixMilli(), 10),
		windowEndArg(req.Window),
		strconv.FormatInt(req.Limit, 10),
		strconv.FormatInt(req.At.UnixMilli(), 10),
		fingerprint,
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
//
// The missing key is refused here rather than by store.IdempotencyKey, which
// would report it as an empty name - true, but reported as a malformed key when
// it is a missing header, and the caller has two different codes for those
// (missing_idempotency_key and validation_failed). What the key is allowed to
// contain is still store.IdempotencyKey's business and is checked when the key
// is built.
func (r Request) validate(op Operation) error {
	switch {
	case (op == Consume || op == Refund) && r.IdempotencyKey == "":
		return fmt.Errorf("the %s script: %w", op, ErrMissingIdempotencyKey)
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

// ConsumeBoolean is a consume against a boolean entitlement (DR-050). There is
// no counter, so the decision is Applied, Replayed or an error: the amount is
// 0 or 1, an amount above 1 is the caller's feature_not_metered before this
// runs and ErrInvalidArgument here, and the one state written is the
// idempotency record, so a retry replays and a reused key is
// ErrIdempotencyKeyReuse exactly as for a metered consume. One round trip.
func (r *Runner) ConsumeBoolean(ctx context.Context, req Request) (Decision, error) {
	if req.IdempotencyKey == "" {
		return "", fmt.Errorf("the %s script: %w", ConsumeBoolean, ErrMissingIdempotencyKey)
	}
	if req.Amount < 0 || req.Amount > 1 {
		return "", fmt.Errorf("the %s script: %w: amount %d is not 0 or 1", ConsumeBoolean, ErrInvalidArgument, req.Amount)
	}
	key, err := store.BalanceKey(req.TenantID, req.FeatureKey)
	if err != nil {
		return "", fmt.Errorf("the %s script: %w", ConsumeBoolean, err)
	}
	idem, err := store.IdempotencyKey(req.TenantID, req.IdempotencyKey)
	if err != nil {
		return "", fmt.Errorf("the %s script: %w", ConsumeBoolean, err)
	}
	// The fingerprint is a consume's, so the same key on a metered consume of
	// the same feature and amount would read as the same operation; a
	// feature's kind never changes, so that pair cannot both exist.
	argv := []any{strconv.FormatInt(req.Amount, 10), "", "", "", "", "",
		Fingerprint(Consume, req.TenantID, req.FeatureKey, req.Amount)}
	return r.include(ctx, consumeBooleanBody, []string{key, idem}, argv)
}

// CheckBoolean is a check against a boolean entitlement: Applied, always, once
// the store has answered. It still costs the one round trip a metered check
// costs, so a boolean check fails with the same 503 a metered one does when
// the store is unreachable (DR-037, IP-17 DoD 4).
func (r *Runner) CheckBoolean(ctx context.Context, req Request) (Decision, error) {
	key, err := store.BalanceKey(req.TenantID, req.FeatureKey)
	if err != nil {
		return "", fmt.Errorf("the %s script: %w", CheckBoolean, err)
	}
	return r.include(ctx, checkBooleanBody, []string{key}, nil)
}

// include runs one of the two boolean scripts, reloading once on NOSCRIPT as
// run does, and reads the decision from the reply's first field.
func (r *Runner) include(ctx context.Context, b body, keys []string, argv []any) (Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	raw, err := r.client.EvalSha(ctx, b.digest, keys, argv...).Result()
	if isNoScript(err) {
		if loadErr := Load(ctx, r.client); loadErr != nil {
			return "", fmt.Errorf("the %s script was not in the store and could not be put back: %w", b.op, loadErr)
		}
		raw, err = r.client.EvalSha(ctx, b.digest, keys, argv...).Result()
	}
	if err != nil {
		return "", err
	}
	table, ok := raw.([]any)
	if !ok || len(table) == 0 {
		return "", fmt.Errorf("the %s script answered %T rather than a table", b.op, raw)
	}
	d, _ := table[0].(string)
	switch Decision(d) {
	case Applied, Replayed:
		return Decision(d), nil
	case Reused:
		return "", fmt.Errorf("the %s script: %w", b.op, ErrIdempotencyKeyReuse)
	case Invalid:
		return "", fmt.Errorf("the %s script: %w", b.op, ErrInvalidArgument)
	}
	return "", fmt.Errorf("the %s script answered the decision %q, which it cannot", b.op, d)
}
