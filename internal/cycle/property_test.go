package cycle

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

// TestProjectedWindowEqualsComputedWindow is DoD item 4 for IP-03 and the
// engine half of the property test in testing-strategy.md §3: the window a
// caller projects from the anchor and the current index is exactly the window
// the engine computes, so the transition's view and the pure function never
// disagree. Deterministic: a failed run prints its seed and the failing case,
// which is the minimal reproduction because each case is self-contained
// (testing-strategy.md §3: shrinking is mandatory, seed is logged).
func TestProjectedWindowEqualsComputedWindow(t *testing.T) {
	seed := uint64(0x51A7E) // stable across runs; a failure prints the seed
	r := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))

	zones := []string{"UTC", "Europe/Berlin", "America/New_York", "Asia/Kolkata", "Australia/Lord_Howe"}
	intervals := []Interval{Hourly, Daily, Weekly, Monthly, Yearly, Never}

	// Deterministic probes guaranteed to sit on or beside 2026 DST transitions,
	// so the RNG cannot starve the suite of DST coverage.
	fixed := []struct{ zone, interval, anchor, now string }{
		{"Europe/Berlin", "daily", "2026-01-01T00:00:00+01:00", "2026-03-29T12:00:00+02:00"},
		{"Europe/Berlin", "daily", "2026-01-01T00:00:00+01:00", "2026-10-25T12:00:00+01:00"},
		{"America/New_York", "daily", "2026-01-01T00:00:00-05:00", "2026-03-08T12:00:00-04:00"},
		{"America/New_York", "daily", "2026-01-01T00:00:00-05:00", "2026-11-01T12:00:00-05:00"},
		{"Australia/Lord_Howe", "daily", "2026-01-01T00:00:00+11:00", "2026-04-05T12:00:00+10:30"},
		{"Australia/Lord_Howe", "daily", "2026-01-01T00:00:00+11:00", "2026-10-04T12:00:00+11:00"},
	}
	for _, f := range fixed {
		checkCaseProperty(t, f.zone, f.interval, f.anchor, f.now, 0)
	}

	const draws = 4000
	for i := 0; i < draws; i++ {
		zone := zones[r.IntN(len(zones))]
		iv := intervals[r.IntN(len(intervals))]
		loc := mustLocation(t, zone)
		anchor := randomZoneTime(r, loc, 2020, 2035)
		now := randomZoneTime(r, loc, 2019, 2037)
		ok := checkCaseProperty(t, zone, iv.String(), anchor.Format(time.RFC3339), now.Format(time.RFC3339), i)
		if !ok {
			t.Fatalf("property failed after %d cases (seed %#x); the case above is the minimal reproduction", i, seed)
		}
	}
}

// checkCaseProperty asserts the window invariants for one (zone, interval,
// anchor, now) draw. It reports through t and returns false on the first
// violation.
func checkCaseProperty(t *testing.T, zone, interval, anchorStr, nowStr string, index int) bool {
	t.Helper()
	loc := mustLocation(t, zone)
	anchor := mustParseRFC3339(t, anchorStr)
	now := mustParseRFC3339(t, nowStr)
	iv := intervalByName(interval)

	w := CurrentWindow(anchor, iv, loc, now)

	label := fmt.Sprintf("case %d (zone %s, interval %s, anchor %s, now %s)", index, zone, interval, anchorStr, nowStr)

	if iv == Never {
		if w.Index != 0 || w.End != nil || !w.Start.Equal(anchor) {
			t.Errorf("%s: never window = index %d, start %v, end %v; want 0, anchor, nil", label, w.Index, w.Start, w.End)
			return false
		}
		return true
	}

	if w.Index < 0 {
		t.Errorf("%s: negative index %d", label, w.Index)
		return false
	}

	// The window the caller projects from the anchor and the index.
	start := Add(anchor, iv, loc, w.Index)
	next := Add(anchor, iv, loc, w.Index+1)
	if !w.Start.Equal(start) {
		t.Errorf("%s: computed start %v != projected Add(n) %v", label, w.Start, start)
		return false
	}
	if w.End == nil || !w.End.Equal(next) {
		t.Errorf("%s: computed end %v != projected Add(n+1) %v", label, valueOrZero(w.End), next)
		return false
	}

	// The window actually contains the instant it was asked about.
	if w.Start.After(now) {
		if w.Index != 0 {
			t.Errorf("%s: window starts after now but index is %d, not 0", label, w.Index)
			return false
		}
	} else if !now.Before(*w.End) {
		t.Errorf("%s: now %v not inside [%v, %v)", label, now, w.Start, *w.End)
		return false
	}

	// Projecting from the current index to the next boundary and asking again
	// returns index n+1 and the same boundary: the caller's forward projection
	// and the engine's answer agree at the boundary itself (DR-009).
	atBoundary := CurrentWindow(anchor, iv, loc, next)
	if atBoundary.Index != w.Index+1 || !atBoundary.Start.Equal(next) {
		t.Errorf("%s: at next boundary, index = %d start %v; want %d and %v",
			label, atBoundary.Index, atBoundary.Start, w.Index+1, next)
		return false
	}

	return true
}

func valueOrZero(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

func intervalByName(s string) Interval {
	for _, iv := range []Interval{Hourly, Daily, Weekly, Monthly, Yearly, Never} {
		if iv.String() == s {
			return iv
		}
	}
	return Daily
}

// randomZoneTime builds a wall-clock calendar time in loc, which is how anchors
// and instants exist in the product: as tenant-local times, not derivations.
func randomZoneTime(r *rand.Rand, loc *time.Location, yearLo, yearHi int) time.Time {
	return time.Date(
		yearLo+r.IntN(yearHi-yearLo+1),
		time.Month(1+r.IntN(12)),
		1+r.IntN(28),
		r.IntN(24),
		r.IntN(60),
		r.IntN(60),
		0,
		loc,
	)
}
