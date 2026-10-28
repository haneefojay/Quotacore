package script

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/cycle"
)

// TestNFRL7RolloverOverhead is NFR-L7, testing-strategy section 5's
// boundary-crossing row: a consume that crosses a boundary costs at most a
// millisecond more than one that does not, measured against the real store.
// Each crossing consume finds its hash one window behind, as the first request
// after a boundary does, so the rollover runs inside the same script call; a
// rollover built as several round trips would show here at once. The figure
// is the difference of the two medians over a thousand calls each.
func TestNFRL7RolloverOverhead(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	const n = 1000
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	timed := func(cross bool) []time.Duration {
		var d []time.Duration
		for i := range n {
			feature := featureKey(t) + "-" + strconv.FormatBool(cross) + "-" + strconv.Itoa(i)
			key := keyFor(t, feature)
			first := fixedWindow(0, base, time.Hour)
			seed(t, s, key, state{Balance: 100, Limit: 100, Index: 0, Start: first.Start, End: first.End})
			w := first
			if cross {
				w = fixedWindow(1, base.Add(time.Hour), time.Hour)
			}
			req := Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, Limit: 100, Window: w,
				Interval: cycle.Hourly, At: w.Start.Add(time.Minute), IdempotencyKey: feature + "-k"}
			start := time.Now()
			out, err := runner.Consume(ctx, req)
			d = append(d, time.Since(start))
			if err != nil || out.Decision != Applied || (out.Transition == Rolled) != cross {
				t.Fatalf("consume %d (crossing %v): %+v, %v", i, cross, out, err)
			}
			s.Client().Del(ctx, key)
		}
		slices.Sort(d)
		return d
	}
	plain, crossing := timed(false), timed(true)
	overhead := crossing[n/2] - plain[n/2]
	t.Logf("NFR-L7: median %v without a boundary, %v across one; overhead %v (p99 %v and %v)",
		plain[n/2], crossing[n/2], overhead, plain[n*99/100], crossing[n*99/100])
	if overhead > time.Millisecond {
		t.Errorf("NFR-L7: a rollover adds %v to a request, above 1 ms", overhead)
	}
}
