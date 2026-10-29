package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// The chaos rows of testing-strategy.md section 6 that a proxy or a store
// command can inject in process (IP-16 D7), so they run under -race on every
// pull request rather than nightly. The rows that need the host or a
// container's disk are in test/chaos.

// TestChaosOneBalanceHashDeleted: a DEL of one tenant-feature's hash
// mid-cycle is a 503 for that pair alone. The tenant's other feature and
// another tenant on the same feature are untouched, the pair is never
// re-granted however often it is asked, and the loss is counted (DR-045).
func TestChaosOneBalanceHashDeleted(t *testing.T) {
	s := newStack(t)
	f, g := uniq("delf"), uniq("delg")
	a := s.tenant(externalID("dela"), "UTC", monthlyAnchor, ent{f, 100, "monthly", false}, ent{g, 100, "monthly", false})
	b := s.tenant(externalID("delb"), "UTC", monthlyAnchor, ent{f, 100, "monthly", false})
	for _, p := range []struct {
		tn tenant
		f  string
	}{{a, f}, {a, g}, {b, f}} {
		s.seedCurrent(p.tn, p.f, 100)
		mustStatus(t, s.consume(s.runtime, p.f, 30, p.tn.External, newKey()), http.StatusOK)
	}
	if err := s.rdb.Del(context.Background(), s.balanceKey(a, f)).Err(); err != nil {
		t.Fatal(err)
	}
	before := s.counter("quotacore_balance_key_missing_total")
	for range 20 {
		if r := s.consume(s.runtime, f, 1, a.External, newKey()); r.Status != 503 || r.code(t) != "service_unavailable" {
			t.Fatalf("the deleted pair answered %d %s, want 503 service_unavailable", r.Status, r.Body)
		}
	}
	if got := s.counter("quotacore_balance_key_missing_total") - before; got != 20 {
		t.Errorf("the loss was counted %v times for 20 requests", got)
	}
	if n, _ := s.rdb.Exists(context.Background(), s.balanceKey(a, f)).Result(); n != 0 {
		t.Error("the deleted hash was re-created, which is a free allowance")
	}
	mustStatus(t, s.consume(s.runtime, g, 1, a.External, newKey()), http.StatusOK)
	mustStatus(t, s.consume(s.runtime, f, 1, b.External, newKey()), http.StatusOK)
	if got := s.balance(a, g); got != 69 {
		t.Errorf("the tenant's other feature holds %d, want 69", got)
	}
	if got := s.balance(b, f); got != 69 {
		t.Errorf("the other tenant holds %d, want 69", got)
	}
}

// TestChaosStoreLatency: with the data store delayed by 400 ms and by 2 s,
// every request fails closed within the 250 ms budget measured by the server,
// nothing piles up behind the delay, so the first request after it is fast, and once the delay is gone each request
// retried with its key settles to exactly one deduction: the delayed command
// may have run after its request gave up, and the retry finds that out rather
// than charging twice. The timeout is not a suggestion.
func TestChaosStoreLatency(t *testing.T) {
	for _, delay := range []time.Duration{400 * time.Millisecond, 2 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			_, redisURL := integrationURLs(t)
			p := newProxy(t, hostOf(t, redisURL))
			s := newStack(t, func(o *stackOptions) { o.redisURL = urlVia(t, redisURL, p.addr()) })
			f := uniq("lat")
			tn := s.tenant(externalID("lat"), "UTC", monthlyAnchor, ent{f, 1000, "monthly", false})
			s.seedCurrent(tn, f, 1000)
			mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)

			p.delay.Store(int64(delay))
			// Ten at once: enough that a queue behind the delay would show, and
			// few enough that a -race build on a shared runner does not add its
			// own wait to the server's figure.
			const n = 10
			keys := make([]string, n)
			results := make([]response, n)
			took := make([]time.Duration, n)
			var wg sync.WaitGroup
			for i := range n {
				keys[i] = newKey()
				wg.Add(1)
				go func() {
					defer wg.Done()
					start := time.Now()
					results[i] = s.consume(s.runtime, f, 1, tn.External, keys[i])
					took[i] = time.Since(start)
				}()
			}
			wg.Wait()
			for i, r := range results {
				if r.Status != 503 || r.code(t) != "service_unavailable" {
					t.Errorf("request %d under a %v delay answered %d %s, want 503", i, delay, r.Status, r.Body)
					continue
				}
				if d := s.logDuration(t, r.Header.Get("X-Request-Id")); d > 260*time.Millisecond {
					t.Errorf("request %d took %v at the server, past the 250 ms budget", i, d)
				}
				if took[i] > time.Second {
					t.Errorf("request %d took %v at the client", i, took[i])
				}
			}

			p.delay.Store(0)
			// Commands still held in the proxy land within one delay.
			time.Sleep(delay + 200*time.Millisecond)
			for i := range n {
				var r response
				for range 20 {
					if r = s.consume(s.runtime, f, 1, tn.External, keys[i]); r.Status == 200 {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
				if r.Status != 200 {
					t.Fatalf("the retry of request %d answered %d %s", i, r.Status, r.Body)
				}
			}
			if got := s.balance(tn, f); got != 999-n {
				t.Errorf("after the retries the balance is %d, want exactly one deduction each, %d", got, 999-n)
			}
			// Nothing queued behind the delay: a request now is answered at
			// the speed of an undelayed store.
			for i := range 20 {
				start := time.Now()
				mustStatus(t, s.check(f, tn.External), http.StatusOK)
				if d := time.Since(start); d > 100*time.Millisecond {
					t.Errorf("check %d after the delay took %v", i, d)
				}
			}
		})
	}
}

