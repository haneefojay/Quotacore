package main

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/auth"
)

var monthlyAnchor = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestT01TwoHundredConsumesOverHTTP (IP-07 DoD 1, T-01): 200 concurrent
// consumes of 1 against a balance of 100 produce exactly 100 200s and 100
// quota_exceeded, a final balance of 0, and every 429 reports a balance of 0.
// Repeated at 1, 2, 4, 8, 16 and 32 client connections.
func TestT01TwoHundredConsumesOverHTTP(t *testing.T) {
	s := newStack(t)
	for _, conns := range []int{1, 2, 4, 8, 16, 32} {
		t.Run(fmt.Sprintf("%d-connections", conns), func(t *testing.T) {
			feature := fmt.Sprintf("t01.c%d", conns)
			tn := s.tenant(externalID("t01"), "UTC", monthlyAnchor, ent{feature, 100, "monthly", false})
			s.seedCurrent(tn, feature, 100)
			client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: conns, MaxIdleConnsPerHost: conns}}
			defer client.CloseIdleConnections()
			s2 := *s
			s2.client = client
			s2.t = t

			var (
				wg       sync.WaitGroup
				mu       sync.Mutex
				ok, quot int
				bad      []string
			)
			for i := 0; i < 200; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					r := s2.consume(s.runtime, feature, 1, tn.External, fmt.Sprintf("t01-%d-%d-%s", conns, i, tn.ID[:8]))
					mu.Lock()
					defer mu.Unlock()
					switch {
					case r.Status == 200:
						ok++
					case r.Status == 429 && r.code(t) == "quota_exceeded":
						quot++
						if b := r.details(t)["balance"]; b != 0.0 {
							bad = append(bad, fmt.Sprintf("a 429 reported balance %v", b))
						}
					default:
						bad = append(bad, fmt.Sprintf("%d %s", r.Status, r.Body))
					}
				}(i)
			}
			wg.Wait()
			if ok != 100 || quot != 100 {
				t.Errorf("%d allowed and %d quota_exceeded, want 100 and 100", ok, quot)
			}
			if len(bad) > 0 {
				t.Errorf("%d unexpected answers, first: %s", len(bad), bad[0])
			}
			if b := s.balance(tn, feature); b != 0 {
				t.Errorf("final balance = %d, want 0", b)
			}
		})
	}
}

// TestCheckChangesNothingInsideACycle (IP-07 DoD 2): a check, allowed or
// denied, moves none of the six fields, the expiry, the idempotency records or
// the events handed to the ledger.
func TestCheckChangesNothingInsideACycle(t *testing.T) {
	s := newStack(t)
	tn := s.tenant(externalID("chk"), "Europe/Berlin", monthlyAnchor, ent{"chk.f", 1000, "monthly", false})
	s.seedCurrent(tn, "chk.f", 500)
	before := s.state(tn, "chk.f")
	for _, amount := range []int64{1, 500, 501, 100000} {
		r := s.do(http.MethodPost, "/v1/check", s.runtime, metered("chk.f", amount, tn.External))
		if r.Status != 200 {
			t.Fatalf("check %d = %d %s", amount, r.Status, r.Body)
		}
		if allowed := r.json(t)["allowed"]; allowed != (amount <= 500) {
			t.Errorf("check %d: allowed = %v", amount, allowed)
		}
		if after := s.state(tn, "chk.f"); !reflect.DeepEqual(before, after) {
			t.Errorf("check %d changed the state:\nbefore %+v\nafter  %+v", amount, before, after)
		}
	}
}

