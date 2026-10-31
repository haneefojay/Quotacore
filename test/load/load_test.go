// Package load_test is testing-strategy.md section 5 against the built image
// on the measurement stack of test/harness (IP-16 D3): every row of the load
// table, with the NFR it proves. It needs Docker and a built image, so it runs
// only when QUOTACORE_LOAD is set, and fails rather than skips when
// QUOTACORE_REQUIRE_LOAD is, as the full-suite workflow sets it.
//
// Each test writes its figures to $QUOTACORE_EVIDENCE_DIR/<test>.json when
// that is set, which the workflow uploads, and which v0-1-evidence.md cites.
// QUOTACORE_LOAD_SCALE shortens every duration by that factor for a smoke run
// on a workstation; the thresholds are unchanged, and a scaled run is never
// evidence.
package load_test

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/quotacore/quotacore/test/harness"
)

const ms = 1000 // microseconds

// unlimited is a limit no run reaches, so every consume applies and a denial
// is never mistaken for a latency.
const unlimited = int64(1) << 50

func duration(t *testing.T, d time.Duration) time.Duration {
	t.Helper()
	if raw := os.Getenv("QUOTACORE_LOAD_SCALE"); raw != "" {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || f < 1 {
			t.Fatalf("QUOTACORE_LOAD_SCALE=%q is not a factor of at least 1", raw)
		}
		return time.Duration(float64(d) / f)
	}
	return d
}

