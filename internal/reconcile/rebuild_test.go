package reconcile

import (
	"strings"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/cycle"
)

func hourly(now time.Time) Target {
	return Target{Subject: sub, Anchor: t0, Interval: cycle.Hourly, Location: time.UTC, Limit: 100, Now: now}
}

func cycleAt(t Target, index int64) Cycle {
	return Cycle{Start: t.window(index).Start, Index: index}
}

func class(t *testing.T, r Rebuilt, want string) Rebuilt {
	t.Helper()
	if r.Class != want {
		t.Fatalf("class %s (%s), want %s", r.Class, r.Reason, want)
	}
	return r
}

// TestTheRebuildReadsTheSixFieldsFromTheLastCycle: the balance is where the
// rows end, the limit is the opening moved by each relimit, the bonus is the
// grants since the opening, and the window is the cycle engine's.
func TestTheRebuildReadsTheSixFieldsFromTheLastCycle(t *testing.T) {
	tg := hourly(t0.Add(5*time.Hour + 10*time.Minute))
	c4, c5 := cycleAt(tg, 4), cycleAt(tg, 5)
	rows := history(100, c4, step{typ: "consumed", delta: -30},
		step{typ: "cycle_rolled_over", set: n(100), cycle: &c5}, step{typ: "granted", delta: 20},
		step{typ: "consumed", delta: -50}, step{typ: "plan_changed", delta: 50}, step{typ: "set", set: n(140)})
	tg.Limit = 150
	r := class(t, Classify(tg, Evidence{Rows: rows}), ClassExact)
	e := r.Estimate
	if e.Balance != 140 || e.Limit != 150 || e.Bonus != 20 || e.Window.Index != 5 || !e.Window.Start.Equal(c5.Start) ||
		e.Window.End == nil || !e.Window.End.Equal(tg.window(6).Start) {
		t.Fatalf("estimate %+v", *e)
	}
	// A force-rollover in the cycle reopens it, spending the bonus.
	rows = append(rows, history(140, c5, step{typ: "force_rolled_over", set: n(150)}, step{typ: "consumed", delta: -1})...)
	e = class(t, Classify(tg, Evidence{Rows: rows}), ClassExact).Estimate
	if e.Balance != 149 || e.Limit != 150 || e.Bonus != 0 {
		t.Fatalf("after a force-rollover, estimate %+v", *e)
	}
}

// TestTheRebuildKeepsAnUntouchedCycle: a hash nothing touched since its
// window closed is written in that window, at the balance it held, never at
// a fresh allowance; one that closed at the limit is the same hash as a
// later window at the limit (Q-22).
func TestTheRebuildKeepsAnUntouchedCycle(t *testing.T) {
	tg := hourly(t0.Add(9*time.Hour + time.Minute))
	c2 := cycleAt(tg, 2)
	rows := history(100, c2, step{typ: "consumed", delta: -30})
	e := class(t, Classify(tg, Evidence{Rows: rows}), ClassExact).Estimate
	if e.Balance != 70 || e.Window.Index != 2 || !e.Window.Start.Equal(c2.Start) {
		t.Fatalf("estimate %+v", *e)
	}
	rows = history(100, c2, step{typ: "consumed", delta: -30}, step{typ: "refunded", delta: 30})
	e = class(t, Classify(tg, Evidence{Rows: rows}), ClassExact).Estimate
	if e.Balance != 100 || e.Limit != 100 || e.Window.Index != 9 {
		t.Fatalf("at the limit, estimate %+v", *e)
	}
}

