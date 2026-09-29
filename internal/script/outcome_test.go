package script

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

// decode is the boundary between two things that were written by hand on the two
// sides of a language boundary, so every test here is about refusing rather than
// about reading. A reply this code cannot read is either a script that changed
// shape or a channel that is not intact, and in both cases a plausible wrong
// number is the worst possible answer: a caller that believed a balance of 0
// because of a parse failure would deny a request that had been granted, or
// report a tenant as exhausted.
func TestDecodeReadsEveryShapeAScriptCanAnswerWith(t *testing.T) {
	const start, end = int64(1759200000000), int64(1761792000000)
	fields := func(decision, transition, balance, limit, bonus, index, endArg string) []any {
		return []any{decision, transition, balance, limit, bonus, index,
			strconv.FormatInt(start, 10), endArg}
	}
	cases := map[string]struct {
		reply      []any
		decision   Decision
		transition Transition
		balance    int64
		limit      int64
		bonus      int64
		index      int64
		end        *time.Time
	}{
		"applied in the window the caller asked about": {
			reply:    fields("applied", "current", "70", "100", "0", "5", strconv.FormatInt(end, 10)),
			decision: Applied, transition: Current, balance: 70, limit: 100, index: 5,
			end: ptr(time.UnixMilli(end).UTC()),
		},
		"applied after a rollover": {
			reply:    fields("applied", "rolled", "100", "100", "0", "6", strconv.FormatInt(end, 10)),
			decision: Applied, transition: Rolled, balance: 100, limit: 100, index: 6,
			end: ptr(time.UnixMilli(end).UTC()),
		},
		"applied for a caller whose window was behind the store": {
			reply:    fields("applied", "stale", "30", "100", "5", "9", strconv.FormatInt(end, 10)),
			decision: Applied, transition: Stale, balance: 30, limit: 100, bonus: 5, index: 9,
			end: ptr(time.UnixMilli(end).UTC()),
		},
		"denied, with the balance that was not spent": {
			reply:    fields("denied", "current", "12", "100", "0", "5", strconv.FormatInt(end, 10)),
			decision: Denied, transition: Current, balance: 12, limit: 100, index: 5,
			end: ptr(time.UnixMilli(end).UTC()),
		},
		"a refund refused at the ceiling, with the balance that did not move": {
			reply:    fields("refused_ceiling", "current", "100", "100", "0", "5", strconv.FormatInt(end, 10)),
			decision: RefusedCeiling, transition: Current, balance: 100, limit: 100, index: 5,
			end: ptr(time.UnixMilli(end).UTC()),
		},
		"an entitlement that never resets": {
			reply:    fields("applied", "current", "100", "100", "0", "0", ""),
			decision: Applied, transition: Current, balance: 100, limit: 100, index: 0, end: nil,
		},
		"a balance at the largest exact integer": {
			reply:    fields("applied", "current", "9007199254740991", "9007199254740991", "0", "5", strconv.FormatInt(end, 10)),
			decision: Applied, transition: Current, balance: 9007199254740991, limit: 9007199254740991, index: 5,
			end: ptr(time.UnixMilli(end).UTC()),
		},
		"a replay, carrying the state that was recorded": {
			reply:    fields("replayed", "current", "70", "100", "0", "5", strconv.FormatInt(end, 10)),
			decision: Replayed, transition: Current, balance: 70, limit: 100, index: 5,
			end: ptr(time.UnixMilli(end).UTC()),
		},
		"a replay of a call that itself rolled": {
			reply:    fields("replayed", "rolled", "100", "100", "0", "6", strconv.FormatInt(end, 10)),
			decision: Replayed, transition: Rolled, balance: 100, limit: 100, index: 6,
			end: ptr(time.UnixMilli(end).UTC()),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := decode(Consume, tc.reply)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if out.Decision != tc.decision || out.Transition != tc.transition {
				t.Errorf("decision, transition = %q, %q; want %q, %q", out.Decision, out.Transition, tc.decision, tc.transition)
			}
			if out.Balance != tc.balance || out.Limit != tc.limit || out.Bonus != tc.bonus || out.Index != tc.index {
				t.Errorf("balance, limit, bonus, index = %d, %d, %d, %d; want %d, %d, %d, %d",
					out.Balance, out.Limit, out.Bonus, out.Index, tc.balance, tc.limit, tc.bonus, tc.index)
			}
			if out.Start != time.UnixMilli(1759200000000).UTC() {
				t.Errorf("Start = %v, want the instant in the reply", out.Start)
			}
			if tc.end == nil {
				if out.End != nil {
					t.Errorf("End = %v, and this reply says the window has none", out.End)
				}
			} else if out.End == nil || !out.End.Equal(*tc.end) {
				t.Errorf("End = %v, want %v", out.End, tc.end)
			}
			// A replay is allowed: the operation it stands for was applied, and
			// reporting a successful call as a quota failure is the opposite of
			// what a retry is for.
			if wantAllowed := tc.decision == Applied || tc.decision == Replayed; out.Allowed() != wantAllowed {
				t.Errorf("Allowed() = %v for decision %q, want %v", out.Allowed(), tc.decision, wantAllowed)
			}
			if out.Replayed() != (tc.decision == Replayed) {
				t.Errorf("Replayed() = %v for decision %q", out.Replayed(), tc.decision)
			}
			if out.Rolled() != (tc.transition == Rolled) {
				t.Errorf("Rolled() = %v for transition %q", out.Rolled(), tc.transition)
			}
		})
	}
}

