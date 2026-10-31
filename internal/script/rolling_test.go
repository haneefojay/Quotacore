package script

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/rolling"
	"github.com/quotacore/quotacore/internal/store"
)

// The three scripts a rolling-window entitlement runs (DR-052, IP-19), and the
// script half of named test T-18, "A rolling window holds under load".

func rollingBodies() []body { return []body{consumeRollingBody, checkRollingBody, refundRollingBody} }

const windowOpen, windowClose = "-- BEGIN window", "-- END window"

func block(text, open, close string) string {
	start, end := strings.Index(text, open), strings.Index(text, close)
	if start < 0 || end < start || strings.Count(text, open) != 1 || strings.Count(text, close) != 1 {
		return ""
	}
	return text[start : end+len(close)]
}

// TestTheRollingScriptsShareOneWindow: the arithmetic is one block written out
// three times, byte for byte; the two mutations hold consume.lua's
// idempotency block byte for byte; and none of the three holds the anchored
// transition block, so a rolling window can never roll a cycle (ADR-0008).
func TestTheRollingScriptsShareOneWindow(t *testing.T) {
	first := block(consumeRollingBody.text, windowOpen, windowClose)
	if first == "" {
		t.Fatal("consume_rolling.lua has no window block")
	}
	idem := block(consumeBody.text, idemOpen, idemClose)
	for _, b := range rollingBodies() {
		if got := block(b.text, windowOpen, windowClose); got != first {
			t.Errorf("%s.lua's window block differs from consume_rolling.lua's; the window is one rule (DR-052)", b.op)
		}
		if strings.Contains(b.text, blockOpen) {
			t.Errorf("%s.lua holds the anchored transition block; the two cadences share no rule (ADR-0008)", b.op)
		}
		if b.op == CheckRolling {
			if strings.Contains(b.text, idemOpen) || strings.Contains(b.text, "KEYS[2]") {
				t.Error("check_rolling.lua has an idempotency record; a check changes nothing (DR-026)")
			}
			continue
		}
		if got := block(b.text, idemOpen, idemClose); got == "" || got != idem {
			t.Errorf("%s.lua's idempotency block differs from consume.lua's (DR-027, DR-028)", b.op)
		}
		lookup := strings.Index(b.text, "redis.call('GET', KEYS[2])")
		write := strings.Index(b.text, "local w, why = window()")
		if lookup < 0 || write < 0 || lookup > write {
			t.Errorf("%s.lua reads the window before it looks up the record; a replay must not prune (DR-027)", b.op)
		}
	}
}

// TestTheRollingScriptsAreBounded is source_test.go's shape rules for the
// rolling scripts: flagged, literal commands from an allowed set, and a check
// that writes nothing. A rolling window is the one place a script iterates,
// over the fields of its own bucket hash, which settle() keeps to one window's
// sixty (IP-19 DoD 4); the loops are named here so a new one is a decision.
func TestTheRollingScriptsAreBounded(t *testing.T) {
	allowed := map[Operation]map[string]bool{
		ConsumeRolling: {"HGETALL": true, "HDEL": true, "HINCRBY": true, "PEXPIREAT": true, "GET": true, "SET": true},
		RefundRolling:  {"HGETALL": true, "HDEL": true, "HINCRBY": true, "PEXPIREAT": true, "GET": true, "SET": true},
		CheckRolling:   {"HGETALL": true},
	}
	loops := map[string]bool{
		"for i = 1, #raw, 2 do":                true, // the bucket hash, read once
		"for _, b in ipairs(w.counted) do":     true, // the refund, newest first
		"table.sort(w.counted, function(a, b)": true,
	}
	banned := []string{"while ", "repeat", "math.random", "os.", "dofile", "loadstring", "require", "KEYS[3]"}
	loop := regexp.MustCompile(`(for [^\n]*do|table\.sort\([^,]*, function\(a, b\))`)
	for _, b := range rollingBodies() {
		if !strings.HasPrefix(b.text, "#!lua\n") {
			t.Errorf("%s.lua is not flagged, so a full store would not refuse it whole (DR-048)", b.op)
		}
		code := stripLuaComments(b.text)
		for _, word := range banned {
			if strings.Contains(code, word) {
				t.Errorf("%s.lua contains %q", b.op, word)
			}
		}
		for _, m := range loop.FindAllString(code, -1) {
			if !loops[m] {
				t.Errorf("%s.lua iterates with %q, which is not one of the named loops", b.op, m)
			}
		}
		for _, m := range strings.Split(code, "redis.call('")[1:] {
			cmd, _, _ := strings.Cut(m, "'")
			if !allowed[b.op][cmd] {
				t.Errorf("%s.lua calls %s", b.op, cmd)
			}
		}
		if !strings.Contains(b.text, "local BOUND = "+strconv.FormatInt(bound, 10)) {
			t.Errorf("%s.lua does not declare the runner's bound", b.op)
		}
	}
	seen := map[Operation]bool{}
	for _, b := range allBodies() {
		seen[b.op] = true
	}
	for _, b := range rollingBodies() {
		if !seen[b.op] {
			t.Errorf("Load does not put %s in the store", b.op)
		}
	}
}