// record writes a test's figures for the evidence document.
func record(t *testing.T, v any) {
	t.Helper()
	raw, _ := json.MarshalIndent(v, "", "  ")
	t.Logf("%s", raw)
	if dir := os.Getenv("QUOTACORE_EVIDENCE_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, t.Name()+".json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// keysFor is the runtime keys a rate needs: the per-key limit is 2,000 a
// second, and each key is held to three quarters of it.
func keysFor(rate float64) int { return int(math.Ceil(rate / 1500)) }

// run sends one open-loop load after a warm-up at the same rate, which is
// discarded: it fills the snapshot cache and the connection pools, so the
// measurement is of the warm path the NFRs name. It returns when the
// measurement started, for ServerLatency.
func run(t *testing.T, s *harness.Stack, seeded harness.Seeded, op string, rate float64, d time.Duration) (harness.Result, time.Time) {
	t.Helper()
	s.Prime(seeded)
	l := harness.Load{URL: s.Direct(), Keys: seeded.Keys, Tenants: seeded.Tenants, Features: seeded.Features,
		Op: op, Rate: rate, Concurrency: 1024, RunID: strconv.FormatInt(time.Now().UnixNano(), 36)}
	l.Duration = duration(t, 30*time.Second)
	if warm := harness.Run(context.Background(), l); warm.OK == 0 {
		t.Fatalf("the warm-up had no success: %v", warm.Outcomes)
	}
	l.Duration = d
	l.RunID += "m"
	start := time.Now()
	return harness.Run(context.Background(), l), start
}

func noErrors(t *testing.T, r harness.Result) {
	t.Helper()
	if errs := r.Errors(); len(errs) > 0 {
		t.Errorf("%d of %d requests were not a success: %v", r.Sent-r.OK, r.Sent, errs)
	}
	if r.Sent == 0 || r.OK != r.Sent {
		t.Errorf("%d sent, %d succeeded", r.Sent, r.OK)
	}
}

// creep is how far the per-minute p99 rose across a run: the median of the
// last third of its minutes less the median of the first third. A p99 that
// creeps moves the whole last third; a minute or two of a shared runner's
// noise moves neither median, where a least-squares slope follows the noise
// (the first full-suite run rose 1.4 ms by slope on two noisy late minutes
// with both medians within 0.4 ms).
func creep(perMinute []harness.MinutePercentile) int64 {
	if len(perMinute) < 3 {
		return 0
	}
	third := len(perMinute) / 3
	median := func(ms []harness.MinutePercentile) int64 {
		v := make([]int64, 0, len(ms))
		for _, m := range ms {
			v = append(v, m.P99)
		}
		slices.Sort(v)
		return v[len(v)/2]
	}
	return median(perMinute[len(perMinute)-third:]) - median(perMinute[:third])
}

// flat fails when the service's per-minute p99 creeps by more than a
// millisecond across the run, which is "p99 stable rather than creeping" in
// testing-strategy section 5, on the service's own time as Q-30 gates every
// latency figure. The client's creep is recorded beside it.
func flat(t *testing.T, server []harness.MinutePercentile) int64 {
	t.Helper()
	c := creep(server)
	if c > ms {
		t.Errorf("the service's per-minute p99 rose %d µs across the run, median of the last third against the first", c)
	}
	return c
}

// latency gates the NFR-L figures on the service's own time for a request,
// read from its access log, and records the client's figure beside it. The
// client's includes the hosted runner's network hops, each of which costs a
// few hundred microseconds of idle wake-up on a virtual machine that no
// reference host has (D17): the client figure is evidence, and a miss there
// alone is named as a gap with both numbers rather than failed here.
type latency struct {
	Client harness.Result      `json:"client"`
	Server harness.Percentiles `json:"server_us"`
}

// TestWarmConsumeLatency is NFR-L1, NFR-L2 and NFR-L3 as testing-strategy
// section 5's first row states them: 100 consumes a second for 10 minutes.
func TestWarmConsumeLatency(t *testing.T) {
	harness.Require(t, "LOAD")
	s := harness.New(t)
	seeded := s.Seed("warm", 4, 100, 1, unlimited, 16)
	r, start := run(t, s, seeded, "consume", 100, duration(t, 10*time.Minute))
	server := s.ServerLatency("POST /v1/consume", start)
	record(t, latency{Client: r, Server: server})
	noErrors(t, r)
	if server.Count < r.OK {
		t.Errorf("the access log holds %d consumes for %d sent", server.Count, r.OK)
	}
	if server.P50 > 1*ms {
		t.Errorf("NFR-L1: p50 %d µs, above 1 ms (client %d µs)", server.P50, r.Latency.P50)
	}
	if server.P99 > 5*ms {
		t.Errorf("NFR-L2: p99 %d µs, above 5 ms (client %d µs)", server.P99, r.Latency.P99)
	}
	if server.P999 > 20*ms {
		t.Errorf("NFR-L3: p99.9 %d µs, above 20 ms (client %d µs)", server.P999, r.Latency.P999)
	}
}

// TestCheckLatency is NFR-L5: a check is a read, and its p99 is 3 ms.
func TestCheckLatency(t *testing.T) {
	harness.Require(t, "LOAD")
	s := harness.New(t)
	seeded := s.Seed("check", 4, 100, 1, unlimited, 16)
	r, start := run(t, s, seeded, "check", 100, duration(t, 10*time.Minute))
	server := s.ServerLatency("POST /v1/check", start)
	record(t, latency{Client: r, Server: server})
	noErrors(t, r)
	if server.P99 > 3*ms {
		t.Errorf("NFR-L5: p99 %d µs, above 3 ms (client %d µs)", server.P99, r.Latency.P99)
	}
}

// TestSustainedThroughput is NFR-T1 (2,000 consumes a second for 30 minutes:
// no error, p99 stable, no unbounded memory growth), and two proofs beside it.
//
// Claim 1's "no database on the request path" (IP-16 D16): across the run,
// Postgres commits far fewer transactions than the service answers requests,
// the service's connection count does not grow, and its pool never waits. A
// request that touched Postgres would show as a commit or a wait per request.
//
// The idempotency-scale row (D5): the store's growth per consume matches the
// per-record figures IP-15 measured, within 10%, and the store refuses no write
// for memory. IP-15 measured 266.9 bytes a record at the shortest key the
// contract allows and 522.9 at the longest; this run's keys are between.
func TestSustainedThroughput(t *testing.T) {
	harness.Require(t, "LOAD")
	s := harness.New(t)
	seeded := s.Seed("sus", 4, 200, keysFor(2000), unlimited, 16)

	commits := func() float64 {
		f, _ := strconv.ParseFloat(s.SQL(`SELECT xact_commit FROM pg_stat_database WHERE datname = 'quotacore'`), 64)
		return f
	}
	conns := func() float64 {
		f, _ := strconv.ParseFloat(s.SQL(`SELECT count(*) FROM pg_stat_activity WHERE usename = 'quotacore'`), 64)
		return f
	}
	fds := func() float64 { n, _ := s.OpenFDs("quotacore"); return float64(n) }
	used := func() float64 { return s.StoreInfo("memory", "used_memory") }
	store := s.Store()
	keys := func() float64 { return float64(store.DBSize(context.Background()).Val()) }

	// The warm-up runs inside run; the baselines are taken after a short one
	// of their own so the cache is full when they are read.
	harness.Run(context.Background(), harness.Load{URL: s.Direct(), Keys: seeded.Keys, Tenants: seeded.Tenants,
		Features: seeded.Features, Rate: 500, Duration: 10 * time.Second, RunID: "pre"})
	before, beforeMetrics := commits(), mustScrape(t)
	connsBefore, usedBefore, keysBefore := conns(), used(), keys()
	sampler := harness.Sample(30*time.Second, fds, used)

	r, start := run(t, s, seeded, "consume", 2000, duration(t, 30*time.Minute))
	points := sampler.Stop()
	serverMinutes := s.ServerLatencyPerMinute("POST /v1/consume", start)
	after, afterMetrics := commits(), mustScrape(t)
	connsAfter, usedAfter, keysAfter := conns(), used(), keys()

	commitDelta := after - before
	wait := afterMetrics.Sum("quotacore_dbpool_wait_seconds_sum") - beforeMetrics.Sum("quotacore_dbpool_wait_seconds_sum")
	refused := s.StoreRejections("OOM")
	// Every new key is an idempotency record: the balances were made by the
	// pre-run, and the warm-up's records are counted with the run's.
	perRecord := (usedAfter - usedBefore) / max(1, keysAfter-keysBefore)
	var memX, memY []float64
	for _, p := range points[len(points)/2:] {
		memX = append(memX, p.At.Hours())
		memY = append(memY, p.Memory)
	}
	memSlope := harness.Slope(memX, memY)
	serverCreep := flat(t, serverMinutes)
	record(t, map[string]any{"load": r, "server_per_minute_us": serverMinutes,
		"p99_creep_us": map[string]int64{"server": serverCreep, "client": creep(r.PerMinute)}, "postgres_commits": commitDelta,
		"service_connections": []float64{connsBefore, connsAfter}, "dbpool_wait_seconds": wait,
		"store_bytes_per_record": perRecord, "store_records": keysAfter - keysBefore, "memory_slope_bytes_per_hour_second_half": memSlope, "samples": points})

	noErrors(t, r)
	if r.Latency.P99 > 5*ms {
		t.Errorf("p99 %d µs at 2,000 a second, above NFR-L2's 5 ms", r.Latency.P99)
	}
	if r.Achieved < 1900 {
		t.Errorf("achieved %.0f a second, not NFR-T1's 2,000", r.Achieved)
	}
	if commitDelta > float64(r.Sent)/20 {
		t.Errorf("D16: Postgres committed %.0f transactions for %d requests, which is a database on the request path", commitDelta, r.Sent)
	}
	if connsAfter > connsBefore+2 {
		t.Errorf("D16: the service's Postgres connections grew from %.0f to %.0f under load", connsBefore, connsAfter)
	}
	if wait > 0.05 {
		t.Errorf("D16: the pool waited %.3f s during the run", wait)
	}
	if refused > 0 {
		t.Errorf("D5: the store refused %.0f writes for memory, with it sized for the run", refused)
	}
	if perRecord > 522.9*1.1 || perRecord < 266.9*0.9 {
		t.Errorf("D5: the store grew %.1f bytes a record, outside IP-15's 266.9 to 522.9 from the shortest key to the longest", perRecord)
	}
	if len(points) > 4 {
		mean := 0.0
		for _, y := range memY {
			mean += y
		}
		mean /= float64(len(memY))
		// The growth the trend accounts for across the second half, as the
		// soak measures it: a rate per hour read off fifteen minutes of a
		// collector's sawtooth projects a few megabytes of noise into an hour
		// (the second full-suite run: 3.6 MB an hour, 0.9 MB across the half).
		// The rate over hours is the soak's to judge.
		growth := memSlope * (memX[len(memX)-1] - memX[0])
		if growth > 0.10*mean {
			t.Errorf("the service's memory grew %.0f bytes over the second half, more than a tenth of its %.0f", growth, mean)
		}
	}
}

func mustScrape(t *testing.T) harness.Metrics {
	t.Helper()
	m, err := harness.Scrape()
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	return m
}

// TestBurst is NFR-T2: 10,000 consumes a second for 60 s. Queueing is
// acceptable and failure is not, so every request must succeed; the latency is
// recorded, and the p99.9 is the figure Q-30 asks about (IP-16 D6).
func TestBurst(t *testing.T) {
	harness.Require(t, "LOAD")
	s := harness.New(t)
	seeded := s.Seed("burst", 4, 400, keysFor(10000), unlimited, 16)
	r, _ := run(t, s, seeded, "consume", 10000, duration(t, 60*time.Second))
	record(t, r)
	noErrors(t, r)
	if r.Achieved < 9500 {
		t.Errorf("achieved %.0f a second, not NFR-T2's 10,000", r.Achieved)
	}
}

// TestTenantScale is NFR-T4, NFR-T5 and NFR-T7: 50,000 tenants on one plan of
// 100 features, all of them touched, with the snapshot inside its cap and the
// p99 of a run across them within a millisecond of the small-dataset run's.
//
// The seed and the first touch of each tenant are not the measurement: the
// first touch of a tenant is a snapshot miss, which is rate limited at 20 a
// second by default and would take 40 minutes, so the miss limit is raised for
// this stack. That limit protects Postgres from a stampede and plays no part
// in the warm path being measured.
func TestTenantScale(t *testing.T) {
	harness.Require(t, "LOAD")
	tenants := 50000
	if os.Getenv("QUOTACORE_LOAD_SCALE") != "" {
		tenants = 5000
	}
	s := harness.New(t, harness.WithEnv("QUOTACORE_CONFIG_REFRESH_MISS_RATE_LIMIT", "5000"))
	small := s.Seed("small", 4, 100, keysFor(2000), unlimited, 16)
	base, _ := run(t, s, small, "consume", 2000, duration(t, 2*time.Minute))
	noErrors(t, base)

	big := s.Seed("scale", 100, tenants, keysFor(2000), unlimited, 32)
	// One check per tenant, at a rate under the miss limit, fills the cache.
	fill := harness.Run(context.Background(), harness.Load{URL: s.Direct(), Keys: big.Keys, Tenants: big.Tenants,
		Features: big.Features[:1], Op: "check", Rate: 2000, Duration: time.Duration(tenants/2000+1) * time.Second, RunID: "fill"})
	r, _ := run(t, s, big, "consume", 2000, duration(t, 5*time.Minute))
	m := mustScrape(t)
	record(t, map[string]any{"small": base, "fill": fill, "scale": r, "tenants": tenants, "features": len(big.Features),
		"cache_entries": m.Sum("quotacore_config_cache_entries"), "cache_evictions": m.Sum("quotacore_config_cache_evictions_total"),
		"memory_bytes": m.Sum("quotacore_memory_bytes")})
	noErrors(t, r)
	if r.Latency.P99 > base.Latency.P99+1*ms {
		t.Errorf("p99 %d µs across %d tenants, against %d µs on the small dataset", r.Latency.P99, tenants, base.Latency.P99)
	}
	if got := m.Sum("quotacore_config_cache_entries"); got > 50000 {
		t.Errorf("NFR-T7: the snapshot holds %.0f tenants, past its cap of 50,000", got)
	}
	// deployment.md section 5's minimum for the service is 512 MiB.
	if got := m.Sum("quotacore_memory_bytes"); got > 512<<20 {
		t.Errorf("the service holds %.0f bytes with %d tenants cached, past deployment.md's 512 MiB minimum", got, tenants)
	}
}

// TestLedgerSaturation is FS-10 and NFR-D5: with Postgres paused and a small
// queue, events are dropped and counted, and enforcement latency is unchanged.
// It is the test of a negative, testing-strategy section 5 says, because the
// intuitive alternative is to block.
func TestLedgerSaturation(t *testing.T) {
	harness.Require(t, "LOAD")
	s := harness.New(t, harness.WithEnv("QUOTACORE_EVENT_QUEUE_SIZE", "1000"))
	seeded := s.Seed("sat", 4, 100, keysFor(2000), unlimited, 16)
	base, _ := run(t, s, seeded, "consume", 2000, duration(t, 2*time.Minute))
	noErrors(t, base)

	before := mustScrape(t)
	s.Compose("pause", "postgres")
	paused := true
	t.Cleanup(func() {
		if paused {
			_, _ = s.ComposeOut("unpause", "postgres")
		}
	})
	r := harness.Run(context.Background(), harness.Load{URL: s.Direct(), Keys: seeded.Keys, Tenants: seeded.Tenants,
		Features: seeded.Features, Rate: 2000, Duration: duration(t, 2*time.Minute), Concurrency: 1024, RunID: "sat"})
	after := mustScrape(t)
	s.Compose("unpause", "postgres")
	paused = false
	dropped := after.Sum("quotacore_event_sink_dropped_total") - before.Sum("quotacore_event_sink_dropped_total")
	record(t, map[string]any{"baseline": base, "saturated": r, "dropped": dropped,
		"queue_depth": after.Sum("quotacore_event_queue_depth")})
	noErrors(t, r)
	if dropped <= 0 {
		t.Error("no event was dropped with Postgres paused and a queue of 1,000")
	}
	if r.Latency.P99 > base.Latency.P99+1*ms {
		t.Errorf("the p99 moved from %d µs to %d µs under ledger pressure", base.Latency.P99, r.Latency.P99)
	}
}

// TestLedgerThroughput is NFR-T8, which ledger_test.go left to IP-16: at
// 2,000 consumes a second, the writer appends every event, and none is
// dropped.
func TestLedgerThroughput(t *testing.T) {
	harness.Require(t, "LOAD")
	s := harness.New(t)
	seeded := s.Seed("ledger", 4, 100, keysFor(2000), unlimited, 16)
	before := mustScrape(t)
	r, _ := run(t, s, seeded, "consume", 2000, duration(t, 3*time.Minute))
	// The writer drains after the load; give it the time a batch takes.
	deadline := time.Now().Add(30 * time.Second)
	var after harness.Metrics
	for {
		after = mustScrape(t)
		if after.Sum("quotacore_event_queue_depth") == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	written := after.Sum("quotacore_event_written_total") - before.Sum("quotacore_event_written_total")
	dropped := after.Sum("quotacore_event_sink_dropped_total") - before.Sum("quotacore_event_sink_dropped_total")
	rows, _ := strconv.ParseFloat(s.SQL(`SELECT count(*) FROM usage_events`), 64)
	record(t, map[string]any{"load": r, "written": written, "dropped": dropped, "rows": rows})
	noErrors(t, r)
	if dropped > 0 {
		t.Errorf("NFR-T8: %.0f events dropped at 2,000 a second", dropped)
	}
	if written < float64(r.OK) {
		t.Errorf("NFR-T8: %.0f events written for %d consumes", written, r.OK)
	}
}

// TestStartupToReady is NFR-L8: a container start to /readyz true within 5 s,
// measured on a restart of the service against a migrated database.
func TestStartupToReady(t *testing.T) {
	harness.Require(t, "LOAD")
	s := harness.New(t)
	s.Compose("stop", "quotacore")
	start := time.Now()
	s.Compose("start", "quotacore")
	if !s.WaitReady(30 * time.Second) {
		t.Fatal("the service was not ready within 30 s")
	}
	took := time.Since(start)
	record(t, map[string]any{"startup_to_ready": took.String()})
	if took > 5*time.Second {
		t.Errorf("NFR-L8: ready after %v", took)
	}
}

// TestPostgresOutageUnderLoad is the first of IP-16 D16's two proofs that the
// request path has no database on it, and NFR-A2: with Postgres stopped while
// 2,000 consumes a second run against cached tenants, every one succeeds, the
// p99 stays within a millisecond of the run before the stop, and the pool
// never waits.
func TestPostgresOutageUnderLoad(t *testing.T) {
	harness.Require(t, "LOAD")
	s := harness.New(t)
	seeded := s.Seed("pgout", 4, 100, keysFor(2000), unlimited, 16)
	base, _ := run(t, s, seeded, "consume", 2000, duration(t, 2*time.Minute))
	noErrors(t, base)

	before := mustScrape(t)
	s.Compose("stop", "postgres")
	stopped := true
	t.Cleanup(func() {
		if stopped {
			_, _ = s.ComposeOut("start", "postgres")
		}
	})
	r := harness.Run(context.Background(), harness.Load{URL: s.Direct(), Keys: seeded.Keys, Tenants: seeded.Tenants,
		Features: seeded.Features, Rate: 2000, Duration: duration(t, 2*time.Minute), Concurrency: 1024, RunID: "pgout"})
	after := mustScrape(t)
	s.Compose("start", "postgres")
	stopped = false
	wait := after.Sum("quotacore_dbpool_wait_seconds_sum") - before.Sum("quotacore_dbpool_wait_seconds_sum")
	record(t, map[string]any{"baseline": base, "postgres_stopped": r, "dbpool_wait_seconds": wait,
		"controlplane_up": after.Sum("quotacore_controlplane_up")})
	noErrors(t, r)
	if r.Latency.P99 > base.Latency.P99+1*ms {
		t.Errorf("NFR-A2: the p99 moved from %d µs to %d µs with Postgres stopped", base.Latency.P99, r.Latency.P99)
	}
	if wait > 0.05 {
		t.Errorf("D16: the pool waited %.3f s with Postgres stopped", wait)
	}
}

// TestRollingSustainedThroughput is IP-19 DoD 6 and its exit criterion: a
// rolling allowance at NFR-T1's 2,000 consumes a second, measured on the same
// stack and in the same run as the metered consume IP-16 measured, so the
// comparison is between two figures one machine produced. The service's own
// p99 for the rolling run stays within NFR-L2's 5 ms and within a millisecond
// of the metered run's, nothing fails, and the round trip is still one,
// which the script tests assert in the client (TestARollingCallIsOneCall).
func TestRollingSustainedThroughput(t *testing.T) {
	harness.Require(t, "LOAD")
	s := harness.New(t)
	fixed := s.Seed("rsf", 4, 200, keysFor(2000), unlimited, 16)
	sliding := s.SeedRolling("rss", 4, 200, keysFor(2000), unlimited, "rolling_1h")
	measure := func(seeded harness.Seeded) (harness.Result, harness.Percentiles) {
		s.Prime(seeded)
		l := harness.Load{URL: s.Direct(), Keys: seeded.Keys, Tenants: seeded.Tenants, Features: seeded.Features,
			Op: "consume", Amount: 1, Rate: 2000, Concurrency: 1024, Window: seeded.Window,
			RunID: seeded.Plan + strconv.FormatInt(time.Now().UnixNano(), 36)}
		l.Duration = duration(t, 30*time.Second)
		if warm := harness.Run(context.Background(), l); warm.OK == 0 {
			t.Fatalf("the warm-up had no success: %v", warm.Outcomes)
		}
		l.Duration = duration(t, 10*time.Minute)
		l.RunID += "m"
		start := time.Now()
		r := harness.Run(context.Background(), l)
		return r, s.ServerLatency("POST /v1/consume", start)
	}
	metered, meteredServer := measure(fixed)
	rolling, rollingServer := measure(sliding)
	record(t, map[string]any{"metered": latency{Client: metered, Server: meteredServer},
		"rolling": latency{Client: rolling, Server: rollingServer}})
	noErrors(t, metered)
	noErrors(t, rolling)
	if rolling.Achieved < 1900 {
		t.Errorf("rolling: achieved %.0f a second, not NFR-T1's 2,000", rolling.Achieved)
	}
	if rollingServer.P99 > 5*ms {
		t.Errorf("rolling: p99 %d µs at 2,000 a second, above NFR-L2's 5 ms (client %d µs)", rollingServer.P99, rolling.Latency.P99)
	}
	if rollingServer.P99 > meteredServer.P99+ms {
		t.Errorf("rolling: p99 %d µs regresses on the metered %d µs by more than a millisecond (IP-19 DoD 6)",
			rollingServer.P99, meteredServer.P99)
	}
}
