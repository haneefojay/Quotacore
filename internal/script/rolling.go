package script

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/quotacore/quotacore/internal/rolling"
	"github.com/quotacore/quotacore/internal/store"
)

// RollingRequest is one call against a rolling-window entitlement (DR-052).
// It carries no cycle: the window and the instant are all the scripts read.
type RollingRequest struct {
	TenantID   string
	FeatureKey string
	// IdempotencyKey is required for a consume and a refund and ignored by a
	// check, as for the anchored scripts (DR-026).
	IdempotencyKey string
	Amount         int64
	Limit          int64
	Window         rolling.Window
	// At is the instant the request was accepted, its one clock read.
	At time.Time
	// Binding is the key's tenant, as for Request (DR-053).
	Binding string
}

// RollingOutcome is a rolling window as the script left it, read inside the
// one execution that decided the request.
type RollingOutcome struct {
	// Decision is Applied, Denied, RefusedCeiling or Replayed; the faults are
	// errors, as for Outcome.
	Decision Decision
	// Remaining is the limit less the window sum, never below zero.
	Remaining int64
	Limit     int64
	// Sum is the units counted in the window, after the decision.
	Sum int64
	// Start is the start of the oldest bucket the window counts.
	Start time.Time
	// NextRelease is when the oldest counted bucket leaves the window, which is
	// the earliest instant a unit comes back; nil when nothing is counted.
	NextRelease *time.Time
}

// Allowed reports whether a consume applied, or a check would.
func (o RollingOutcome) Allowed() bool { return o.Decision == Applied || o.Decision == Replayed }

// Replayed reports a replay of a recorded consume or refund (DR-027).
func (o RollingOutcome) Replayed() bool { return o.Decision == Replayed }

// ConsumeRolling adds the amount to the window's current bucket if the window
// sum plus the amount is within the limit, or reports Denied and changes
// nothing (DR-017). One round trip.
func (r *Runner) ConsumeRolling(ctx context.Context, req RollingRequest) (RollingOutcome, error) {
	return r.rolling(ctx, consumeRollingBody, req)
}

// CheckRolling reports whether the amount would fit, and writes nothing.
func (r *Runner) CheckRolling(ctx context.Context, req RollingRequest) (RollingOutcome, error) {
	return r.rolling(ctx, checkRollingBody, req)
}

// RefundRolling returns the amount to the window from its newest buckets, or
// reports RefusedCeiling when the window holds less than the amount (DR-019).
func (r *Runner) RefundRolling(ctx context.Context, req RollingRequest) (RollingOutcome, error) {
	return r.rolling(ctx, refundRollingBody, req)
}

func (r *Runner) rolling(ctx context.Context, b body, req RollingRequest) (RollingOutcome, error) {
	switch {
	case req.Amount < 0 || req.Amount > bound:
		return RollingOutcome{}, fmt.Errorf("the %s script: %w: amount %d is negative or above 2^53", b.op, ErrInvalidArgument, req.Amount)
	case req.Limit < 0 || req.Limit > bound:
		return RollingOutcome{}, fmt.Errorf("the %s script: %w: limit %d is negative or above 2^53", b.op, ErrInvalidArgument, req.Limit)
	case req.Window.IsZero() || req.At.IsZero():
		return RollingOutcome{}, fmt.Errorf("the %s script: %w: the window and the instant must both be set", b.op, ErrInvalidArgument)
	case b.op != CheckRolling && req.IdempotencyKey == "":
		return RollingOutcome{}, fmt.Errorf("the %s script: %w", b.op, ErrMissingIdempotencyKey)
	}
	key, err := store.BucketCounterKey(req.TenantID, req.FeatureKey)
	if err != nil {
		return RollingOutcome{}, fmt.Errorf("the %s script: %w", b.op, err)
	}
	keys := []string{key}
	fingerprint := ""
	if b.op != CheckRolling {
		idem, err := store.IdempotencyKey(req.TenantID, req.IdempotencyKey)
		if err != nil {
			return RollingOutcome{}, fmt.Errorf("the %s script: %w", b.op, err)
		}
		keys = append(keys, idem)
		op := Consume
		if b.op == RefundRolling {
			op = Refund
		}
		// A consume's or a refund's fingerprint, as the anchored scripts
		// compute it: a feature never changes kind, so the same key cannot
		// name an anchored and a rolling operation on one feature.
		fingerprint = Fingerprint(op, req.TenantID, req.FeatureKey, req.Amount)
	}
	argv := []any{
		strconv.FormatInt(req.Amount, 10),
		strconv.FormatInt(req.Limit, 10),
		strconv.FormatInt(req.Window.Duration().Milliseconds(), 10),
		strconv.FormatInt(req.At.UnixMilli(), 10),
		"", "",
		fingerprint,
		req.Binding,
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	raw, err := r.client.EvalSha(ctx, b.digest, keys, argv...).Result()
	if isNoScript(err) {
		if loadErr := Load(ctx, r.client); loadErr != nil {
			return RollingOutcome{}, fmt.Errorf("the %s script was not in the store and could not be put back: %w", b.op, loadErr)
		}
		raw, err = r.client.EvalSha(ctx, b.digest, keys, argv...).Result()
	}
	if err != nil {
		return RollingOutcome{}, err
	}
	// The reply is the anchored scripts' nine fields, read by the same
	// decoder, so a replayed record from the shared idempotency block reads
	// back the same way: the balance position holds the remaining, the index
	// position the window sum, and the end position the next release.
	out, err := decode(b.op, raw)
	if err != nil {
		return RollingOutcome{}, err
	}
	if out.Transition != Current || out.Bonus != 0 {
		return RollingOutcome{}, fmt.Errorf("the %s script answered a %s transition or a bonus of %d, which a rolling window has none of",
			b.op, out.Transition, out.Bonus)
	}
	return RollingOutcome{
		Decision:    out.Decision,
		Remaining:   out.Balance,
		Limit:       out.Limit,
		Sum:         out.Index,
		Start:       out.Start,
		NextRelease: out.End,
	}, nil
}
