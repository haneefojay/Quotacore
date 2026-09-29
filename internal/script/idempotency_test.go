package script

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTheFingerprintIsHexOfTheRightLength(t *testing.T) {
	fp := Fingerprint(Consume, testTenant, "llm.tokens.output", 30)
	if len(fp) != fingerprintLength {
		t.Fatalf("the fingerprint is %d characters and fingerprintLength says %d", len(fp), fingerprintLength)
	}
	for _, r := range fp {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("the fingerprint %q contains %q, and the record frames it as hexadecimal", fp, r)
		}
	}
}

func TestTheFingerprintIsStable(t *testing.T) {
	// Two attempts at the same logical operation must produce the same
	// fingerprint, because recognising a replay is a comparison between two of
	// them.
	first := Fingerprint(Refund, testTenant, "llm.tokens.output", 30)
	second := Fingerprint(Refund, testTenant, "llm.tokens.output", 30)
	if first != second {
		t.Errorf("the same request produced %s and %s; a replay would never be recognised", first, second)
	}
}

func TestTheFingerprintChangesWithEveryPartOfTheOperation(t *testing.T) {
	// DR-027 replays a repeat whose operation, tenant, feature and amount all
	// match; DR-028 refuses one whose fingerprint differs. So each of the four
	// has to move the digest, or one of those two rules cannot be implemented
	// at all - and a field that does not move it is a field the record cannot
	// distinguish.
	base := Fingerprint(Consume, testTenant, "llm.tokens.output", 30)
	for _, c := range []struct {
		name string
		got  string
	}{
		{"a different operation", Fingerprint(Refund, testTenant, "llm.tokens.output", 30)},
		{"a different tenant", Fingerprint(Consume, "1b1c4d2e-0f3a-4a5b-8c6d-7e8f9a0b1c2d", "llm.tokens.output", 30)},
		{"a different feature", Fingerprint(Consume, testTenant, "llm.tokens.input", 30)},
		{"a different amount", Fingerprint(Consume, testTenant, "llm.tokens.output", 31)},
		{"an amount of zero", Fingerprint(Consume, testTenant, "llm.tokens.output", 0)},
		{"no feature key at all", Fingerprint(Consume, testTenant, "", 30)},
	} {
		if c.got == base {
			t.Errorf("%s produces the fingerprint %s, the same one as the request it has to be told apart from", c.name, base)
		}
	}
}

func TestTheFingerprintCannotBeShiftedByTheContentsOfAField(t *testing.T) {
	// A separator-joined hash is unambiguous only while no field can contain the
	// separator, and a feature key is customer-supplied text. These two pairs
	// would each collide under a NUL-joined hash; the first because the NUL moves
	// the boundary between two fields, the second because it is the boundary
	// itself. A collision here is a customer's second operation being replayed as
	// their first, which is the double charge the whole mechanism exists to
	// prevent, so the fields are length-prefixed instead.
	for _, c := range []struct {
		name string
		a    string
		b    string
	}{
		{"a NUL inside the feature key", Fingerprint(Consume, testTenant, "a\x00b", 30), Fingerprint(Consume, testTenant, "a", 30)},
		{"a NUL that is the whole separator", Fingerprint(Consume, testTenant, "a\x00", 30), Fingerprint(Consume, testTenant, "a", 30)},
	} {
		if c.a == c.b {
			t.Errorf("%s: two different operations hash to %s, so the fields are not length-prefixed", c.name, c.a)
		}
	}
}

func TestTheFingerprintExcludesTheRequestId(t *testing.T) {
	// idempotency-and-retries.md names this as the small detail that is very
	// easy to get wrong and produces a system where retries always fail: a retry
	// carries a new request id by definition, so a fingerprint that included one
	// would make every repeat a mismatch and 409 every time. The function takes
	// no request id, which is the way that is asserted - there is nowhere for one
	// to be read from, rather than a promise that one is not.
	if got := Fingerprint(Consume, testTenant, "llm.tokens.output", 30); got != Fingerprint(Consume, testTenant, "llm.tokens.output", 30) {
		t.Errorf("the fingerprint is not a function of its four fields alone: %s", got)
	}
}

func TestTheWindowIsTwentyFourHoursAndTheScriptsSaySo(t *testing.T) {
	if IdempotencyWindow != 24*time.Hour {
		t.Fatalf("IdempotencyWindow is %s; DR-029 pins it to 24h and it is a customer-facing promise", IdempotencyWindow)
	}
	// The scripts hold their own copy of the number, because a script cannot
	// import a Go constant, and a second copy of a number that decides a
	// customer's guarantee is asserted rather than trusted - the same treatment
	// source_test.go gives the 2^53 bound.
	written := "local DAY = " + strconv.FormatInt(int64(IdempotencyWindow/time.Second), 10)
	for _, b := range bodies() {
		if !strings.Contains(b.text, written) {
			t.Errorf("%s.lua does not declare %q, so the window this package promises and the one a record gets are not the same number", b.op, written)
		}
	}
}