// TestARollingCallIsOneCall is IP-19 DoD 6's round-trip count, taken in the
// client: a rolling consume, check and refund each make one evaluation, by
// digest, against the bucket hash and nothing else.
func TestARollingCallIsOneCall(t *testing.T) {
	w, _ := rolling.Parse("rolling_1h")
	reply := []any{"applied", "current", "9", "10", "0", "1", "1000", "3601000", ""}
	for _, c := range []struct {
		name string
		run  func(*Runner) (RollingOutcome, error)
	}{
		{"consume", func(r *Runner) (RollingOutcome, error) {
			return r.ConsumeRolling(context.Background(), RollingRequest{TenantID: testTenant, FeatureKey: "api.calls",
				IdempotencyKey: "a-client-key", Amount: 1, Limit: 10, Window: w, At: time.UnixMilli(1000)})
		}},
		{"check", func(r *Runner) (RollingOutcome, error) {
			return r.CheckRolling(context.Background(), RollingRequest{TenantID: testTenant, FeatureKey: "api.calls",
				Amount: 1, Limit: 10, Window: w, At: time.UnixMilli(1000)})
		}},
		{"refund", func(r *Runner) (RollingOutcome, error) {
			return r.RefundRolling(context.Background(), RollingRequest{TenantID: testTenant, FeatureKey: "api.calls",
				IdempotencyKey: "a-client-key", Amount: 1, Limit: 10, Window: w, At: time.UnixMilli(1000)})
		}},
	} {
		fake := &fakeStore{reply: reply}
		out, err := c.run(newRunner(t, fake, 250*time.Millisecond))
		if err != nil || out.Decision != Applied || out.Remaining != 9 || out.Sum != 1 || out.NextRelease == nil {
			t.Fatalf("%s: %+v, %v", c.name, out, err)
		}
		evals, loads, bodies := fake.counts()
		if evals != 1 || loads != 0 || bodies != 0 {
			t.Errorf("a rolling %s made %d evaluations, %d loads and %d bodies; the rule is one round trip by digest",
				c.name, evals, loads, bodies)
		}
	}
	fake := &fakeStore{reply: reply}
	r := newRunner(t, fake, 250*time.Millisecond)
	if _, err := r.ConsumeRolling(context.Background(), RollingRequest{TenantID: testTenant, FeatureKey: "api.calls",
		Amount: 1, Limit: 10, Window: w, At: time.UnixMilli(1000)}); !errors.Is(err, ErrMissingIdempotencyKey) {
		t.Errorf("a consume without a key: %v", err)
	}
	if _, err := r.CheckRolling(context.Background(), RollingRequest{TenantID: testTenant, FeatureKey: "api.calls",
		Amount: 1, Limit: 10, At: time.UnixMilli(1000)}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a check with no window: %v", err)
	}
	if evals, _, _ := fake.counts(); evals != 0 {
		t.Errorf("a refused rolling call reached the store %d times", evals)
	}
}

