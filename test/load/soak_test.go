package load_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quotacore/quotacore/test/harness"
)

// TestSoak is testing-strategy.md section 5's soak row as amended by IP-16
// (D4): 500 consumes a second for QUOTACORE_SOAK_DURATION, 5 h 30 m by
// default because a hosted runner's job ends at 6 h, with "no leak" asserted
// as a slope rather than a final value. Every 30 s it samples the service's
// memory, goroutines and open descriptors, the data store's memory, and the
// skew between the service's clock and the store's. Over the second half, by
// when every pool and cache has filled, a least-squares slope projected across
// that half must stay inside noise. Then counter drift: every consume that
// succeeded is one ledger row, and the balances sum to what was granted less
// what was consumed.
//
// The store grows on purpose: every idempotency record is held for 24 h
// (DR-029), so its memory is checked per record against IP-15's figure rather
// than for a flat line.
func TestSoak(t *testing.T) {
	harness.Require(t, "SOAK")
	length := 5*time.Hour + 30*time.Minute
	if raw := os.Getenv("QUOTACORE_SOAK_DURATION"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("QUOTACORE_SOAK_DURATION=%q: %v", raw, err)
		}
		length = d
	}
	s := harness.New(t)
	// Small enough that the sums below are exact in a float64.
	const limit = int64(1) << 30
	seeded := s.Seed("soak", 4, 200, 1, limit, 16)
	store := s.Store()
	keys := func() float64 { return float64(store.DBSize(context.Background()).Val()) }
	fds := func() float64 { n, _ := s.OpenFDs("quotacore"); return float64(n) }
	used := func() float64 { return s.StoreInfo("memory", "used_memory") }

	// Every tenant cached before the clock starts, as the other load runs do:
	// a cold start at 500 a second would otherwise meet the snapshot's miss
	// limiter (20 a second) for the first ten seconds, which the first soak
	// run counted as 1,958 refusals.
	s.Prime(seeded)
	usedBefore, keysBefore := used(), keys()
	start := time.Now()
	sampler := harness.Sample(30*time.Second, fds, used)
	r := harness.Run(context.Background(), harness.Load{URL: s.Direct(), Keys: seeded.Keys, Tenants: seeded.Tenants,
		Features: seeded.Features, Rate: 500, Duration: length, Concurrency: 256, RunID: "soak"})
	points := sampler.Stop()
	usedAfter, keysAfter := used(), keys()
	// What a failure needs to be placed and attributed: the service's 5xx by
	// minute and cause, every counter it exposes, and Postgres's own account
	// of its vacuums and checkpoints, which share the two CPUs.
	serverErrors := s.ServerErrors(start)
	final, _ := harness.Scrape()
	postgres := s.LogLines("postgres", start, "automatic vacuum", "automatic analyze", "checkpoint complete")

	second := points[len(points)/2:]
	span := 0.0
	if len(second) > 1 {
		span = (second[len(second)-1].At - second[0].At).Hours()
	}
	growth := func(of func(harness.Point) float64) (projected, mean float64) {
		var x, y []float64
		for _, p := range second {
			x = append(x, p.At.Hours())
			y = append(y, of(p))
			mean += of(p)
		}
		if len(y) > 0 {
			mean /= float64(len(y))
		}
		return harness.Slope(x, y) * span, mean
	}
	memGrowth, memMean := growth(func(p harness.Point) float64 { return p.Memory })
	goGrowth, goMean := growth(func(p harness.Point) float64 { return p.Goroutines })
	fdGrowth, fdMean := growth(func(p harness.Point) float64 { return p.FDs })
	var worstSkew float64
	for _, p := range points {
		worstSkew = max(worstSkew, p.Skew, -p.Skew)
	}

	ledgerRows, ledgerSum := settle(t, s)
	balances := sumBalances(t, s)
	granted := float64(limit) * float64(len(seeded.Tenants)*len(seeded.Features))
	perRecord := (usedAfter - usedBefore) / max(1, keysAfter-keysBefore)
	record(t, map[string]any{"load": r, "duration": length.String(), "samples": points,
		"memory_growth_second_half": memGrowth, "memory_mean": memMean,
		"goroutine_growth_second_half": goGrowth, "goroutine_mean": goMean,
		"fd_growth_second_half": fdGrowth, "fd_mean": fdMean, "worst_clock_skew_seconds": worstSkew,
		"ledger_rows": ledgerRows, "ledger_sum": ledgerSum, "balance_sum": balances,
		"store_bytes_per_record": perRecord, "server_errors": serverErrors, "metrics": final,
		"postgres_maintenance": postgres})

	noErrors(t, r)
	if memGrowth > 0.10*memMean {
		t.Errorf("memory rose %.0f bytes over the second half, against a mean of %.0f", memGrowth, memMean)
	}
	if goGrowth > max(10, 0.05*goMean) {
		t.Errorf("goroutines rose %.1f over the second half, against a mean of %.0f", goGrowth, goMean)
	}
	if fdGrowth > max(10, 0.05*fdMean) {
		t.Errorf("open descriptors rose %.1f over the second half, against a mean of %.0f", fdGrowth, fdMean)
	}
	if worstSkew >= 1 {
		t.Errorf("the clock skew reached %.3f s, the alert's threshold (Q-26)", worstSkew)
	}
	if ledgerRows != float64(r.OK) || -ledgerSum != float64(r.OK) {
		t.Errorf("counter drift: %d consumes succeeded, the ledger holds %.0f rows summing to %.0f", r.OK, ledgerRows, ledgerSum)
	}
	if granted-balances != float64(r.OK) {
		t.Errorf("counter drift: the balances fell by %.0f for %d consumes", granted-balances, r.OK)
	}
	if perRecord > 522.9*1.1 {
		t.Errorf("the store grew %.1f bytes a record, against IP-15's 522.9", perRecord)
	}
}

// settle waits for the ledger to stop moving and returns its consumed rows and
// their sum.
func settle(t *testing.T, s *harness.Stack) (rows, sum float64) {
	t.Helper()
	last := -1.0
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		rows = number(t, s.SQL(`SELECT count(*) FROM usage_events WHERE event_type = 'consumed'`))
		if rows == last {
			break
		}
		last = rows
	}
	// sum(bigint) is numeric, which the driver does not hand back as a
	// number; the cast keeps it one.
	sum = number(t, s.SQL(`SELECT coalesce(sum(delta), 0)::bigint FROM usage_events WHERE event_type = 'consumed'`))
	return rows, sum
}

func number(t *testing.T, raw string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("not a number from Postgres: %q", raw)
	}
	return f
}

// sumBalances adds every balance hash's balance in the store.
func sumBalances(t *testing.T, s *harness.Stack) float64 {
	t.Helper()
	ctx := context.Background()
	store := s.Store()
	var total float64
	iter := store.Scan(ctx, 0, "qc:{t:*}:bal:*", 1000).Iterator()
	for iter.Next(ctx) {
		if strings.Count(iter.Val(), ":bal:") != 1 {
			continue
		}
		v, err := store.HGet(ctx, iter.Val(), "balance").Float64()
		if err != nil {
			t.Fatalf("%s: %v", iter.Val(), err)
		}
		total += v
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}
	return total
}