func TestDecodeRefusesAnythingItDoesNotUnderstand(t *testing.T) {
	full := func(decision, transition string) []any {
		return []any{decision, transition, "70", "100", "0", "5", "1759200000000", "1761792000000"}
	}
	cases := map[string]any{
		"no reply at all":              nil,
		"a string rather than a table": "applied",
		"a number rather than a table": int64(1),
		"a table of seven":             []any{"applied", "current", "70", "100", "0", "5", "1759200000000"},
		"a table of nine":              append(full("applied", "current"), "extra"),
		"a number where a string was promised": []any{
			"applied", "current", int64(70), "100", "0", "5", "1759200000000", "1761792000000",
		},
		"a decision that is not one of the seven":   full("granted", "current"),
		"a transition that is not one of the three": full("applied", "rewound"),
		"a negative balance":                        []any{"applied", "current", "-1", "100", "0", "5", "1759200000000", "1761792000000"},
		"a balance above 2^53":                      []any{"applied", "current", "9007199254740992", "100", "0", "5", "1759200000000", "1761792000000"},
		"a balance that is not a number":            []any{"applied", "current", "seventy", "100", "0", "5", "1759200000000", "1761792000000"},
		"a start that is not an instant":            []any{"applied", "current", "70", "100", "0", "5", "yesterday", "1761792000000"},
		"an end that is not an instant":             []any{"applied", "current", "70", "100", "0", "5", "1759200000000", "tomorrow"},
		"a missing state carried with the fault":    []any{"state_missing", "", "0", "0", "0", "0", "", ""},
		"an invalid argument carried with a state":  []any{"invalid", "", "70", "100", "0", "5", "1759200000000", ""},
		"a reused key":                              []any{"reused", "", "", "", "", "", "", ""},
		"a reused key carrying a balance":           []any{"reused", "current", "70", "100", "0", "5", "1759200000000", ""},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := decode(Refund, reply)
			if err == nil {
				t.Fatalf("decode returned %+v, and this reply is not one to interpret", out)
			}
			if out != (Outcome{}) {
				t.Errorf("decode returned %+v alongside its error; a refused reply must carry no state", out)
			}
		})
	}
}

// The two refusals a caller branches on are matched with errors.Is rather than by
// reading the message, because the caller turns them into HTTP statuses and
// request-lifecycle.md step 2 is specific about which.
func TestTheTwoNamedRefusalsAreRecognisable(t *testing.T) {
	if _, err := decode(Consume, []any{"state_missing", "", "", "", "", "", "", ""}); !errors.Is(err, ErrStateMissing) {
		t.Errorf("a state-missing reply gave %v, and the caller matches it with errors.Is", err)
	}
	if _, err := decode(Refund, []any{"invalid", "", "", "", "", "", "", ""}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("an invalid reply gave %v, and the caller matches it with errors.Is", err)
	}
	// A decision that is not in the set is neither: it is a defect in this code
	// or in the store's copy of a script, and naming it as one of the two
	// recognised refusals would send a defect out as a 503 the client retries.
	if _, err := decode(Check, []any{"unknown", "", "", "", "", "", "", ""}); errors.Is(err, ErrStateMissing) || errors.Is(err, ErrInvalidArgument) {
		t.Error("a decision that is not one of the seven was reported as a recognised refusal")
	}
}

// The third named refusal, and the one the whole mechanism exists for: a key
// that was already used for a different operation is refused as a 409 whose
// remedy is a new key, and it is never applied (DR-028).
func TestAReusedKeyIsItsOwnRefusal(t *testing.T) {
	if _, err := decode(Consume, []any{"reused", "", "", "", "", "", "", ""}); !errors.Is(err, ErrIdempotencyKeyReuse) {
		t.Errorf("a reused reply gave %v, and the caller matches it with errors.Is", err)
	}
	if _, err := decode(Refund, []any{"reused", "", "", "", "", "", "", ""}); !errors.Is(err, ErrIdempotencyKeyReuse) {
		t.Errorf("a reused refund gave %v, want the reuse marker", err)
	}
	// A missing key is the other half of the pair, and the two are different
	// codes: one is a client that forgot the header, the other is a client that
	// reused one.
	if errors.Is(ErrMissingIdempotencyKey, ErrIdempotencyKeyReuse) {
		t.Error("the missing-key and reused-key refusals are the same error, and they map to different codes")
	}
}