// propagation suspends a cached tenant through one instance and measures how
// long the other takes to refuse it, then resumes it and measures the same.
func propagation(t *testing.T, writer, reader *stack, feature string, tn tenant, trials int) []time.Duration {
	t.Helper()
	tp := "/v1/admin/tenants/" + tn.ID
	var lags []time.Duration
	for i := range trials {
		action, want := "suspend", "tenant_suspended"
		if i%2 == 1 {
			action, want = "resume", ""
		}
		mustStatus(t, writer.adminDo(http.MethodPost, tp+"/"+action, fmt.Sprintf(`{"version":%d,"reason":"q08"}`, writer.version(tp))), http.StatusOK)
		start := time.Now()
		for {
			// The writer's key: both instances read one key table.
			r := reader.do(http.MethodPost, "/v1/check", writer.runtime, metered(feature, 1, tn.External))
			if (want == "" && r.Status == 200) || (want != "" && r.Status != 200 && r.code(t) == want) {
				break
			}
			if time.Since(start) > 10*time.Second {
				t.Fatalf("trial %d: the other instance did not see the %s within 10 s: %d %s", i, action, r.Status, r.Body)
			}
			time.Sleep(time.Millisecond)
		}
		lags = append(lags, time.Since(start))
	}
	return lags
}

func quantile(d []time.Duration, q float64) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[min(len(s)-1, max(0, int(q*float64(len(s))+0.5)-1))]
}

// TestQ08ConfigurationPropagation is NFR-D3 measured, which closes Q-08: two
// instances on one control plane and one data store, a suspend and a resume
// written through one, and the time until the other enforces it. The
// distribution is written for the evidence document.
func TestQ08ConfigurationPropagation(t *testing.T) {
	s := newStack(t)
	other := newStack(t, func(o *stackOptions) { o.dbURL = s.dbURL })
	f := uniq("q08")
	s.createFeature(f)
	plan := uniq("q08p")
	s.createPlan(plan, 1000, "monthly", f)
	tn := asTenant(t, s.createTenant(externalID("q08"), plan, "UTC"), ent{f, 1000, "monthly", false})
	mustStatus(t, other.do(http.MethodPost, "/v1/check", s.runtime, metered(f, 1, tn.External)), http.StatusOK)
	lags := propagation(t, s, other, f, tn, 200)
	p50, p99, worst := quantile(lags, 0.5), quantile(lags, 0.99), slices.Max(lags)
	t.Logf("NFR-D3 over %d changes: p50 %v, p99 %v, max %v", len(lags), p50, p99, worst)
	if dir := os.Getenv("QUOTACORE_EVIDENCE_DIR"); dir != "" {
		raw, _ := json.MarshalIndent(map[string]any{"changes": len(lags), "p50": p50.String(), "p99": p99.String(), "max": worst.String()}, "", "  ")
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, t.Name()+".json"), raw, 0o644)
	}
	if p99 > time.Second {
		t.Errorf("NFR-D3: p99 propagation %v, above a second", p99)
	}
}

// TestChaosPubSubUnavailable: with every subscriber connection muted, so the
// second instance believes it is listening and hears nothing, a change still
// reaches it within the refresh interval (a second in these tests) plus the
// time a check takes, and is enforced there correctly: propagation widens,
// correctness holds.
func TestChaosPubSubUnavailable(t *testing.T) {
	_, redisURL := integrationURLs(t)
	p := newProxy(t, hostOf(t, redisURL))
	s := newStack(t)
	other := newStack(t, func(o *stackOptions) { o.dbURL = s.dbURL; o.redisURL = urlVia(t, redisURL, p.addr()) })
	f := uniq("ps")
	s.createFeature(f)
	plan := uniq("psp")
	s.createPlan(plan, 1000, "monthly", f)
	tn := asTenant(t, s.createTenant(externalID("ps"), plan, "UTC"), ent{f, 1000, "monthly", false})
	mustStatus(t, other.do(http.MethodPost, "/v1/check", s.runtime, metered(f, 1, tn.External)), http.StatusOK)

	p.muteSubscribers.Store(true)
	lags := propagation(t, s, other, f, tn, 10)
	worst := slices.Max(lags)
	t.Logf("with pub/sub muted, propagation took at most %v over %d changes", worst, len(lags))
	if worst > 2500*time.Millisecond {
		t.Errorf("with pub/sub muted a change took %v, past the one-second refresh interval's reach", worst)
	}
	// The tenant ends resumed after an even number of changes; it is
	// enforced, not just readable.
	mustStatus(t, other.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
}
