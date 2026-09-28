// Package cycle is the pure boundary engine. It maps an anchor, an interval and
// a time zone to the cycle window containing an instant, with no I/O, no clock
// read and no state. The whole contract is DR-002 in [domain-rules](../../docs/product/domain-rules.md):
// window(n) = [ boundary(anchor, interval, zone, n), boundary(anchor, interval, zone, n+1) ),
// and every boundary is derived from the anchor rather than from the previous
// boundary, so a missed boundary is indistinguishable from three successive
// advances (DR-009). The behaviours asserted here are the rows of the test
// matrix in [cycle-engine](../../docs/architecture/cycle-engine.md).
//
// The zone database is embedded, not read from the host: this package imports
// time/tzdata for its own tests, where the binary path is covered in
// cmd/quotacore. A host that updates its zone database must not change a
// customer's cycle boundaries retroactively (NFR-OPS8).
package cycle

import (
	"strconv"
	"time"

	_ "time/tzdata"
)

// Interval is the cadence of an entitlement, mirroring the reset_interval
// enum in the API contract.
type Interval int

// The six intervals of the contract. Hourly is the only one that is a
// duration; the rest are wall-clock commitments (DR-006).
const (
	Hourly Interval = iota
	Daily
	Weekly
	Monthly
	Yearly
	Never
)

var intervalNames = [...]string{"hourly", "daily", "weekly", "monthly", "yearly", "never"}

// String returns the wire value of the interval, used for diagnostics and in
// test output.
func (iv Interval) String() string {
	if iv < Hourly || iv > Never {
		return "Interval(" + strconv.Itoa(int(iv)) + ")"
	}
	return intervalNames[iv]
}

// Add returns the n-th boundary after the anchor: boundary(anchor, interval,
// zone, n) in the algebra of cycle-engine.md §2, with boundary(…, 0) the anchor
// itself. For a `never` interval there is no boundary, so Add returns the
// anchor; CurrentWindow never calls it (DR-008).
//
// Calendar arithmetic is performed on the anchor's wall-clock value in the
// tenant's zone, then converted to an instant, never by adding a duration to a
// localised instant (DR-003, ADR-0007). A zone change therefore recomputes
// future boundaries from the same anchor instant without re-anchoring (DR-033).
func Add(anchor time.Time, iv Interval, loc *time.Location, n int64) time.Time {
	switch iv {
	case Hourly:
		return anchor.Add(time.Duration(n) * time.Hour)
	case Daily:
		return addDays(anchor, loc, n)
	case Weekly:
		return addDays(anchor, loc, 7*n)
	case Monthly:
		return addMonths(anchor, loc, n)
	case Yearly:
		return addYears(anchor, loc, n)
	default:
		return anchor
	}
}

// Window is the outcome of CurrentWindow. End is nil for a `never`
// entitlement, which has no boundary, no expiry and no rollover (DR-008).
type Window struct {
	Index int64
	Start time.Time
	End   *time.Time
}

// CurrentWindow returns the window that contains now: the largest n whose
// boundary is at or before now (DR-002). For a `never` entitlement it returns
// index zero and the anchor with an open end.
//
// The returned window is a pure function of its arguments; nothing is read and
// no clock is consulted. If now precedes the anchor, index zero is returned so
// that the caller's monotonic guard can refuse the write as stale rather than
// the engine inventing a boundary that does not exist (C-3).
func CurrentWindow(anchor time.Time, iv Interval, loc *time.Location, now time.Time) Window {
	if iv == Never {
		return Window{Index: 0, Start: anchor, End: nil}
	}
	n := currentIndex(anchor, iv, loc, now)
	end := Add(anchor, iv, loc, n+1)
	return Window{Index: n, Start: Add(anchor, iv, loc, n), End: &end}
}

// currentIndex finds the largest n with Add(n) <= now. Exponential doubling
// followed by a binary search, never a linear scan: a five-year-old hourly
// tenant is about 22 iterations of cheap date arithmetic, and a linear scan
// would be tens of thousands (performance.md §8).
func currentIndex(anchor time.Time, iv Interval, loc *time.Location, now time.Time) int64 {
	hi := int64(1)
	for !Add(anchor, iv, loc, hi).After(now) {
		hi *= 2
	}
	lo := int64(0)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if Add(anchor, iv, loc, mid+1).After(now) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// addDays adds n calendar days on the anchor's wall clock in loc. A DST
// transition therefore makes a daily boundary 23 or 25 hours after its
// predecessor and a weekly boundary 167 or 169 (DR-006, DR-007).
func addDays(anchor time.Time, loc *time.Location, n int64) time.Time {
	a := anchor.In(loc)
	return time.Date(a.Year(), a.Month(), a.Day()+int(n), a.Hour(), a.Minute(), a.Second(), a.Nanosecond(), loc)
}

// addMonths adds n months, clamping the day to the last day of the target
// month. The clamp is applied from the anchor's day-of-month, never from the
// previous boundary's day, which is the entire fix for the drift bug in
// cycle-engine.md §3.1 (DR-004).
func addMonths(anchor time.Time, loc *time.Location, n int64) time.Time {
	a := anchor.In(loc)
	y, m, d := a.Date()
	total := int64(m) - 1 + n
	yy := y + int(total/12)
	mm := total % 12
	if mm < 0 {
		mm += 12
		yy--
	}
	month := time.Month(mm + 1)
	if d > daysInMonth(yy, month) {
		d = daysInMonth(yy, month)
	}
	return time.Date(yy, month, d, a.Hour(), a.Minute(), a.Second(), a.Nanosecond(), loc)
}

// addYears adds n years, clamping a 29 February anchor to 28 February in
// non-leap years while the anchor itself stays 29 February (DR-005).
func addYears(anchor time.Time, loc *time.Location, n int64) time.Time {
	a := anchor.In(loc)
	y, m, d := a.Date()
	yy := y + int(n)
	if d > daysInMonth(yy, m) {
		d = daysInMonth(yy, m)
	}
	return time.Date(yy, m, d, a.Hour(), a.Minute(), a.Second(), a.Nanosecond(), loc)
}

func daysInMonth(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
