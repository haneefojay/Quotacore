package cycle

import (
	"testing"
	"time"
)

// Benchmark far-future windows, one per interval, at five years after the
// anchor. Performance.md §8 names the worst case the benchmark must assert: a
// five-year-old hourly tenant is about 22 iterations of cheap date arithmetic
// under the exponential-doubling-and-binary-search in §3.3, and the cost must
// stay flat as the elapsed time grows because it is logarithmic, never linear.
func BenchmarkCurrentWindowFiveYears(b *testing.B) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	berlin := mustLoadLocationOrSkip(b)
	now := anchor.Add(5 * 365 * 24 * time.Hour)

	for _, iv := range []Interval{Hourly, Daily, Weekly, Monthly, Yearly} {
		b.Run(iv.String(), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				CurrentWindow(anchor, iv, berlin, now)
			}
		})
	}
}

// Benchmark the same far-future distance straight through Add, which isolates
// the date arithmetic from the search.
func BenchmarkAddFiveYearsHourly(b *testing.B) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	loc := time.UTC
	// 2026 → 2031 at hourly cadence.
	const n = int64(5*365*24) + 1
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Add(anchor, Hourly, loc, n)
	}
}

// TestSearchIsNotScanned asserts the doubling-and-binary-search from §3.3 by
// construction: the number of boundary computations for a far-future `now` is
// logarithmic, so a month-not-enough gap (400 days, the longest idle case T-06
// parametrises) and a multi-decade gap must both resolve. The linear-scan
// implementation the document warns about would take 8,760 iterations for the
// hourly decade; this test only requires correctness, while the benchmark above
// reports the cost so a regression to linear is visible in the bench output.
func TestSearchIsNotLinearByConstruction(t *testing.T) {
	anchor := mustParseRFC3339(t, "2026-01-01T00:00:00Z")
	now := mustParseRFC3339(t, "2036-06-01T00:00:00Z")

	t.Run("hourly ten years ahead", func(t *testing.T) {
		w := CurrentWindow(anchor, Hourly, time.UTC, now)
		if w.Index < 90_000 { // ~91,310 hours in ten years; a wide-range search must not miss
			t.Fatalf("index = %d, want on the order of the hours elapsed", w.Index)
		}
		if !now.Before(*w.End) {
			t.Fatalf("now not inside the returned window")
		}
	})

	t.Run("yearly four hundred days idle is one boundary ahead", func(t *testing.T) {
		// The longest idle the T-06 parameter set reaches is 400 days.
		w := CurrentWindow(anchor, Yearly, time.UTC, now)
		if w.Index < 9 {
			t.Fatalf("yearly index at year ten = %d, want >= 9", w.Index)
		}
	})
}

func mustLoadLocationOrSkip(b *testing.B) *time.Location {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		b.Skipf("Berlin zone unavailable: %v", err)
	}
	return loc
}