// TestCheckAcrossABoundaryOnlyRolls (IP-07 DoD 2 as amended, C6, and IP-12
// D3): a check after the stored window has closed applies the transition any
// request would, so the balance it reports is the new cycle's, and nothing
// else: no deduction, no record, and one event, the cycle_rolled_over that
// opens the new cycle.
func TestCheckAcrossABoundaryOnlyRolls(t *testing.T) {
	s := newStack(t)
	tn := s.tenant(externalID("roll"), "UTC", monthlyAnchor, ent{"roll.f", 1000, "monthly", false})
	cur := s.currentWindow(tn, "roll.f", time.Now())
	prevEnd := cur.Start
	s.seed(tn, "roll.f", stored{Balance: 3, Limit: 1000, Bonus: 7, Index: cur.Index - 1,
		Start: prevEnd.AddDate(0, -1, 0), End: &prevEnd})
	events := s.events.count()
	r := s.do(http.MethodPost, "/v1/check", s.runtime, metered("roll.f", 10, tn.External))
	if r.Status != 200 || r.json(t)["allowed"] != true || r.json(t)["balance"] != 1000.0 || r.json(t)["bonus"] != 0.0 {
		t.Fatalf("check across a boundary = %d %s, want allowed against the new cycle's 1000", r.Status, r.Body)
	}
	after := s.state(tn, "roll.f")
	want := map[string]string{"balance": "1000", "limit": "1000", "bonus": "0",
		"cycle_index": fmt.Sprint(cur.Index), "window_start": fmt.Sprint(cur.Start.UnixMilli()),
		"window_end": fmt.Sprint(cur.End.UnixMilli())}
	if !reflect.DeepEqual(after.Hash, want) {
		t.Errorf("after the rolling check the hash is %v, want the transition only, %v", after.Hash, want)
	}
	if len(after.Records) != 0 {
		t.Errorf("a check wrote %d records", len(after.Records))
	}
	if got := s.events.since(events); len(got) != 1 || got[0].Type != "cycle_rolled_over" {
		t.Fatalf("a rolling check handed the sink %+v, want one cycle_rolled_over", got)
	}
	rows := s.ledger(tn)
	if len(rows) != 1 {
		t.Fatalf("the ledger holds %+v, want the one rollover", rows)
	}
	row := rows[0]
	if row.Type != "cycle_rolled_over" || row.Source != "system" || row.Delta != 997 || row.Balance != 1000 ||
		row.Index != cur.Index || !row.Start.Equal(cur.Start) || row.RequestID == "" || row.RequestID != r.Header.Get("X-Request-Id") {
		t.Errorf("the rollover row = %+v, want system, +997 to 1000, index %d from %s, under the check's request id %s",
			row, cur.Index, cur.Start, r.Header.Get("X-Request-Id"))
	}
}

// TestBalanceRendersTheCycleInTheTenantsZone (IP-07 DoD 3): cycle_start and
// cycle_end are RFC 3339 with the tenant's offset, in four zones, including a
// Berlin month whose two ends have different offsets, and a never
// entitlement's two fields are null together (INV-C6).
func TestBalanceRendersTheCycleInTheTenantsZone(t *testing.T) {
	s := newStack(t)
	for _, c := range []struct {
		zone       string
		start, end string
	}{
		{"UTC", "2026-03-01T00:00:00Z", "2026-04-01T00:00:00Z"},
		{"Europe/Berlin", "2026-03-01T00:00:00+01:00", "2026-04-01T00:00:00+02:00"},
		{"America/New_York", "2026-03-01T00:00:00-05:00", "2026-04-01T00:00:00-04:00"},
		{"Asia/Kolkata", "2026-03-01T00:00:00+05:30", "2026-04-01T00:00:00+05:30"},
	} {
		loc, _ := time.LoadLocation(c.zone)
		start := time.Date(2026, 3, 1, 0, 0, 0, 0, loc)
		end := time.Date(2026, 4, 1, 0, 0, 0, 0, loc)
		tn := s.tenant(externalID("zone"), c.zone, start, ent{"zone.m", 100, "monthly", false}, ent{"zone.n", 5, "never", false})
		s.seed(tn, "zone.m", stored{Balance: 60, Limit: 100, Bonus: 10, Index: 0, Start: start, End: &end})
		s.seed(tn, "zone.n", stored{Balance: 5, Limit: 5, Start: start})
		r := s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External, s.runtime, "")
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", c.zone, r.Status, r.Body)
		}
		body := string(r.Body)
		for _, want := range []string{
			`"cycle_start":"` + c.start + `"`, `"cycle_end":"` + c.end + `"`, `"consumed":50`,
			`"feature_key":"zone.n","balance":5,"limit":5,"bonus":0,"consumed":0,"cycle_start":null,"cycle_end":null`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: body %s lacks %s", c.zone, body, want)
			}
		}
	}
}

