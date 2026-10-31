package script

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// T-17's script half, IP-18 DoD 2, 3 and 4, against the real store: a grant's
// bonus belongs to one cycle, a refused grant or set changes nothing, and
// neither moves the window (DR-019, DR-020, INV-C5).

// TestT17AGrantNeverAccumulates grants in three consecutive cycles, each
// window named rather than waited for. Each grant's transition rolls the
// cycle that ended, so the bonus it adds to starts at zero: after each the
// bonus is that cycle's grant alone and the balance the new limit plus it.
// A second grant in the same cycle adds to the first, because the bonus is
// the total granted in the current cycle (DR-019).
func TestT17AGrantNeverAccumulates(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	feature := featureKey(t)
	key := keyFor(t, feature)
	base := time.Now().UTC().Truncate(time.Hour).Add(-72 * time.Hour)
	seed(t, s, key, state{Balance: 100, Limit: 100, Index: 0, Start: base, End: ends(base.Add(time.Hour))})

	grant := func(index, amount int64) AdminOutcome {
		t.Helper()
		start := base.Add(time.Duration(index) * time.Hour)
		out, err := runner.Grant(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: amount,
			Window: fixedWindow(index, start, time.Hour), Limit: 100, At: start.Add(30 * time.Minute)})
		if err != nil || out.Decision != Applied {
			t.Fatalf("grant %d in cycle %d = %+v, %v", amount, index, out, err)
		}
		return out
	}
	for i, amount := range []int64{30, 40, 50} {
		index := int64(i + 1)
		out := grant(index, amount)
		if out.Transition != Rolled || out.Index != index {
			t.Errorf("cycle %d: transition %q at index %d, want rolled to %d", index, out.Transition, out.Index, index)
		}
		h := fields(t, s, key)
		if out.Bonus != amount || h["bonus"] != fmt.Sprint(amount) || out.Balance != 100+amount || h["balance"] != fmt.Sprint(100+amount) {
			t.Errorf("cycle %d after a grant of %d: reply bonus %d balance %d, hash %v; the bonus accumulated across cycles (DR-020, INV-C5)",
				index, amount, out.Bonus, out.Balance, h)
		}
		if out.Before == nil || *out.Before != 100 {
			t.Errorf("cycle %d: the balance before the grant is %v, want the new cycle's 100", index, out.Before)
		}
	}
	if out := grant(3, 5); out.Transition != Current || out.Bonus != 55 || out.Balance != 155 {
		t.Errorf("a second grant in the same cycle = %+v, want bonus 55 and balance 155", out)
	}
}

// TestT17ARefusedCorrectionChangesNothingInTheStore is IP-18 DoD 3 in the
// script: a set above limit plus bonus and a grant past 2^53 - 1 are refused
// inside the script that would apply them, so the refusal and the check are
// one instant, and the hash is byte-identical after.
func TestT17ARefusedCorrectionChangesNothingInTheStore(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	feature := featureKey(t)
	key := keyFor(t, feature)
	at := time.Now().UTC().Truncate(time.Second)
	w := fixedWindow(4, at.Add(-time.Hour), 24*time.Hour)
	seed(t, s, key, state{Balance: 60, Limit: 100, Bonus: 5, Index: 4, Start: w.Start, End: w.End})
	before := fields(t, s, key)
	req := func(amount int64) Request {
		return Request{TenantID: testTenant, FeatureKey: feature, Amount: amount, Window: w, Limit: 100, At: at}
	}

	if out, err := runner.Set(ctx, req(106)); err != nil || out.Decision != RefusedCeiling {
		t.Errorf("a set of 106 over a ceiling of 105 = %+v, %v; want refused_ceiling", out, err)
	}
	if out, err := runner.Grant(ctx, req(9007199254740991)); err != nil || out.Decision != RefusedCeiling {
		t.Errorf("a grant past 2^53 - 1 = %+v, %v; want refused_ceiling", out, err)
	}
	assertUnchanged(t, s, key, before, "after a refused set and a refused grant")
	if out, err := runner.Set(ctx, req(105)); err != nil || out.Decision != Applied || out.Balance != 105 {
		t.Errorf("a set at the ceiling exactly = %+v, %v; want applied", out, err)
	}
}

// TestT17NeitherMovesTheWindow is IP-18 DoD 4: a grant and a set in the
// current cycle leave the cycle index and both window fields byte-identical,
// and change only the balance, and for a grant the bonus.
func TestT17NeitherMovesTheWindow(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	feature := featureKey(t)
	key := keyFor(t, feature)
	at := time.Now().UTC().Truncate(time.Second)
	w := fixedWindow(9, at.Add(-time.Hour), 24*time.Hour)
	seed(t, s, key, state{Balance: 20, Limit: 100, Index: 9, Start: w.Start, End: w.End})
	before := fields(t, s, key)
	req := func(amount int64) Request {
		return Request{TenantID: testTenant, FeatureKey: feature, Amount: amount, Window: w, Limit: 100, At: at}
	}
	for _, op := range []struct {
		name string
		run  func(context.Context, Request) (AdminOutcome, error)
		arg  int64
	}{{"grant", runner.Grant, 25}, {"set", runner.Set, 70}} {
		out, err := op.run(ctx, req(op.arg))
		if err != nil || out.Decision != Applied || out.Transition != Current {
			t.Fatalf("%s = %+v, %v", op.name, out, err)
		}
		after := fields(t, s, key)
		for _, name := range []string{"cycle_index", "window_start", "window_end", "limit"} {
			if after[name] != before[name] {
				t.Errorf("after a %s, %s = %q, want %q", op.name, name, after[name], before[name])
			}
		}
	}
	if h := fields(t, s, key); h["balance"] != "70" || h["bonus"] != "25" {
		t.Errorf("the hash holds %v, want balance 70 and bonus 25", h)
	}
}
