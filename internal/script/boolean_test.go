package script

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/quotacore/quotacore/internal/cycle"
)

// The two scripts a boolean entitlement runs (DR-050, IP-17). The checks here
// are the boolean half of source_test.go's and runner_test.go's, plus the
// latency comparison IP-17 DoD 4 asks for.

func booleanBodies() []body { return []body{consumeBooleanBody, checkBooleanBody} }

// TestABooleanCallIsOneCall is IP-17 DoD 4's count, taken in the client as
// TestOneConsumeIsOneCall takes the metered one: a boolean consume and a
// boolean check each make exactly one evaluation, by digest.
func TestABooleanCallIsOneCall(t *testing.T) {
	for _, c := range []struct {
		name string
		run  func(*Runner) (Decision, error)
	}{
		{"consume", func(r *Runner) (Decision, error) {
			return r.ConsumeBoolean(context.Background(), Request{TenantID: testTenant, FeatureKey: "sso", Amount: 1,
				IdempotencyKey: "a-client-key"})
		}},
		{"check", func(r *Runner) (Decision, error) {
			return r.CheckBoolean(context.Background(), Request{TenantID: testTenant, FeatureKey: "sso", Amount: 1})
		}},
	} {
		fake := &fakeStore{reply: []any{"applied"}}
		d, err := c.run(newRunner(t, fake, 250*time.Millisecond))
		if err != nil || d != Applied {
			t.Fatalf("%s: %v, %v", c.name, d, err)
		}
		evals, loads, bodies := fake.counts()
		if evals != 1 || loads != 0 || bodies != 0 {
			t.Errorf("a boolean %s made %d evaluations, %d loads and %d bodies; the rule is one round trip by digest",
				c.name, evals, loads, bodies)
		}
	}
}

// TestABooleanConsumeRefusesACount: an amount above 1 never reaches the store,
// and a consume without a key is refused before it does either.
func TestABooleanConsumeRefusesACount(t *testing.T) {
	fake := &fakeStore{reply: []any{"applied"}}
	r := newRunner(t, fake, 250*time.Millisecond)
	if _, err := r.ConsumeBoolean(context.Background(), Request{TenantID: testTenant, FeatureKey: "sso", Amount: 2,
		IdempotencyKey: "k"}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("amount 2: %v, want ErrInvalidArgument", err)
	}
	if _, err := r.ConsumeBoolean(context.Background(), Request{TenantID: testTenant, FeatureKey: "sso", Amount: 1}); !errors.Is(err, ErrMissingIdempotencyKey) {
		t.Errorf("no key: %v, want ErrMissingIdempotencyKey", err)
	}
	if evals, _, _ := fake.counts(); evals != 0 {
		t.Errorf("a refused boolean consume reached the store %d times", evals)
	}
}

// TestTheBooleanConsumeSharesTheIdempotencyBlock: consume_boolean.lua holds
// consume.lua's idempotency block byte for byte, so a record one writes the
// other reads back, and check_boolean.lua has none (DR-026, DR-027).
func TestTheBooleanConsumeSharesTheIdempotencyBlock(t *testing.T) {
	block := func(text string) string {
		start, end := strings.Index(text, idemOpen), strings.Index(text, idemClose)
		if start < 0 || end < start {
			return ""
		}
		return text[start : end+len(idemClose)]
	}
	want := block(consumeBody.text)
	if got := block(consumeBooleanBody.text); got == "" || got != want {
		t.Error("consume_boolean.lua's idempotency block differs from consume.lua's (DR-027, DR-028)")
	}
	if strings.Contains(checkBooleanBody.text, idemOpen) || strings.Contains(checkBooleanBody.text, "KEYS[2]") {
		t.Error("check_boolean.lua has an idempotency record; a check changes nothing (DR-026)")
	}
	if strings.Index(consumeBooleanBody.text, "redis.call('GET', KEYS[2])") > strings.LastIndex(consumeBooleanBody.text, "redis.call('SET'") {
		t.Error("consume_boolean.lua writes its record before it looks one up")
	}
}

// TestTheBooleanScriptsAreBounded is source_test.go's shape rules for the two
// boolean scripts: flagged, literal commands from the allowed set, no loop and
// no load, and no read of KEYS[1], which names the slot and is not a balance.
func TestTheBooleanScriptsAreBounded(t *testing.T) {
	allowed := map[string]bool{"GET": true, "SET": true}
	banned := []string{"for ", "while ", "repeat", "pairs(", "ipairs(", "select(",
		"math.random", "os.", "dofile", "loadstring", "require"}
	for _, b := range booleanBodies() {
		if !strings.HasPrefix(b.text, "#!lua\n") {
			t.Errorf("%s.lua is not flagged, so a full store would not refuse it whole (DR-048)", b.op)
		}
		code := stripLuaComments(b.text)
		for _, word := range banned {
			if strings.Contains(code, word) {
				t.Errorf("%s.lua contains %q", b.op, word)
			}
		}
		if strings.Contains(code, "KEYS[1]") {
			t.Errorf("%s.lua reads KEYS[1]; a boolean has no balance hash", b.op)
		}
		for _, m := range strings.Split(code, "redis.call('")[1:] {
			cmd, _, _ := strings.Cut(m, "'")
			if !allowed[cmd] {
				t.Errorf("%s.lua calls %s", b.op, cmd)
			}
		}
	}
	seen := map[Operation]bool{}
	for _, b := range allBodies() {
		seen[b.op] = true
	}
	if !seen[ConsumeBoolean] || !seen[CheckBoolean] {
		t.Error("Load does not put the boolean scripts in the store")
	}
}

