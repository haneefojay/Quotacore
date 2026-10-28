package reference_test

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The restore drill (IP-15 DoD 5, testing-strategy.md section 9) and the
// store loss inside the window (DoD 8, T-14), against the reference stack
// with the real `quotacore rebuild`, as data-store-lost.md tells an operator
// to run it.

var drillZones = []string{"UTC", "America/New_York", "Europe/London", "Asia/Kolkata", "Australia/Lord_Howe", "Pacific/Chatham"}

var drillFeatures = []struct{ key, interval string }{
	{"drill.daily", "daily"}, {"drill.monthly", "monthly"}, {"drill.never", "never"},
}

type entry struct {
	Feature string `json:"feature_key"`
	Balance int64  `json:"balance"`
	Limit   int64  `json:"limit"`
	Bonus   int64  `json:"bonus"`
}

// entries reads a tenant's balances, keyed by feature.
func entries(token, tenant string) (map[string]entry, response) {
	r := do("GET", "/v1/balance?tenant_external_id="+tenant, token, "")
	for try := 0; r.code() == "service_unavailable" && try < 20; try++ {
		// A cold cache after a restart admits a bounded number of misses at
		// once and asks the rest to retry shortly.
		time.Sleep(100 * time.Millisecond)
		r = do("GET", "/v1/balance?tenant_external_id="+tenant, token, "")
	}
	if r.err != nil || r.status != 200 {
		return nil, r
	}
	var v struct {
		Entries []entry `json:"entries"`
	}
	json.Unmarshal(r.body, &v)
	out := map[string]entry{}
	for _, e := range v.Entries {
		out[e.Feature] = e
	}
	return out, r
}

type subject struct{ tenant, feature string }

// rebuildReport is the part of `quotacore rebuild`'s report the drill reads.
type rebuildReport struct {
	Applied bool           `json:"applied"`
	Counts  map[string]int `json:"counts"`
	Entries []struct {
		ExternalID string `json:"external_id"`
		Feature    string `json:"feature_key"`
		Class      string `json:"class"`
		Reason     string `json:"reason"`
		Action     string `json:"action"`
	} `json:"entries"`
}

func (s *stack) rebuild(args ...string) (int, rebuildReport) {
	s.t.Helper()
	code, out, errw := s.cli(append([]string{"rebuild"}, args...)...)
	var rep rebuildReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		s.t.Fatalf("rebuild %v: exit %d, not a report: %v\n%s\n%s", args, code, err, out, errw)
	}
	return code, rep
}