// rollingFixture is a fresh feature on the real store: its bucket key, cleared,
// and a request template.
func rollingFixture(t *testing.T, window string, limit int64) (*store.Store, *Runner, string, RollingRequest) {
	t.Helper()
	s, runner := integrationRunner(t)
	feature := featureKey(t)
	key, err := store.BucketCounterKey(testTenant, feature)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Client().Del(context.Background(), key).Err(); err != nil {
		t.Fatal(err)
	}
	w, ok := rolling.Parse(window)
	if !ok {
		t.Fatalf("no window %s", window)
	}
	return s, runner, key, RollingRequest{TenantID: testTenant, FeatureKey: feature, Limit: limit, Window: w}
}

// windowSum reads the bucket hash directly and sums what the window counts at
// an instant, which is the ground truth the scripts' answers are compared to.
func windowSum(t *testing.T, s *store.Store, key string, w rolling.Window, at time.Time) (sum int64, fields int) {
	t.Helper()
	all, err := s.Client().HGetAll(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	b := w.Precision().Milliseconds()
	now := at.UnixMilli()
	horizon := now - now%b - w.Duration().Milliseconds()
	for k, v := range all {
		start, _ := strconv.ParseInt(k, 10, 64)
		units, _ := strconv.ParseInt(v, 10, 64)
		if start > horizon {
			sum += units
		}
	}
	return sum, len(all)
}

// TestT18TwoHundredAgainstAHundred is IP-19 DoD 1: 200 concurrent consumes of
// 1 against a rolling allowance of 100 succeed exactly 100 times, and the
// window sum, read from the store while they run, is never above 100 and is
// 100 at the end.
func TestT18TwoHundredAgainstAHundred(t *testing.T) {
	s, runner, key, req := rollingFixture(t, "rolling_1h", 100)
	at := time.Now()
	req.At = at
	var applied, denied atomic.Int64
	var over atomic.Int64
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if sum, _ := windowSum(t, s, key, req.Window, at); sum > 100 {
				over.Store(sum)
			}
		}
	}()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := req
			r.Amount = 1
			r.IdempotencyKey = "t18-" + strconv.Itoa(i) + "-" + uuid.NewString()
			out, err := runner.ConsumeRolling(context.Background(), r)
			switch {
			case err != nil:
				t.Error(err)
			case out.Decision == Applied:
				applied.Add(1)
				if out.Sum > 100 || out.Remaining != 100-out.Sum {
					t.Errorf("an applied consume reported sum %d and remaining %d", out.Sum, out.Remaining)
				}
			case out.Decision == Denied:
				denied.Add(1)
			default:
				t.Errorf("decision %s", out.Decision)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(stop)
	<-watched
	if applied.Load() != 100 || denied.Load() != 100 {
		t.Errorf("applied %d and denied %d of 200 against 100; want exactly 100 each (IP-19 DoD 1)", applied.Load(), denied.Load())
	}
	if n := over.Load(); n != 0 {
		t.Errorf("the window sum was %d at an instant checked; it must never pass the limit", n)
	}
	if sum, _ := windowSum(t, s, key, req.Window, at); sum != 100 {
		t.Errorf("the window sum is %d after the run, want 100", sum)
	}
	check := req
	check.Amount = 0
	if out, err := runner.CheckRolling(context.Background(), check); err != nil || out.Sum != 100 || out.Remaining != 0 {
		t.Errorf("check after the run: %+v, %v", out, err)
	}
}

