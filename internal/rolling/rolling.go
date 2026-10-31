// Package rolling names the seven rolling windows a rolling feature can be
// configured with (DR-052, ADR-0008) and nothing else: their wire names, their
// durations, and the width of one of their sixty buckets. The arithmetic that
// counts a window is in the store's scripts, which read nothing but these
// numbers and the instant.
//
// It deliberately does not import internal/cycle, and a source test fails if it
// ever does. A rolling window has no anchor, no calendar and no cycle index, so
// it shares no rule with the anchored engine, and the anchored engine, proved at
// IP-03, is not touched by the second cadence (cycle-engine.md section 9).
package rolling

import "time"

// Buckets is how many buckets one window is counted in. The precision of a
// window sum is one bucket: a unit counts for at least the window less one
// bucket and at most the window (DR-052).
const Buckets = 60

// Window is one of the seven supported lookbacks.
type Window struct {
	name     string
	duration time.Duration
}

// The seven windows, in the order the contract lists them.
var windows = []Window{
	{"rolling_5m", 5 * time.Minute},
	{"rolling_15m", 15 * time.Minute},
	{"rolling_1h", time.Hour},
	{"rolling_6h", 6 * time.Hour},
	{"rolling_24h", 24 * time.Hour},
	{"rolling_7d", 7 * 24 * time.Hour},
	{"rolling_30d", 30 * 24 * time.Hour},
}

// All lists the supported windows.
func All() []Window { return append([]Window(nil), windows...) }

// Parse resolves a wire name, such as rolling_1h. A name outside the supported
// set reports false, and the caller answers invalid_window (IP-19 DoD 7).
func Parse(name string) (Window, bool) {
	for _, w := range windows {
		if w.name == name {
			return w, true
		}
	}
	return Window{}, false
}

// FromSeconds resolves the stored form, plan_entitlements.rolling_window_seconds.
func FromSeconds(s int64) (Window, bool) {
	for _, w := range windows {
		if int64(w.duration/time.Second) == s {
			return w, true
		}
	}
	return Window{}, false
}

// Name is the wire name, such as rolling_1h.
func (w Window) Name() string { return w.name }

// Duration is the length of the lookback.
func (w Window) Duration() time.Duration { return w.duration }

// Seconds is the stored form.
func (w Window) Seconds() int64 { return int64(w.duration / time.Second) }

// Precision is the width of one bucket, which is the precision of the window
// sum, and it is a whole number of seconds for every supported window.
func (w Window) Precision() time.Duration { return w.duration / Buckets }

// IsZero reports a Window that names nothing, which is how an entitlement
// that is not rolling carries one.
func (w Window) IsZero() bool { return w.duration == 0 }