// TestQuotaExceededAndRateLimitedAreDistinct (IP-07 DoD 4, DR-040).
func TestQuotaExceededAndRateLimitedAreDistinct(t *testing.T) {
	s := newStack(t, func(o *stackOptions) {
		o.env["QUOTACORE_RATELIMIT_RUNTIME_RPS"] = "0.5"
		o.env["QUOTACORE_RATELIMIT_RUNTIME_BURST"] = "2"
	})
	tn := s.tenant(externalID("dist"), "UTC", monthlyAnchor, ent{"dist.a", 10, "monthly", false}, ent{"dist.b", 10, "monthly", false})
	s.seedCurrent(tn, "dist.a", 0)
	s.seedCurrent(tn, "dist.b", 10)

	quota := s.consume(s.admin, "dist.a", 1, tn.External, newKey())
	if quota.Status != 429 || quota.code(t) != "quota_exceeded" {
		t.Fatalf("an empty balance = %d %s", quota.Status, quota.Body)
	}
	d := quota.details(t)
	if d["cycle_end"] == nil || quota.Header.Get("Retry-After") != "" || strings.Contains(string(quota.Body), "retry_after_ms") {
		t.Errorf("quota_exceeded must carry cycle_end and neither Retry-After nor retry_after_ms: %v %s", quota.Header, quota.Body)
	}

	for i := 0; i < 2; i++ {
		if r := s.consume(s.runtime, "dist.b", 1, tn.External, newKey()); r.Status != 200 {
			t.Fatalf("inside the burst: %d %s", r.Status, r.Body)
		}
	}
	before := s.state(tn, "dist.b")
	limited := s.consume(s.runtime, "dist.b", 1, tn.External, newKey())
	if limited.Status != 429 || limited.code(t) != "rate_limited" {
		t.Fatalf("past the burst = %d %s", limited.Status, limited.Body)
	}
	if limited.Header.Get("Retry-After") == "" || !strings.Contains(string(limited.Body), "retry_after_ms") ||
		limited.details(t)["cycle_end"] != nil {
		t.Errorf("rate_limited must carry Retry-After and retry_after_ms and no cycle_end: %v %s", limited.Header, limited.Body)
	}
	if after := s.state(tn, "dist.b"); !reflect.DeepEqual(before, after) {
		t.Errorf("rate_limited changed the state: %+v → %+v", before, after)
	}
}

// TestAnAdminKeyWorksOnTheDataPlane (IP-07 DoD 8, NFR-S4).
func TestAnAdminKeyWorksOnTheDataPlane(t *testing.T) {
	s := newStack(t)
	tn := s.tenant(externalID("adm"), "UTC", monthlyAnchor, ent{"adm.f", 100, "monthly", false})
	s.seedCurrent(tn, "adm.f", 100)
	for _, r := range []response{
		s.consume(s.admin, "adm.f", 1, tn.External, newKey()),
		s.do(http.MethodPost, "/v1/refund", s.admin, metered("adm.f", 1, tn.External), "Idempotency-Key", newKey()),
		s.do(http.MethodPost, "/v1/check", s.admin, metered("adm.f", 1, tn.External)),
		s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External, s.admin, ""),
	} {
		if r.Status != 200 {
			t.Errorf("an admin key on the data plane = %d %s", r.Status, r.Body)
		}
	}
	if r := s.do(http.MethodGet, "/v1/admin/keys", s.runtime, ""); r.Status != 403 || r.code(t) != "insufficient_scope" {
		t.Errorf("a runtime key on the control plane = %d %s", r.Status, r.Body)
	}
}

