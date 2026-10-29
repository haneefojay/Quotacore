package reconcile

import (
	"math/rand"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// rows builds a history from a list of (type, delta) steps, starting at a
// balance, in one cycle unless a step names another.
type step struct {
	typ   string
	delta int64
	cycle *Cycle
	set   *int64 // an opening sets the balance rather than adding to it
}

func history(start int64, c Cycle, steps ...step) []Row {
	cur := start
	var out []Row
	at := t0
	for i, s := range steps {
		cc := c
		if s.cycle != nil {
			cc = *s.cycle
			c = cc
		}
		d := s.delta
		if s.set != nil {
			d = *s.set - cur
		}
		cur += d
		at = at.Add(time.Second)
		st := cc.Start
		out = append(out, Row{ID: string(rune('a' + i)), Seq: int64(i + 1), Type: s.typ, Delta: d, After: cur,
			CycleIndex: cc.Index, CycleStart: &st, At: at})
	}
	return out
}

func kinds(v Verified) []string {
	var out []string
	for _, f := range v.Findings {
		out = append(out, f.Kind)
	}
	return out
}

func one(t *testing.T, v Verified, kind string) Finding {
	t.Helper()
	if len(v.Findings) != 1 || v.Findings[0].Kind != kind {
		t.Fatalf("findings %v, want one %s", kinds(v), kind)
	}
	return v.Findings[0]
}

var sub = Subject{TenantID: "00000000-0000-0000-0000-000000000001", Feature: "f"}

func n(v int64) *int64 { return &v }

// TestTheIdentityHoldsInAnyOrderTheRowsChain: the rows of a cycle reconcile
// whatever order two instances' clocks wrote them in, as long as they chain
// by balance; a row that chains nowhere is found, under the identity kind.
func TestTheIdentityHoldsInAnyOrderTheRowsChain(t *testing.T) {
	c := Cycle{Start: t0, Index: 3}
	rows := history(100, c, step{typ: "consumed", delta: -10}, step{typ: "consumed", delta: -20},
		step{typ: "granted", delta: 5}, step{typ: "refunded", delta: 7}, step{typ: "consumed", delta: -1})
	hash := &Hash{Present: true, Balance: 81, Limit: 100, Cycle: c}
	if v := Verify(sub, rows, nil, hash); len(v.Findings) != 0 || v.End != 81 {
		t.Fatalf("in order: %v, end %d", kinds(v), v.End)
	}
	rng := rand.New(rand.NewSource(1))
	for range 50 {
		shuffled := append([]Row(nil), rows...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		prev := &Row{ID: "p", After: 100, CycleIndex: 3, CycleStart: &t0}
		if v := Verify(sub, shuffled, prev, hash); len(v.Findings) != 0 || v.End != 81 {
			t.Fatalf("shuffled %v: %v, end %d", ids(shuffled), kinds(v), v.End)
		}
	}
	// One row dropped from the middle: nothing chains past the gap.
	dropped := append(append([]Row(nil), rows[:2]...), rows[3:]...)
	f := one(t, Verify(sub, dropped, nil, nil), KindIdentity)
	if f.Row != rows[3].ID || *f.Expected != 70 || *f.Actual != 75 {
		t.Errorf("the identity finding names %s expecting %d from %d", f.Row, *f.Expected, *f.Actual)
	}
	// A balance_after altered in place.
	altered := append([]Row(nil), rows...)
	altered[1].After += 3
	if v := Verify(sub, altered, nil, nil); len(v.Findings) == 0 {
		t.Error("an altered balance_after reconciles")
	}
}

func ids(rows []Row) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// TestAForceRolloverReopensItsCycle: a force-rollover refills the window it is
// in, so its row is in the same cycle and its balance_after is the opening the
// rest of the cycle counts from.
func TestAForceRolloverReopensItsCycle(t *testing.T) {
	c := Cycle{Start: t0, Index: 5}
	rows := history(100, c, step{typ: "consumed", delta: -60}, step{typ: "force_rolled_over", set: n(100)},
		step{typ: "consumed", delta: -30})
	if v := Verify(sub, rows, nil, &Hash{Present: true, Balance: 70, Limit: 100, Cycle: c}); len(v.Findings) != 0 {
		t.Fatalf("findings %v", kinds(v))
	}
	// A force-rollover that lowered a stale index opens a new cycle at the
	// lower index, which is still a later cycle in the ledger's order.
	lower := Cycle{Start: t0.Add(time.Hour), Index: 2}
	rows = history(100, c, step{typ: "consumed", delta: -60},
		step{typ: "force_rolled_over", set: n(100), cycle: &lower}, step{typ: "consumed", delta: -1})
	if v := Verify(sub, rows, nil, &Hash{Present: true, Balance: 99, Limit: 100, Cycle: lower}); len(v.Findings) != 0 {
		t.Fatalf("lowered: findings %v", kinds(v))
	}
}

// TestAReanchorStartsACycleAtIndexZero: a re-anchor's plan_changed row opens
// a cycle at index 0 in a new window, which the verifier counts as a new cycle
// by its window, not as a step back in the index.
func TestAReanchorStartsACycleAtIndexZero(t *testing.T) {
	old := Cycle{Start: t0, Index: 7}
	next := Cycle{Start: t0.Add(36 * time.Hour), Index: 0}
	rows := history(500, old, step{typ: "consumed", delta: -100},
		step{typ: "plan_changed", set: n(800), cycle: &next}, step{typ: "consumed", delta: -10})
	if v := Verify(sub, rows, nil, &Hash{Present: true, Balance: 790, Limit: 800, Cycle: next}); len(v.Findings) != 0 {
		t.Fatalf("findings %v", kinds(v))
	}
	// The hash left behind in the old cycle after a re-anchor is a cycle the
	// ledger cannot have led to.
	one(t, Verify(sub, rows, nil, &Hash{Present: true, Balance: 400, Limit: 500, Cycle: old}), KindHashCycle)
}

// TestACycleWithNoOpeningRowOpensAtTheLimit: a rollover that closed a balance
// already equal to the new limit wrote no row (Q-22), so the hash may sit in a
// later cycle than the ledger's last row; that is consistent only when the
// hash is untouched at its limit and the ledger ended at that limit.
func TestACycleWithNoOpeningRowOpensAtTheLimit(t *testing.T) {
	c := Cycle{Start: t0, Index: 1}
	later := Cycle{Start: t0.Add(time.Hour), Index: 2}
	rows := history(100, c, step{typ: "consumed", delta: -10}, step{typ: "refunded", delta: 10})
	if v := Verify(sub, rows, nil, &Hash{Present: true, Balance: 100, Limit: 100, Cycle: later}); len(v.Findings) != 0 {
		t.Fatalf("findings %v", kinds(v))
	}
	// The hash moved in the later cycle with no row: a dropped event at the
	// end of the open cycle.
	one(t, Verify(sub, rows, nil, &Hash{Present: true, Balance: 90, Limit: 100, Cycle: later}), KindHashCycle)
	// The ledger ended below the limit, so the opening had a delta, and its
	// row is missing.
	short := rows[:1]
	one(t, Verify(sub, short, nil, &Hash{Present: true, Balance: 100, Limit: 100, Cycle: later}), KindHashCycle)
	// In the same cycle, the hash is the last balance_after.
	f := one(t, Verify(sub, rows, nil, &Hash{Present: true, Balance: 95, Limit: 100, Cycle: c}), KindHashBalance)
	if *f.Expected != 100 || *f.Actual != 95 {
		t.Errorf("hash_balance expects %d, found %d", *f.Expected, *f.Actual)
	}
	// No hash at all is reported, under its own kind, and is not a mismatch.
	if k := one(t, Verify(sub, rows, nil, &Hash{}), KindHashMissing).Kind; Counted(k) {
		t.Error("a missing hash counts as a mismatch")
	}
}

// TestAClosedCycleChainsToTheNextOpening: an event dropped at the end of a
// closed cycle leaves that cycle self-consistent; the next cycle's opening
// row starts from the balance the closed cycle really ended at, which is how
// the drop is found, under the chain kind.
func TestAClosedCycleChainsToTheNextOpening(t *testing.T) {
	c := Cycle{Start: t0, Index: 1}
	next := Cycle{Start: t0.Add(time.Hour), Index: 2}
	rows := history(100, c, step{typ: "consumed", delta: -10}, step{typ: "consumed", delta: -15},
		step{typ: "cycle_rolled_over", set: n(100), cycle: &next}, step{typ: "consumed", delta: -1})
	hash := &Hash{Present: true, Balance: 99, Limit: 100, Cycle: next}
	if v := Verify(sub, rows, nil, hash); len(v.Findings) != 0 {
		t.Fatalf("findings %v", kinds(v))
	}
	dropped := append([]Row{rows[0]}, rows[2:]...)
	f := one(t, Verify(sub, dropped, nil, hash), KindChain)
	if f.Row != rows[2].ID || *f.Expected != 90 || *f.Actual != 75 {
		t.Errorf("the chain finding names %s expecting %d from %d", f.Row, *f.Expected, *f.Actual)
	}
	// The same drop seen by a pass that reads only the new cycle, with the
	// closed cycle's last row as the row before.
	f = one(t, Verify(sub, dropped[1:], &dropped[0], hash), KindChain)
	if f.Row != rows[2].ID {
		t.Errorf("from a prefix, the chain finding names %s", f.Row)
	}
	// A fingerprint is stable across reads and distinct across kinds.
	if f.Fingerprint() != one(t, Verify(sub, dropped, nil, hash), KindChain).Fingerprint() {
		t.Error("the same finding has two fingerprints")
	}
}

// TestALastRowWithNoWindowIsNamed: a row written before migration 00010 has
// no cycle_start, so the hash cannot be compared with it.
func TestALastRowWithNoWindowIsNamed(t *testing.T) {
	rows := []Row{{ID: "a", Type: "consumed", Delta: -1, After: 9}}
	one(t, Verify(sub, rows, nil, &Hash{Present: true, Balance: 9, Limit: 10, Cycle: Cycle{Start: t0}}), KindNoCycleStart)
}
