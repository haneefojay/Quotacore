package cycle

import (
	"testing"
	"time"
)

// TestCycleIndexIsMonotonic is the engine half of DoD item 5 for IP-03: the
// computed cycle index is a non-decreasing function of the instant it is asked
// for. A caller that presents instants in ascending order never sees the index
// fall, which is the property the script's monotonic guard relies on.
func TestCycleIndexIsMonotonic(t *testing.T) {
	for _, interval := range []Interval{Hourly, Daily, Weekly, Monthly, Yearly} {
		for _, zone := range []string{"UTC", "Europe/Berlin", "America/New_York"} {
			loc := mustLocation(t, zone)
			t.Run(interval.String()+" in "+zone, func(t *testing.T) {
				anchor := mustParseRFC3339(t, "2026-01-01T00:00:00Z")
				var last int64 = -1
				start := mustParseRFC3339(t, "2026-01-01T00:00:00Z")
				for i := 0; i <= 400*4; i++ {
					now := start.Add(time.Duration(i) * 6 * time.Hour)
					idx := CurrentWindow(anchor, interval, loc, now).Index
					if idx < last {
						t.Fatalf("index fell from %d to %d between successive ascending instants", last, idx)
					}
					last = idx
				}
			})
		}
	}
}

// TestClockJumpBackwards is the host-clock-jump case: the engine is a pure
// function, so an instant presented after a backwards clock jump yields the
// index its own value demands — an older one than the instant that preceded
// it. That older index is exactly what the script's monotonic guard refuses as
// a stale write (C-3); the engine half is that the function is deterministic
// and consistent with the instant, never with the order it was asked.
func TestClockJumpBackwards(t *testing.T) {
	anchor := mustParseRFC3339(t, "2026-01-01T00:00:00Z")
	before := mustParseRFC3339(t, "2026-06-01T00:00:00Z")
	backwards := mustParseRFC3339(t, "2026-05-31T00:00:00Z") // a day earlier

	wBefore := CurrentWindow(anchor, Monthly, time.UTC, before)
	wBack := CurrentWindow(anchor, Monthly, time.UTC, backwards)

	if wBack.Index >= wBefore.Index {
		t.Fatalf("a backwards clock jump must present the older instant's index; got %d then %d",
			wBefore.Index, wBack.Index)
	}

	again := CurrentWindow(anchor, Monthly, time.UTC, backwards)
	if again.Index != wBack.Index || !again.Start.Equal(wBack.Start) {
		t.Fatalf("CurrentWindow is not a pure function: two calls with the same instant disagreed")
	}
}

// TestIdleAcrossBoundaries is the engine half of DoD item 6 for IP-03 and the
// engine side of T-06: a tenant idle across several boundaries is placed in the
// current window in one step, with no replay. 97 days on a monthly cadence
// crosses three boundaries, and the engine jumps straight to the fourth cycle
// rather than advancing one at a time (DR-009).
func TestIdleAcrossBoundaries(t *testing.T) {
	anchor := mustParseRFC3339(t, "2026-01-01T00:00:00Z")
	t0 := mustParseRFC3339(t, "2026-02-05T10:00:00Z")
	idle := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC) // t0 + 97 days

	w0 := CurrentWindow(anchor, Monthly, time.UTC, t0)
	w := CurrentWindow(anchor, Monthly, time.UTC, idle)

	if got, want := w0.Index, int64(1); got != want {
		t.Fatalf("index at t0 = %d, want %d (the February boundary)", got, want)
	}
	if got, want := w.Index-w0.Index, int64(3); got != want {
		t.Fatalf("index jump over 97 idle days = %d, want 3 (February, March, April boundaries)", got)
	}
	if !w.Start.Equal(Add(anchor, Monthly, time.UTC, w.Index)) {
		t.Fatalf("window start %v does not equal Add(anchor, monthly, index)", w.Start)
	}
	if got, want := w.Start.Format(time.RFC3339), "2026-05-01T00:00:00Z"; got != want {
		t.Fatalf("window start = %s, want %s", got, want)
	}
}
