package script

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Decision is what a script decided about a request. It is a closed set of five,
// and the strings are the ones the scripts return, so a value that is not in
// this set means the reply and this code disagree and is refused rather than
// interpreted.
type Decision string

const (
	// Applied means the mutation was written, or - for Check - that the
	// request would fit in the remaining balance.
	Applied Decision = "applied"
	// Denied means the balance could not cover the amount. For Consume it is
	// quota_exceeded and the state is untouched (DR-025); for Check it is an
	// answer that fits in a 200.
	Denied Decision = "denied"
	// RefusedCeiling means a refund would have pushed the balance past
	// limit+bonus. The whole refund is refused and the balance is untouched
	// (DR-020).
	RefusedCeiling Decision = "refused_ceiling"
	// StateMissing is the marker of request-lifecycle.md step 2: the balance
	// hash is absent, or present with a field that is not a number, so there
	// is nothing to enforce against. It is 503 service_unavailable and it
	// writes nothing, because a balance is materialised at provisioning and at
	// plan assignment and at no other moment (DR-045).
	StateMissing Decision = "state_missing"
	// Invalid means the script would not accept the arguments it was given, so
	// it refused before reading or writing anything.
	Invalid Decision = "invalid"
)

// Transition is what the script found when it compared the caller's window with
// the store's, and it is reported separately from the decision because a stale
// caller still gets an answer: the decision is made against the state as it
// stands, and the verdict tells the caller its window was behind (C-3).
type Transition string

const (
	// Current means the store was already in the window the caller asked
	// about, so no boundary had closed.
	Current Transition = "current"
	// Rolled means a boundary closed, and the allowance was re-granted once,
	// here, inside the same execution as the decision (DR-045).
	Rolled Transition = "rolled"
	// Stale means the caller was behind the store: its window had not opened,
	// the store had already passed the index it named, or it named an index
	// ahead of a window that has not closed. Nothing was rolled and nothing
	// was refused; the request was answered against the live state.
	Stale Transition = "stale"
)

// Outcome is the state the store was left in, as the script saw it inside the
// one execution that decided it. Reading the balance from anywhere else would
// be a second read that can disagree with the decision (performance.md §3), so
// these fields are the answer and the reply is not followed by a read.
type Outcome struct {
	Decision   Decision
	Transition Transition
	// Balance is what the key holds now, after the decision. For a denial or a
	// refusal it is what it was, which is what makes "changed nothing"
	// checkable by the caller instead of assumed.
	Balance int64
	Limit   int64
	Bonus   int64
	// Index, Start and End are the cycle the store is on, which is not always
	// the cycle the caller asked about: a rolled outcome is on a later one, and
	// a stale outcome is on one the caller did not name. End is nil for a
	// `never` entitlement, which has no boundary and no expiry (DR-008).
	Index int64
	Start time.Time
	End   *time.Time
}

// Allowed reports whether the request fits in the balance, which is the question
// Check exists to answer and a decision rather than a status code.
func (o Outcome) Allowed() bool { return o.Decision == Applied }

// Rolled reports whether this execution crossed a boundary and re-granted the
// allowance. IP-13 counts it, and a test asserts that it happens on a crossed
// boundary and on no other occasion.
func (o Outcome) Rolled() bool { return o.Transition == Rolled }

// ErrStateMissing is the Go name for the state-missing marker. The caller maps
// it to 503 service_unavailable and to the missing-balance counter, both of
// which request-lifecycle.md step 2 owns.
var ErrStateMissing = errors.New("the balance is not there to enforce against")

// ErrInvalidArgument is a request this package or the script refused before
// touching the store. The caller maps it to 400: invalid_amount or
// validation_failed.
var ErrInvalidArgument = errors.New("the request would be refused as invalid")

// replyFields is the arity of every reply, and it is fixed. A table with a
// missing or an extra element would decode into the wrong field rather than
// fail, so both are refused.
const replyFields = 8

// decode turns a script's reply into an Outcome, or into an error. It never
// guesses: an unknown decision, an unknown verdict, a number where a string was
// promised, an empty string where a number was promised or a number outside the
// representable range is an error, because a plausible wrong number is worse
// than no number.
//
// A state_missing or invalid reply carries no values at all, and requiring that
// emptiness is deliberate: a script that answered a fault with a zero balance
// would let a caller that forgot to check the decision report a full
// exhaustion.
func decode(op Operation, raw any) (Outcome, error) {
	table, ok := raw.([]any)
	if !ok {
		return Outcome{}, fmt.Errorf("the %s script answered %T rather than a table of %d strings", op, raw, replyFields)
	}
	if len(table) != replyFields {
		return Outcome{}, fmt.Errorf("the %s script answered %d fields rather than %d", op, len(table), replyFields)
	}
	fields := make([]string, replyFields)
	for i, v := range table {
		s, isString := v.(string)
		if !isString {
			return Outcome{}, fmt.Errorf("the %s script answered %q in field %d rather than a string", op, v, i+1)
		}
		fields[i] = s
	}

	out := Outcome{
		Decision:   Decision(fields[0]),
		Transition: Transition(fields[1]),
	}
	switch out.Decision {
	case Applied, Denied, RefusedCeiling:
	case StateMissing:
		if err := requireEmpty(op, fields); err != nil {
			return Outcome{}, err
		}
		return Outcome{}, fmt.Errorf("the %s script: %w", op, ErrStateMissing)
	case Invalid:
		if err := requireEmpty(op, fields); err != nil {
			return Outcome{}, err
		}
		return Outcome{}, fmt.Errorf("the %s script: %w", op, ErrInvalidArgument)
	default:
		return Outcome{}, fmt.Errorf("the %s script answered the decision %q, which is not one of the five", op, fields[0])
	}

	switch out.Transition {
	case Current, Rolled, Stale:
	default:
		return Outcome{}, fmt.Errorf("the %s script answered the transition %q, which is not one of the three", op, fields[1])
	}

	numbers := []*int64{&out.Balance, &out.Limit, &out.Bonus, &out.Index}
	for i, dst := range numbers {
		n, err := parseCount(op, fields[i+2])
		if err != nil {
			return Outcome{}, err
		}
		*dst = n
	}
	start, err := parseMilli(op, fields[6])
	if err != nil {
		return Outcome{}, err
	}
	out.Start = time.UnixMilli(start).UTC()
	if fields[7] != "" {
		end, err := parseMilli(op, fields[7])
		if err != nil {
			return Outcome{}, err
		}
		converted := time.UnixMilli(end).UTC()
		out.End = &converted
	}
	return out, nil
}

// requireEmpty asserts that a fault carried no state at all.
func requireEmpty(op Operation, fields []string) error {
	for i, f := range fields[2:] {
		if f != "" {
			return fmt.Errorf("the %s script answered %q in field %d of a %s, which must carry no state", op, f, i+3, fields[0])
		}
	}
	return nil
}

func parseCount(op Operation, s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > bound {
		return 0, fmt.Errorf("the %s script answered %q where a count below 2^53 was promised", op, s)
	}
	return n, nil
}

func parseMilli(op Operation, s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("the %s script answered %q where a millisecond instant was promised", op, s)
	}
	return n, nil
}