func TestTheRestoreDrill(t *testing.T) {
	s := newStack(t)
	rng := rand.New(rand.NewPCG(15, 15))

	// Step 1: a dataset across tenants, features, intervals and zones, with
	// mid-cycle set, grant and plan changes.
	for _, f := range drillFeatures {
		s.mustAdmin("feature", "create", "--key", f.key, "--name", f.key, "--unit", "units")
	}
	plan := func(key string, scale int64) {
		s.mustAdmin("plan", "create", "--key", key, "--name", key,
			"--entitlement", fmt.Sprintf("drill.daily=%d/daily", 1000*scale),
			"--entitlement", fmt.Sprintf("drill.monthly=%d/monthly", 100000*scale),
			"--entitlement", fmt.Sprintf("drill.never=%d/never", 50000*scale))
	}
	plan("drill-a", 1)
	plan("drill-b", 2)
	const tenants = 200
	name := func(i int) string { return fmt.Sprintf("drill-%03d", i) }
	for i := range tenants {
		s.mustAdmin("tenant", "create", "--external-id", name(i), "--name", name(i), "--plan", "drill-a",
			"--timezone", drillZones[i%len(drillZones)])
	}
	token := s.runtimeKey()
	type applied struct {
		tenant, feature, key string
		amount               int64
	}
	var consumed []applied
	for range 5000 {
		a := applied{name(rng.IntN(tenants)), drillFeatures[rng.IntN(3)].key, newIdemKey(), int64(1 + rng.IntN(20))}
		r := consume(token, a.tenant, a.feature, a.amount, a.key)
		for try := 0; r.code() == "service_unavailable" && try < 20; try++ {
			// A tenant created moments ago may not resolve yet; the answer says
			// to retry, and a retry with the same key is safe.
			time.Sleep(100 * time.Millisecond)
			r = consume(token, a.tenant, a.feature, a.amount, a.key)
		}
		if r.status == 200 {
			consumed = append(consumed, a)
		} else if r.status == 429 && r.code() == "rate_limited" {
			time.Sleep(10 * time.Millisecond)
		} else if r.code() != "quota_exceeded" {
			t.Fatalf("consume: %d %s", r.status, r.body)
		}
	}
	for _, a := range consumed[:300] {
		body := fmt.Sprintf(`{"tenant_external_id":%q,"feature_key":%q,"amount":%d}`, a.tenant, a.feature, a.amount/2+1)
		if r := do("POST", "/v1/refund", token, body, "Idempotency-Key", "refund-"+a.key); r.status != 200 && r.code() != "refund_exceeds_grant" {
			t.Fatalf("refund: %d %s", r.status, r.body)
		}
	}
	for i := range 10 {
		tn := name(i)
		s.mustAdmin("grant", tn, "--feature", "drill.monthly", "--amount", "500", "--reason", "drill: goodwill")
		s.mustAdmin("set", tn, "--feature", "drill.daily", "--value", "7", "--reason", "drill: corrected")
		got := s.mustAdmin("tenant", "get", tn)
		s.mustAdmin("tenant", "update", tn, "--version", fmt.Sprint(got["version"]), "--plan", "drill-b")
		consume(token, tn, "drill.never", 11, newIdemKey())
	}
	// A plan change re-anchors the tenant in the drain, on the configuration
	// refresh tick, and opens a new cycle at the new plan's limit (DR-013).
	// The expected balances are the ones after it: a drain still pending at the
	// loss runs after it and writes that same new cycle, which is the plan
	// assignment DR-045 allows, not an over-grant.
	eventually(t, 2*time.Minute, "the plan changes' re-anchors", func() bool {
		return s.psql("quotacore", "SELECT count(*) FROM pending_reanchors") == "0"
	})
	rows := s.settleLedger()
	t.Logf("seeded %d tenants, %d consumes, %d ledger rows", tenants, len(consumed), rows)

	// Faults the rebuild must find: three ledgers whose chain is broken past
	// their opening row, as a corrupted restore would leave them. Nothing else
	// is unreconcilable. (A broken opening row would break the limit the
	// rebuild derives too, which --set keeps: Q-29.)
	injected := map[subject]bool{}
	for _, sub := range []subject{{name(150), "drill.monthly"}, {name(151), "drill.daily"}, {name(152), "drill.never"}} {
		out := s.psql("quotacore", fmt.Sprintf(`UPDATE usage_events SET balance_after = balance_after + 1 WHERE id = (
			SELECT u.id FROM usage_events u JOIN tenants t ON t.id = u.tenant_id JOIN features f ON f.id = u.feature_id
			 WHERE t.external_id = '%s' AND f.key = '%s' ORDER BY u.seq OFFSET 1 LIMIT 1)`, sub.tenant, sub.feature))
		if !strings.Contains(out, "UPDATE 1") {
			t.Fatalf("the fault on %v changed no row: %q", sub, out)
		}
		injected[sub] = true
	}

	// Step 2: the expected balances.
	pre := map[subject]entry{}
	for i := range tenants {
		es, r := entries(token, name(i))
		if r.status != 200 {
			t.Fatalf("balance %s: %d %s", name(i), r.status, r.body)
		}
		for f, e := range es {
			pre[subject{name(i), f}] = e
		}
	}

	// Step 6, watched from here to the end: no answer anywhere shows a
	// balance above its pre-loss value, or a full allowance a tenant did not
	// already have, and no write is accepted while the fleet is read-only or
	// for a hash the rebuild left absent (DR-045).
	var violations atomic.Int64
	var vmu sync.Mutex
	var seen []string
	violate := func(f string, a ...any) {
		if violations.Add(1) <= 10 {
			vmu.Lock()
			seen = append(seen, fmt.Sprintf(f, a...))
			vmu.Unlock()
		}
	}
	// Step 3, first half: read-only first (data-store-lost.md). The watcher
	// starts once the fleet is read-only, so none of its writes lands before
	// the loss; it writes only while the fleet is read-only, where every write
	// must be refused, and only reads once it is normal again.
	s.restartIn("readonly")
	var readOnly atomic.Bool
	readOnly.Store(true)
	stop := make(chan struct{})
	var watching sync.WaitGroup
	reads := atomic.Int64{}
	for w := range 4 {
		watching.Add(1)
		go func() {
			defer watching.Done()
			for i := w; ; i = (i + 4) % tenants {
				select {
				case <-stop:
					return
				default:
				}
				tn := name(i)
				if es, r := entries(token, tn); r.status == 200 {
					reads.Add(1)
					for f, e := range es {
						p := pre[subject{tn, f}]
						if e.Balance > p.Balance {
							violate("%s:%s read %d, above its pre-loss %d", tn, f, e.Balance, p.Balance)
						}
						if e.Balance == e.Limit+e.Bonus && p.Balance < p.Limit+p.Bonus {
							violate("%s:%s read a full allowance %d it did not have (%d)", tn, f, e.Balance, p.Balance)
						}
					}
				}
				if readOnly.Load() {
					for sub := range injected {
						if sub.tenant != tn {
							continue
						}
						if r := consume(token, tn, sub.feature, 1, newIdemKey()); r.status == 200 {
							violate("%s:%s accepted a write with its hash absent: %s", tn, sub.feature, r.body)
						}
					}
					if r := consume(token, tn, "drill.monthly", 1, newIdemKey()); r.status == 200 {
						violate("%s accepted a write while the fleet was read-only: %s", tn, r.body)
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}

	// Step 3, second half: the loss.
	s.valkey("flushall")
	if n := s.valkey("dbsize"); n != "0" {
		t.Fatalf("DBSIZE after the flush is %s", n)
	}

	// Step 4: a dry run, then the rebuild.
	if code, dry := s.rebuild(); code != 1 || dry.Applied {
		t.Fatalf("the dry run exited %d (applied %v); a run that finds lost hashes exits 1", code, dry.Applied)
	}
	_, rep := s.rebuild("--apply", "--reason", "restore drill")

	// Step 5: every exact entry was written and every flagged one is a fault
	// the drill injected, and the reverse.
	flagged := map[subject]bool{}
	written := 0
	for _, e := range rep.Entries {
		sub := subject{e.ExternalID, e.Feature}
		switch e.Class {
		case "exact":
			if e.Action != "written" {
				t.Errorf("exact %v was not written: %+v", sub, e)
			}
			written++
		default:
			flagged[sub] = true
			if e.Class != "unreconcilable" {
				t.Errorf("%v is %s (%s); the drill injected no loss", sub, e.Class, e.Reason)
			}
		}
	}
	if fmt.Sprint(flagged) != fmt.Sprint(injected) {
		t.Errorf("the rebuild flagged %v; the drill broke %v", flagged, injected)
	}
	if written+len(flagged) != len(pre) {
		t.Errorf("the report covers %d tenant-features, the dataset %d", written+len(flagged), len(pre))
	}

	// data-store-lost.md's decision for the flagged ones: set each to the
	// value the operator knows, with a reason, still read-only.
	for sub := range injected {
		s.rebuild(fmt.Sprintf("--set=%s:%s=%d", sub.tenant, sub.feature, pre[sub].Balance), "--reason", "restore drill: known balance")
	}

	// Step 7: normal mode, and every balance is what it was.
	readOnly.Store(false)
	s.restartIn("normal")
	time.Sleep(2 * time.Second)
	close(stop)
	watching.Wait()
	for i := range tenants {
		es, r := entries(token, name(i))
		if r.status != 200 {
			t.Errorf("after the drill, balance %s: %d %s", name(i), r.status, r.body)
			continue
		}
		for f, e := range es {
			if p := pre[subject{name(i), f}]; e != p {
				t.Errorf("%s:%s after the drill is %+v, before the loss %+v", name(i), f, e, p)
			}
		}
	}
	if n := violations.Load(); n > 0 {
		t.Errorf("%d DR-045 violations during the drill: %s", n, strings.Join(seen, "; "))
	}
	if reads.Load() == 0 {
		t.Error("the watcher read nothing, so step 6 was not observed")
	}
	t.Logf("rebuilt %d exact, %d flagged and set; %d reads watched", written, len(flagged), reads.Load())
}

// TestAStoreLossInsideTheWindowIsReversed (IP-15 DoD 8, T-14, DR-049): a
// consume of 30, the store lost and rebuilt, the identical request retried
// inside its window and applied again, and reconciliation reversing it once.
func TestAStoreLossInsideTheWindowIsReversed(t *testing.T) {
	s := newStack(t, withEnv("QUOTACORE_RECONCILE_INTERVAL", "1m"))
	feature, tenants := provision(t, s, "t14", 1, 1000)
	tenant := tenants[0]
	token := s.runtimeKey()
	key := newIdemKey()
	if r := consume(token, tenant, feature, 30, key); r.status != 200 || !strings.Contains(string(r.body), `"balance":970`) {
		t.Fatalf("the first consume: %d %s", r.status, r.body)
	}
	s.settleLedger()

	s.restartIn("readonly")
	s.valkey("flushall")
	if _, rep := s.rebuild("--apply", "--reason", "T-14"); len(rep.Entries) != 1 || rep.Entries[0].Action != "written" {
		t.Fatalf("the rebuild did not restore the one hash: %+v", rep)
	}
	s.restartIn("normal")

	// The record is gone, so the retry is applied again.
	r := consume(token, tenant, feature, 30, key)
	if r.status != 200 || !strings.Contains(string(r.body), `"balance":940`) || !strings.Contains(string(r.body), `"replayed":false`) {
		t.Fatalf("the retry after the loss: %d %s; want a second deduction to 940", r.status, r.body)
	}

	// One reconciliation pass detects it and refunds once.
	var events []map[string]any
	eventually(t, 4*time.Minute, "the reversal", func() bool {
		events = s.events(tenant, "--idempotency-key", key)
		return len(events) >= 3
	})
	var deductions, reversals int
	for _, e := range events {
		switch e["event_type"] {
		case "consumed":
			deductions++
		case "refunded":
			if e["source"] == "system" && e["delta"] == float64(30) {
				reversals++
			}
		}
	}
	if deductions != 2 || reversals != 1 || len(events) != 3 {
		t.Fatalf("events under the key: %v; want two deductions and one system refund of 30", events)
	}
	if bal, _ := balanceOf(t, token, tenant); bal[feature] != 970 {
		t.Fatalf("the net balance is %d, want one charge of 30 (970)", bal[feature])
	}
	// A second pass adds nothing.
	time.Sleep(90 * time.Second)
	if again := s.events(tenant, "--idempotency-key", key); len(again) != 3 {
		t.Fatalf("a later pass changed the events under the key: %v", again)
	}
	if bal, _ := balanceOf(t, token, tenant); bal[feature] != 970 {
		t.Fatalf("a later pass moved the balance to %d", bal[feature])
	}
}

// events lists a tenant's ledger through the CLI.
func (s *stack) events(tenant string, filters ...string) []map[string]any {
	s.t.Helper()
	v := s.mustAdmin(append([]string{"events", tenant, "--all"}, filters...)...)
	var out []map[string]any
	for _, field := range v {
		if items, ok := field.([]any); ok {
			for _, d := range items {
				out = append(out, d.(map[string]any))
			}
		}
	}
	return out
}