// TestAMissingBalanceIs503AndCounted (IP-05 DoD 6, DR-045): on all four
// routes a provisioned feature with no hash is 503 service_unavailable,
// counted, and never initialised.
func TestAMissingBalanceIs503AndCounted(t *testing.T) {
	s := newStack(t)
	tn := s.tenant(externalID("miss"), "UTC", monthlyAnchor, ent{"miss.f", 100, "monthly", false})
	for i, call := range []func() response{
		func() response { return s.consume(s.runtime, "miss.f", 1, tn.External, newKey()) },
		func() response {
			return s.do(http.MethodPost, "/v1/refund", s.runtime, metered("miss.f", 1, tn.External), "Idempotency-Key", newKey())
		},
		func() response {
			return s.do(http.MethodPost, "/v1/check", s.runtime, metered("miss.f", 1, tn.External))
		},
		func() response {
			return s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External, s.runtime, "")
		},
	} {
		r := call()
		if r.Status != 503 || r.code(t) != "service_unavailable" {
			t.Errorf("route %d: %d %s, want 503 service_unavailable", i, r.Status, r.Body)
		}
		if got := s.counter("quotacore_balance_key_missing_total"); got != float64(i+1) {
			t.Errorf("after route %d the counter is %v, want %d", i, got, i+1)
		}
	}
	if n, _ := s.rdb.Exists(context.Background(), s.balanceKey(tn, "miss.f")).Result(); n != 0 {
		t.Error("a missing balance was initialised")
	}
}

// TestT02FiftyDeliveriesOverHTTP (IP-06 DoD 1, T-02): fifty concurrent
// deliveries of one key make one deduction; forty-nine are replays with the
// header; the state values are identical in all fifty.
func TestT02FiftyDeliveriesOverHTTP(t *testing.T) {
	s := newStack(t)
	tn := s.tenant(externalID("t02"), "UTC", monthlyAnchor, ent{"t02.f", 1000, "monthly", false})
	s.seedCurrent(tn, "t02.f", 1000)
	key := newKey()
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		responses []response
	)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := s.consume(s.runtime, "t02.f", 30, tn.External, key)
			mu.Lock()
			responses = append(responses, r)
			mu.Unlock()
		}()
	}
	wg.Wait()
	replays := 0
	var first map[string]any
	for _, r := range responses {
		if r.Status != 200 {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		m := r.json(t)
		if m["replayed"] == true {
			replays++
			if r.Header.Get("Idempotent-Replay") != "true" {
				t.Error("a replay without Idempotent-Replay")
			}
		}
		delete(m, "replayed")
		if first == nil {
			first = m
		} else if !reflect.DeepEqual(first, m) {
			t.Errorf("state values differ between deliveries: %v vs %v", first, m)
		}
	}
	if replays != 49 {
		t.Errorf("%d replays, want 49", replays)
	}
	if b := s.balance(tn, "t02.f"); b != 970 {
		t.Errorf("balance = %d, want one deduction, 970", b)
	}
	if s.events.count() != 1 {
		t.Errorf("%d events handed to the ledger, want 1", s.events.count())
	}
}

// TestRevocationReachesTheCacheOffThePath: a key revoked in Postgres and
// announced on the invalidation channel is refused on the next request after
// the reconcile, without the request paying for a read.
func TestRevocationReachesTheCacheOffThePath(t *testing.T) {
	s := newStack(t)
	tn := s.tenant(externalID("rev"), "UTC", monthlyAnchor, ent{"rev.f", 100, "monthly", false})
	s.seedCurrent(tn, "rev.f", 100)
	tok := s.key(auth.Runtime)
	if r := s.do(http.MethodPost, "/v1/check", tok, metered("rev.f", 1, tn.External)); r.Status != 200 {
		t.Fatalf("before revocation: %d", r.Status)
	}
	prefix := strings.TrimPrefix(strings.Split(tok, ".")[0], "qc_runtime_")
	if _, err := s.seedDB.Exec(`UPDATE api_keys SET state = 'revoked', revoked_at = now() WHERE prefix = $1`, prefix); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	v, err := s.rdb.Incr(ctx, "qc:cfg:version").Result()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.rdb.Publish(ctx, "qc:cfg:invalidate", fmt.Sprintf(`{"version":"%d"}`, v)).Err(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		r := s.do(http.MethodPost, "/v1/check", tok, metered("rev.f", 1, tn.External))
		if r.Status == 401 && r.code(t) == "api_key_invalid" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a revoked key was still accepted a second after the invalidation: %d", r.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
