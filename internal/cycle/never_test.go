package cycle

import (
	"testing"
	"time"
)

// TestNeverHasNoBoundary is the engine half of DoD item 3 for IP-03: a `never`
// entitlement has no boundary, so the window is the anchor with an open end in
// every zone and at any distance into the future (DR-008). The PTTL-as-(-1)
// assertion is a data-store property and is deferred to IP-05.
func TestNeverHasNoBoundary(t *testing.T) {
	for _, zone := range []string{"UTC", "Europe/Berlin", "Asia/Kolkata"} {
		loc := mustLocation(t, zone)
		for _, nowStr := range []string{"2026-01-01T00:00:00Z", "2030-01-01T00:00:00Z", "2100-06-15T12:00:00Z"} {
			now := mustParseRFC3339(t, nowStr)
			w := CurrentWindow(mustParseRFC3339(t, "2026-01-01T00:00:00Z"), Never, loc, now)
			if w.End != nil {
				t.Errorf("zone %s now %s: end = %v, want nil (no boundary)", zone, nowStr, w.End)
			}
			if w.Index != 0 {
				t.Errorf("zone %s now %s: index = %d, want 0", zone, nowStr, w.Index)
			}
			if !w.Start.Equal(mustParseRFC3339(t, "2026-01-01T00:00:00Z")) {
				t.Errorf("zone %s now %s: start = %v, want the anchor", zone, nowStr, w.Start)
			}
		}
	}

	for _, n := range []int64{0, 1, 5} {
		got := Add(mustParseRFC3339(t, "2026-01-01T00:00:00Z"), Never, time.UTC, n)
		if want := mustParseRFC3339(t, "2026-01-01T00:00:00Z"); !got.Equal(want) {
			t.Errorf("Add(never, %d) = %v, want the anchor", n, got)
		}
	}
}