// TestBooleanScriptsAgainstTheStore runs the two scripts on the real store:
// applied, replayed, reused and a refused amount, and no balance hash written.
func TestBooleanScriptsAgainstTheStore(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	feature := featureKey(t)
	key := "bk-" + uuid.NewString()
	req := Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, IdempotencyKey: key}
	if d, err := runner.ConsumeBoolean(ctx, req); err != nil || d != Applied {
		t.Fatalf("first: %v, %v", d, err)
	}
	if d, err := runner.ConsumeBoolean(ctx, req); err != nil || d != Replayed {
		t.Errorf("retry: %v, %v, want replayed", d, err)
	}
	other := req
	other.Amount = 0
	if _, err := runner.ConsumeBoolean(ctx, other); !errors.Is(err, ErrIdempotencyKeyReuse) {
		t.Errorf("same key, amount 0: %v, want reuse", err)
	}
	if d, err := runner.CheckBoolean(ctx, Request{TenantID: testTenant, FeatureKey: feature}); err != nil || d != Applied {
		t.Errorf("check: %v, %v", d, err)
	}
	if n, err := s.Client().Exists(ctx, keyFor(t, feature)).Result(); err != nil || n != 0 {
		t.Errorf("a boolean call wrote a balance hash: %d, %v", n, err)
	}
	// A record written by the metered consume for the same feature and amount
	// reads as the same operation, so the two share one fingerprint space; a
	// feature never changes kind, so that pair cannot both exist.
	if _, err := runner.ConsumeBoolean(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 1,
		IdempotencyKey: "bk-" + uuid.NewString()}); err != nil {
		t.Errorf("a fresh key: %v", err)
	}
}

// TestBooleanCostsNoMoreThanMetered is IP-17 DoD 4's benchmark half: against
// the real store, a boolean check's p99 is at or below a metered check's, and
// a boolean consume's at or below a metered consume's. The calls are
// interleaved so a slow stretch of the store falls on both, and the comparison
// is repeated up to three times before it fails, because a p99 over a
// thousand calls on a shared machine is a measurement and not a constant.
func TestBooleanCostsNoMoreThanMetered(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	const n = 1000
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	w := fixedWindow(0, base, 24*time.Hour)
	feature := featureKey(t)
	measure := func() (checkB, checkM, consumeB, consumeM time.Duration) {
		seed(t, s, keyFor(t, feature), state{Balance: bound, Limit: bound, Index: 0, Start: w.Start, End: w.End})
		var cb, cm, ub, um []time.Duration
		timed := func(into *[]time.Duration, f func() error) {
			start := time.Now()
			if err := f(); err != nil {
				t.Fatal(err)
			}
			*into = append(*into, time.Since(start))
		}
		metered := Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, Limit: bound, Window: w,
			Interval: cycle.Daily, At: w.Start.Add(time.Minute)}
		for i := range n {
			k := "bc-" + strconv.Itoa(i) + "-" + uuid.NewString()
			timed(&cb, func() error { _, err := runner.CheckBoolean(ctx, metered); return err })
			timed(&cm, func() error { _, err := runner.Check(ctx, metered); return err })
			timed(&ub, func() error {
				r := metered
				r.IdempotencyKey = k + "-b"
				_, err := runner.ConsumeBoolean(ctx, r)
				return err
			})
			timed(&um, func() error {
				r := metered
				r.IdempotencyKey = k + "-m"
				_, err := runner.Consume(ctx, r)
				return err
			})
		}
		p99 := func(d []time.Duration) time.Duration { slices.Sort(d); return d[n*99/100] }
		return p99(cb), p99(cm), p99(ub), p99(um)
	}
	for attempt := 1; ; attempt++ {
		cb, cm, ub, um := measure()
		t.Logf("attempt %d: check p99 boolean %v, metered %v; consume p99 boolean %v, metered %v", attempt, cb, cm, ub, um)
		if cb <= cm && ub <= um {
			return
		}
		if attempt == 3 {
			t.Errorf("a boolean call's p99 is above the metered one's in three measurements (IP-17 DoD 4)")
			return
		}
	}
}
