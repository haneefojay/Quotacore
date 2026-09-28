package cycle

import (
	"testing"
	"time"
)

// matrixCase is one row of the boundary table in cycle-engine.md §6. Every
// timestamp is an exact RFC 3339 instant with an offset; `now` and `anchor`
// carry the offset the zone actually had at that wall-clock moment. wantEnd is
// "null" for the row 13 `never` case, whose window has no end (DR-008).
type matrixCase struct {
	name      string
	zone      string
	interval  Interval
	anchor    string
	now       string
	wantStart string
	wantEnd   string // "null" when no boundary exists
}

var matrix []matrixCase = []matrixCase{
	{name: "1 hourly UTC anchored at midnight", zone: "UTC", interval: Hourly,
		anchor: "2026-01-01T00:00:00Z", now: "2026-01-01T03:30:00Z",
		wantStart: "2026-01-01T03:00:00Z", wantEnd: "2026-01-01T04:00:00Z"},
	{name: "2 daily UTC across three boundaries", zone: "UTC", interval: Daily,
		anchor: "2026-01-01T00:00:00Z", now: "2026-01-05T12:00:00Z",
		wantStart: "2026-01-05T00:00:00Z", wantEnd: "2026-01-06T00:00:00Z"},
	{name: "3 daily UTC, anchor two days before now", zone: "UTC", interval: Daily,
		anchor: "2026-03-28T00:00:00Z", now: "2026-03-30T12:00:00Z",
		wantStart: "2026-03-30T00:00:00Z", wantEnd: "2026-03-31T00:00:00Z"},
	{name: "4 daily Europe/Berlin after spring forward", zone: "Europe/Berlin", interval: Daily,
		anchor: "2026-03-01T00:00:00+01:00", now: "2026-03-30T12:00:00+02:00",
		wantStart: "2026-03-30T00:00:00+02:00", wantEnd: "2026-03-31T00:00:00+02:00"},
	{name: "5 weekly Europe/Berlin crossing spring forward", zone: "Europe/Berlin", interval: Weekly,
		anchor: "2026-01-05T09:00:00+01:00", now: "2026-04-01T00:00:00+02:00",
		wantStart: "2026-03-30T09:00:00+02:00", wantEnd: "2026-04-06T09:00:00+02:00"},
	{name: "6 monthly UTC clamped, 31st anchor, mid month", zone: "UTC", interval: Monthly,
		anchor: "2026-01-31T00:00:00Z", now: "2026-04-10T00:00:00Z",
		wantStart: "2026-03-31T00:00:00Z", wantEnd: "2026-04-30T00:00:00Z"},
	{name: "7 monthly UTC clamped, 31st anchor, 1 March", zone: "UTC", interval: Monthly,
		anchor: "2026-01-31T00:00:00Z", now: "2026-03-01T00:00:00Z",
		wantStart: "2026-02-28T00:00:00Z", wantEnd: "2026-03-31T00:00:00Z"},
	{name: "8 yearly UTC", zone: "UTC", interval: Yearly,
		anchor: "2026-01-01T00:00:00Z", now: "2028-06-01T00:00:00Z",
		wantStart: "2028-01-01T00:00:00Z", wantEnd: "2029-01-01T00:00:00Z"},
	{name: "9 yearly UTC degraded, non-leap", zone: "UTC", interval: Yearly,
		anchor: "2024-02-29T00:00:00Z", now: "2025-06-01T00:00:00Z",
		wantStart: "2025-02-28T00:00:00Z", wantEnd: "2026-02-28T00:00:00Z"},
	{name: "10 yearly UTC restored on a leap year", zone: "UTC", interval: Yearly,
		anchor: "2024-02-29T00:00:00Z", now: "2028-06-01T00:00:00Z",
		wantStart: "2028-02-29T00:00:00Z", wantEnd: "2029-02-28T00:00:00Z"},
	{name: "11 daily America/New_York after spring forward", zone: "America/New_York", interval: Daily,
		anchor: "2026-03-07T00:00:00-05:00", now: "2026-03-09T12:00:00-04:00",
		wantStart: "2026-03-09T00:00:00-04:00", wantEnd: "2026-03-10T00:00:00-04:00"},
	{name: "12 daily Europe/Berlin after autumn fall back", zone: "Europe/Berlin", interval: Daily,
		anchor: "2026-10-24T00:00:00+02:00", now: "2026-10-26T12:00:00+01:00",
		wantStart: "2026-10-26T00:00:00+01:00", wantEnd: "2026-10-27T00:00:00+01:00"},
	{name: "13 never, no boundary four years on", zone: "UTC", interval: Never,
		anchor: "2026-01-01T00:00:00Z", now: "2030-01-01T00:00:00Z",
		wantStart: "2026-01-01T00:00:00Z", wantEnd: "null"},
	{name: "14 monthly UTC on the boundary exactly", zone: "UTC", interval: Monthly,
		anchor: "2026-01-01T00:00:00Z", now: "2026-05-01T00:00:00Z",
		wantStart: "2026-05-01T00:00:00Z", wantEnd: "2026-06-01T00:00:00Z"},
	{name: "15 monthly UTC one millisecond before the boundary", zone: "UTC", interval: Monthly,
		anchor: "2026-01-01T00:00:00Z", now: "2026-04-30T23:59:59.999Z",
		wantStart: "2026-04-01T00:00:00Z", wantEnd: "2026-05-01T00:00:00Z"},
	{name: "16 monthly UTC mid cycle", zone: "UTC", interval: Monthly,
		anchor: "2026-01-01T00:00:00Z", now: "2026-04-15T00:00:00Z",
		wantStart: "2026-04-01T00:00:00Z", wantEnd: "2026-05-01T00:00:00Z"},
	{name: "17 monthly UTC, stored index ahead", zone: "UTC", interval: Monthly,
		anchor: "2026-01-01T00:00:00Z", now: "2026-02-15T00:00:00Z",
		wantStart: "2026-02-01T00:00:00Z", wantEnd: "2026-03-01T00:00:00Z"},
	{name: "18 daily Asia/Kolkata, half-hour offset", zone: "Asia/Kolkata", interval: Daily,
		anchor: "2026-01-01T00:00:00+05:30", now: "2026-01-02T12:00:00+05:30",
		wantStart: "2026-01-02T00:00:00+05:30", wantEnd: "2026-01-03T00:00:00+05:30"},
	{name: "19 hourly UTC", zone: "UTC", interval: Hourly,
		anchor: "2026-01-01T00:00:00Z", now: "2026-01-01T02:30:00Z",
		wantStart: "2026-01-01T02:00:00Z", wantEnd: "2026-01-01T03:00:00Z"},
	{name: "20 monthly UTC, zone change must not re-anchor", zone: "UTC", interval: Monthly,
		anchor: "2026-01-15T14:30:00Z", now: "2026-01-15T15:00:00Z",
		wantStart: "2026-01-15T14:30:00Z", wantEnd: "2026-02-15T14:30:00Z"},
}