// TestALossOrARejectedEventIsNeverExact: a gap or a crashed session from the
// ledger's last cycle on makes it unverified, and a rejected dead letter or a
// row that does not chain makes it unreconcilable; the estimate is still
// offered for an operator to accept.
func TestALossOrARejectedEventIsNeverExact(t *testing.T) {
	tg := hourly(t0.Add(3*time.Hour + time.Minute))
	c3 := cycleAt(tg, 3)
	rows := history(100, c3, step{typ: "consumed", delta: -30}, step{typ: "consumed", delta: -5})
	gap := Loss{TenantID: sub.TenantID, Feature: sub.Feature, Reason: "queue_full", From: c3.Start.Add(time.Second),
		To: c3.Start.Add(2 * time.Second)}
	r := class(t, Classify(tg, Evidence{Rows: rows, Losses: []Loss{gap}}), ClassUnverified)
	if r.Estimate == nil || r.Estimate.Balance != 65 {
		t.Fatalf("an unverified hash offers no estimate: %+v", r.Estimate)
	}
	other := gap
	other.TenantID = "someone-else"
	class(t, Classify(tg, Evidence{Rows: rows, Losses: []Loss{other}}), ClassExact)
	crash := Loss{Reason: "crash", From: c3.Start.Add(-time.Hour), To: c3.Start.Add(time.Minute)}
	class(t, Classify(tg, Evidence{Rows: rows, Losses: []Loss{crash}}), ClassUnverified)
	earlier := Loss{Reason: "crash", From: t0, To: t0.Add(time.Minute)}
	class(t, Classify(tg, Evidence{Rows: rows, Losses: []Loss{earlier}}), ClassExact)

	class(t, Classify(tg, Evidence{Rows: rows, Rejected: 1}), ClassUnreconcilable)
	tampered := append([]Row(nil), rows...)
	tampered[0].After += 7
	r = class(t, Classify(tg, Evidence{Rows: tampered}), ClassUnreconcilable)
	if len(r.Findings) == 0 {
		t.Error("an unreconcilable hash names no finding")
	}
	// A relimit that floored the balance at zero hides the limit it set.
	floored := append(append([]Row(nil), rows...), history(65, c3, step{typ: "plan_changed", set: n(0)})...)
	class(t, Classify(tg, Evidence{Rows: floored}), ClassUnreconcilable)
}

// TestRetentionBoundsWhatIsExact: a cycle that opened before the retention
// horizon with no opening row may have lost rows to retention, which is
// every old `never` feature; one whose opening row survived is whole.
func TestRetentionBoundsWhatIsExact(t *testing.T) {
	tg := Target{Subject: sub, Anchor: t0, Interval: cycle.Never, Location: time.UTC, Limit: 100,
		Now: t0.Add(30 * 24 * time.Hour), Horizon: t0.Add(7 * 24 * time.Hour)}
	c := Cycle{Start: t0, Index: 0}
	rows := history(100, c, step{typ: "consumed", delta: -1})
	for i := range rows {
		rows[i].At = tg.Horizon.Add(time.Hour)
	}
	r := class(t, Classify(tg, Evidence{Rows: rows}), ClassUnreconcilable)
	if !strings.Contains(r.Reason, "retention") {
		t.Errorf("reason %q does not name retention", r.Reason)
	}
	class(t, Classify(tg, Evidence{}), ClassUnreconcilable)

	h := hourly(t0.Add(30 * time.Hour))
	h.Horizon = t0.Add(24 * time.Hour)
	c29 := cycleAt(h, 29)
	opened := history(40, c29, step{typ: "cycle_rolled_over", set: n(100)}, step{typ: "consumed", delta: -1})
	for i := range opened {
		opened[i].At = c29.Start.Add(time.Duration(i+1) * time.Second)
	}
	class(t, Classify(h, Evidence{Rows: opened}), ClassExact)
}

// TestAFeatureWithNoRowsOpensAtItsLimit: no row means the hash never moved,
// unless its configuration changed in the cycle or a loss could be its one
// event.
func TestAFeatureWithNoRowsOpensAtItsLimit(t *testing.T) {
	tg := hourly(t0.Add(2*time.Hour + time.Minute))
	e := class(t, Classify(tg, Evidence{}), ClassExact).Estimate
	if e.Balance != 100 || e.Limit != 100 || e.Bonus != 0 || e.Window.Index != 2 {
		t.Fatalf("estimate %+v", *e)
	}
	tg.ConfigChanged = tg.window(2).Start.Add(time.Second)
	class(t, Classify(tg, Evidence{}), ClassUnverified)
	tg.ConfigChanged = time.Time{}
	class(t, Classify(tg, Evidence{Losses: []Loss{{Reason: "crash", From: t0, To: t0.Add(time.Second)}}}), ClassUnverified)
}

// TestAPresentHashIsVerifiedNotRewritten.
func TestAPresentHashIsVerifiedNotRewritten(t *testing.T) {
	tg := hourly(t0.Add(time.Minute))
	c0 := cycleAt(tg, 0)
	rows := history(100, c0, step{typ: "consumed", delta: -1})
	r := class(t, Classify(tg, Evidence{Rows: rows, Hash: Hash{Present: true, Balance: 98, Limit: 100, Cycle: c0}}), ClassPresent)
	if r.Estimate != nil || len(r.Findings) != 1 || r.Findings[0].Kind != KindHashBalance {
		t.Fatalf("estimate %v, findings %v", r.Estimate, r.Findings)
	}
}