// TestT18TheWindowSlides is UC-17's acceptance as amended: rolling_5m, limit
// 10, 8 consumed; 3 more four minutes later is denied, because 8 + 3 is above
// 10, and allowed once the 8 have left the window, which is within one bucket
// of five minutes after they were consumed (DR-052).
func TestT18TheWindowSlides(t *testing.T) {
	_, runner, _, req := rollingFixture(t, "rolling_5m", 10)
	ctx := context.Background()
	b := req.Window.Precision()
	t0 := time.Now().Truncate(b).Add(b / 2) // the middle of a bucket
	consume := func(at time.Time, n int64) RollingOutcome {
		t.Helper()
		r := req
		r.At, r.Amount, r.IdempotencyKey = at, n, "slide-"+uuid.NewString()
		out, err := runner.ConsumeRolling(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := consume(t0, 8); out.Decision != Applied || out.Remaining != 2 {
		t.Fatalf("8 at t0: %+v", out)
	}
	if out := consume(t0.Add(4*time.Minute), 3); out.Decision != Denied || out.Sum != 8 || out.Remaining != 2 {
		t.Errorf("3 four minutes later: %+v, want denied with 8 counted", out)
	} else if out.NextRelease == nil || !out.NextRelease.Equal(t0.Truncate(b).Add(5*time.Minute)) {
		t.Errorf("next release %v, want the 8's bucket plus five minutes", out.NextRelease)
	}
	// A unit counts for at least the window less one bucket and at most the
	// window: just before W - b it still counts, and from W on it never does.
	if out := consume(t0.Add(5*time.Minute-b-b/2-time.Millisecond), 3); out.Decision != Denied {
		t.Errorf("before W - b the 8 must still count: %+v", out)
	}
	if out := consume(t0.Add(5*time.Minute), 3); out.Decision != Applied || out.Sum != 3 {
		t.Errorf("five minutes later the 8 are out of the window: %+v", out)
	}
}

// TestT18TheKeyIsBounded is IP-19 DoD 4: one consume per bucket across three
// windows of virtual time leaves exactly one key for the tenant-feature, never
// more than one window's sixty buckets in it, and an expiry at its newest
// bucket plus the window, so it cannot outlive its usefulness.
func TestT18TheKeyIsBounded(t *testing.T) {
	s, runner, key, req := rollingFixture(t, "rolling_5m", 1000)
	ctx := context.Background()
	b := req.Window.Precision()
	marker, err := store.WindowMarkerKey(testTenant, req.FeatureKey)
	if err != nil {
		t.Fatal(err)
	}
	balance, err := store.BalanceKey(testTenant, req.FeatureKey)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Truncate(b)
	most := 0
	for i := range 3 * rolling.Buckets {
		at := base.Add(time.Duration(i) * b)
		r := req
		r.At, r.Amount, r.IdempotencyKey = at, 1, "bound-"+uuid.NewString()
		out, err := runner.ConsumeRolling(ctx, r)
		if err != nil || out.Decision != Applied {
			t.Fatalf("consume %d: %+v, %v", i, out, err)
		}
		sum, fields := windowSum(t, s, key, req.Window, at)
		most = max(most, fields)
		if fields > rolling.Buckets {
			t.Fatalf("after consume %d the key holds %d buckets; a window has %d", i, fields, rolling.Buckets)
		}
		if want := int64(min(i+1, rolling.Buckets)); sum != want || out.Sum != want {
			t.Fatalf("after consume %d the window sum is %d (script %d), want %d", i, sum, out.Sum, want)
		}
		expires, err := s.Client().PExpireTime(ctx, key).Result()
		if err != nil {
			t.Fatal(err)
		}
		if want := at.Add(req.Window.Duration()).UnixMilli(); expires.Milliseconds() != want {
			t.Fatalf("after consume %d the key expires at %d, want %d (newest bucket plus the window)", i, expires.Milliseconds(), want)
		}
	}
	if most != rolling.Buckets {
		t.Errorf("the key never held a full window of buckets (most %d)", most)
	}
	n, err := s.Client().Exists(ctx, key, marker, balance).Result()
	if err != nil || n != 1 {
		t.Errorf("%d keys exist for the tenant-feature, want exactly the bucket hash: %v", n, err)
	}
}

// TestT18ClockSkewIsBoundedAndVisible is IP-19 DoD 5. A writer whose clock is
// two seconds fast puts its units in a bucket ahead of a slow writer's now; the
// slow writer counts them and does not prune them, so a skew never quietly
// lowers the sum. The fast writer's own prune drops buckets early by at most
// the skew, which the clock-skew alert bounds (NFR-D8, Q-26).
func TestT18ClockSkewIsBoundedAndVisible(t *testing.T) {
	s, runner, key, req := rollingFixture(t, "rolling_5m", 10)
	ctx := context.Background()
	b := req.Window.Precision()
	now := time.Now().Truncate(b).Add(b - time.Second) // a second before a bucket boundary
	fast := now.Add(2 * time.Second)                   // two seconds ahead, in the next bucket
	r := req
	r.At, r.Amount, r.IdempotencyKey = fast, 4, "skew-fast-"+uuid.NewString()
	if out, err := runner.ConsumeRolling(ctx, r); err != nil || out.Decision != Applied {
		t.Fatalf("fast writer: %+v, %v", out, err)
	}
	r = req
	r.At, r.Amount, r.IdempotencyKey = now, 4, "skew-slow-"+uuid.NewString()
	out, err := runner.ConsumeRolling(ctx, r)
	if err != nil || out.Decision != Applied || out.Sum != 8 {
		t.Fatalf("slow writer: %+v, %v; it must count the bucket ahead of its clock", out, err)
	}
	if _, fields := windowSum(t, s, key, req.Window, now); fields != 2 {
		t.Errorf("the slow writer pruned a bucket ahead of it: %d buckets left", fields)
	}
	// Past the window on the fast clock but not on the slow one: the fast
	// writer counts only what its own clock places in the window, an error of
	// one bucket at a skew of under one bucket.
	check := req
	check.At, check.Amount = now.Add(5*time.Minute+b/2), 0
	slowView, err := runner.CheckRolling(ctx, check)
	if err != nil {
		t.Fatal(err)
	}
	check.At = fast.Add(5 * time.Minute)
	fastView, err := runner.CheckRolling(ctx, check)
	if err != nil {
		t.Fatal(err)
	}
	if diff := slowView.Sum - fastView.Sum; diff < 0 || diff > 4 {
		t.Errorf("slow view %d and fast view %d differ by more than the one bucket the skew spans", slowView.Sum, fastView.Sum)
	}
}

// TestT18RefundAndReplay: a refund comes out of the newest buckets first and is
// refused whole when the window holds less than it; a retried consume replays
// what the first attempt was told and a reused key is refused (DR-019, DR-027,
// DR-028).
func TestT18RefundAndReplay(t *testing.T) {
	s, runner, key, req := rollingFixture(t, "rolling_5m", 10)
	ctx := context.Background()
	b := req.Window.Precision()
	t0 := time.Now().Truncate(b)
	c1 := req
	c1.At, c1.Amount, c1.IdempotencyKey = t0, 3, "rr-"+uuid.NewString()
	first, err := runner.ConsumeRolling(ctx, c1)
	if err != nil || first.Decision != Applied {
		t.Fatal(first, err)
	}
	c2 := req
	c2.At, c2.Amount, c2.IdempotencyKey = t0.Add(b), 4, "rr-"+uuid.NewString()
	if out, err := runner.ConsumeRolling(ctx, c2); err != nil || out.Sum != 7 {
		t.Fatal(out, err)
	}
	again := c1
	again.At = t0.Add(2 * b)
	if out, err := runner.ConsumeRolling(ctx, again); err != nil || out.Decision != Replayed || out.Sum != first.Sum ||
		out.Remaining != first.Remaining {
		t.Errorf("retry: %+v, %v; want the first attempt's answer", out, err)
	}
	reuse := c1
	reuse.Amount = 1
	if _, err := runner.ConsumeRolling(ctx, reuse); !errors.Is(err, ErrIdempotencyKeyReuse) {
		t.Errorf("same key, other amount: %v", err)
	}
	over := req
	over.At, over.Amount, over.IdempotencyKey = t0.Add(2*b), 8, "rr-"+uuid.NewString()
	if out, err := runner.RefundRolling(ctx, over); err != nil || out.Decision != RefusedCeiling || out.Sum != 7 {
		t.Errorf("refund 8 of 7: %+v, %v", out, err)
	}
	back := req
	back.At, back.Amount, back.IdempotencyKey = t0.Add(2*b), 5, "rr-"+uuid.NewString()
	out, err := runner.RefundRolling(ctx, back)
	if err != nil || out.Decision != Applied || out.Sum != 2 || out.Remaining != 8 {
		t.Fatalf("refund 5: %+v, %v", out, err)
	}
	all, err := s.Client().HGetAll(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	// The newer bucket's 4 went first, then 1 of the older bucket's 3.
	if len(all) != 1 || all[strconv.FormatInt(t0.UnixMilli(), 10)] != "2" {
		t.Errorf("after the refund the buckets are %v, want the older bucket with 2", all)
	}
}

// TestRollingCostsAboutWhatMeteredCosts is the live half of IP-19 DoD 6: a
// rolling consume over a full window of buckets is within a small factor of
// a metered consume's p99 on the same store, so the sustained run in the load
// suite has no new cost to absorb. The load suite measures NFR-T1 itself.
func TestRollingCostsAboutWhatMeteredCosts(t *testing.T) {
	s, runner, _, req := rollingFixture(t, "rolling_5m", bound)
	ctx := context.Background()
	const n = 1000
	b := req.Window.Precision()
	base := time.Now().Truncate(b)
	// A full window: one unit in each of the sixty buckets, which is the most
	// a window ever reads.
	for i := range rolling.Buckets {
		r := req
		r.At, r.Amount, r.IdempotencyKey = base.Add(time.Duration(i)*b), 1, "fill-"+uuid.NewString()
		if _, err := runner.ConsumeRolling(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	at := base.Add(time.Duration(rolling.Buckets-1) * b)
	feature := featureKey(t) + "-metered"
	w := fixedWindow(0, at.Add(-time.Hour), 24*time.Hour)
	seed(t, s, keyFor(t, feature), state{Balance: bound, Limit: bound, Index: 0, Start: w.Start, End: w.End})
	measure := func() (rp, mp time.Duration) {
		var rs, ms []time.Duration
		for i := range n {
			k := "cost-" + strconv.Itoa(i) + "-" + uuid.NewString()
			start := time.Now()
			r := req
			r.At, r.Amount, r.IdempotencyKey = at, 1, k+"-r"
			if _, err := runner.ConsumeRolling(ctx, r); err != nil {
				t.Fatal(err)
			}
			rs = append(rs, time.Since(start))
			start = time.Now()
			if _, err := runner.Consume(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, Limit: bound,
				Window: w, Interval: cycle.Daily, At: at, IdempotencyKey: k + "-m"}); err != nil {
				t.Fatal(err)
			}
			ms = append(ms, time.Since(start))
		}
		p99 := func(d []time.Duration) time.Duration { slices.Sort(d); return d[n*99/100] }
		return p99(rs), p99(ms)
	}
	for attempt := 1; ; attempt++ {
		rp, mp := measure()
		t.Logf("attempt %d: consume p99 rolling %v, metered %v", attempt, rp, mp)
		if rp <= 3*mp || rp <= mp+time.Millisecond {
			return
		}
		if attempt == 3 {
			t.Errorf("a rolling consume's p99 is more than three times a metered one's in three measurements (IP-19 DoD 6)")
			return
		}
	}
}
