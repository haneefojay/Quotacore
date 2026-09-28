package cycle

import (
	"testing"
	"time"
)

// TestDaylightSavingPair is DoD item 2 for IP-03: a spring-forward day has a
// 23-hour cycle and an autumn day a 25-hour one, and neither transition
// skips a boundary (DR-007). The boundaries on both sides of the transition
// still exist at the wall-clock hour their zone dictates.
func TestDaylightSavingPair(t *testing.T) {
	berlin := mustLocation(t, "Europe/Berlin")

	t.Run("spring forward 2026-03-29: the day is 23 hours", func(t *testing.T) {
		anchor := mustParseRFC3339(t, "2026-01-01T00:00:00+01:00")
		now := mustParseRFC3339(t, "2026-03-29T12:00:00+02:00")
		w := CurrentWindow(anchor, Daily, berlin, now)

		if got, want := w.Start.Format(time.RFC3339), "2026-03-29T00:00:00+01:00"; got != want {
			t.Fatalf("cycle_start = %s, want %s", got, want)
		}
		if got, want := w.End.Format(time.RFC3339), "2026-03-30T00:00:00+02:00"; got != want {
			t.Fatalf("cycle_end = %s, want %s", got, want)
		}
		if got := w.End.Sub(w.Start); got != 23*time.Hour {
			t.Fatalf("window is %s, want 23h0m0s", got)
		}
	})

	t.Run("autumn fall back 2026-10-25: the day is 25 hours", func(t *testing.T) {
		anchor := mustParseRFC3339(t, "2026-01-01T00:00:00+01:00")
		now := mustParseRFC3339(t, "2026-10-25T12:00:00+01:00")
		w := CurrentWindow(anchor, Daily, berlin, now)

		if got, want := w.Start.Format(time.RFC3339), "2026-10-25T00:00:00+02:00"; got != want {
			t.Fatalf("cycle_start = %s, want %s", got, want)
		}
		if got, want := w.End.Format(time.RFC3339), "2026-10-26T00:00:00+01:00"; got != want {
			t.Fatalf("cycle_end = %s, want %s", got, want)
		}
		if got := w.End.Sub(w.Start); got != 25*time.Hour {
			t.Fatalf("window is %s, want 25h0m0s", got)
		}
	})

	// Neither transition skips a boundary: every daily boundary that flanks the
	// transition exists, and only the one cycle that contains it is shortened
	// (spring) or lengthened (autumn). A naive implementation that adds 24
	// hours to a localised instant produces a wrong boundary on one of these
	// four days, and it shows here rather than in any other row.
	t.Run("spring: consecutive windows are 24, 23, 24, 24 hours", func(t *testing.T) {
		anchor := mustParseRFC3339(t, "2026-01-01T00:00:00+01:00")
		want := [...]struct {
			day   string
			start string
			span  time.Duration
		}{
			{"2026-03-28T12:00:00+01:00", "2026-03-28T00:00:00+01:00", 24 * time.Hour},
			{"2026-03-29T12:00:00+02:00", "2026-03-29T00:00:00+01:00", 23 * time.Hour},
			{"2026-03-30T12:00:00+02:00", "2026-03-30T00:00:00+02:00", 24 * time.Hour},
			{"2026-03-31T12:00:00+02:00", "2026-03-31T00:00:00+02:00", 24 * time.Hour},
		}
		for _, wc := range want {
			now := mustParseRFC3339(t, wc.day)
			w := CurrentWindow(anchor, Daily, berlin, now)
			if got := w.Start.Format(time.RFC3339); got != wc.start {
				t.Errorf("now %s: start = %s, want %s", wc.day, got, wc.start)
			}
			if got := w.End.Sub(w.Start); got != wc.span {
				t.Errorf("now %s: span = %s, want %s", wc.day, got, wc.span)
			}
		}
	})

	t.Run("autumn: consecutive windows are 24, 25, 24, 24 hours", func(t *testing.T) {
		anchor := mustParseRFC3339(t, "2026-01-01T00:00:00+01:00")
		want := [...]struct {
			day   string
			start string
			span  time.Duration
		}{
			{"2026-10-24T12:00:00+02:00", "2026-10-24T00:00:00+02:00", 24 * time.Hour},
			{"2026-10-25T12:00:00+01:00", "2026-10-25T00:00:00+02:00", 25 * time.Hour},
			{"2026-10-26T12:00:00+01:00", "2026-10-26T00:00:00+01:00", 24 * time.Hour},
			{"2026-10-27T12:00:00+01:00", "2026-10-27T00:00:00+01:00", 24 * time.Hour},
		}
		for _, wc := range want {
			now := mustParseRFC3339(t, wc.day)
			w := CurrentWindow(anchor, Daily, berlin, now)
			if got := w.Start.Format(time.RFC3339); got != wc.start {
				t.Errorf("now %s: start = %s, want %s", wc.day, got, wc.start)
			}
			if got := w.End.Sub(w.Start); got != wc.span {
				t.Errorf("now %s: span = %s, want %s", wc.day, got, wc.span)
			}
		}
	})
}