// TestBoundaryMatrix is T-07: every row of the boundary table in
// cycle-engine.md §6, asserted at its exact RFC 3339 instant. The table is a
// test fixture, so adding a row to the documentation is adding a test.
func TestBoundaryMatrix(t *testing.T) {
	for _, tc := range matrix {
		t.Run(tc.name, func(t *testing.T) {
			loc := mustLocation(t, tc.zone)
			anchor := mustParseRFC3339(t, tc.anchor)
			now := mustParseRFC3339(t, tc.now)

			w := CurrentWindow(anchor, tc.interval, loc, now)

			if got := w.Start.Format(time.RFC3339); got != tc.wantStart {
				t.Errorf("cycle_start = %s, want %s", got, tc.wantStart)
			}
			if tc.wantEnd == "null" {
				if w.End != nil {
					t.Errorf("cycle_end = %v, want null for a never entitlement", w.End)
				}
			} else if w.End == nil {
				t.Errorf("cycle_end = null, want %s", tc.wantEnd)
			} else if got := w.End.Format(time.RFC3339); got != tc.wantEnd {
				t.Errorf("cycle_end = %s, want %s", got, tc.wantEnd)
			}

			if tc.interval != Never {
				if now.Before(w.Start) {
					t.Errorf("window starts after now: now %s, start %s", now, w.Start)
				}
				if now.After(*w.End) || now.Equal(*w.End) {
					t.Errorf("window ends before or at now: now %s, end %s", now, *w.End)
				}
			}
		})
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	if name == "UTC" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("location %s: %v", name, err)
	}
	return loc
}

func mustParseRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}
