package reference_test

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAFullStoreRefusesRatherThanDisplaces (IP-15 DoD 7, T-13, DR-048): the
// store is filled to its maxmemory with real idempotency records, through the
// service. The first write that does not fit is 503 service_unavailable with
// the balance unchanged; evicted_keys stays 0 and DBSIZE never falls, so no
// part of the idempotency namespace was flushed to make room; and the moment
// the store has room it takes writes again and every record written before the
// fill answers its recorded response.
func TestAFullStoreRefusesRatherThanDisplaces(t *testing.T) {
	s := newStack(t, withEnv("QUOTACORE_VALKEY_MAXMEMORY", "12mb"))
	if policy := s.valkey("config", "get", "maxmemory-policy"); !strings.HasSuffix(policy, "noeviction") {
		t.Fatalf("the reference store runs %q", policy)
	}
	feature, tenants := provision(t, s, "full", 1, 1_000_000_000_000)
	tenant := tenants[0]
	token := s.runtimeKey()

	type record struct{ key, body string }
	var early []record
	for range 500 {
		key := newIdemKey()
		r := consume(token, tenant, feature, 1, key)
		if r.status != 200 {
			t.Fatalf("consume before the fill: %d %s", r.status, r.body)
		}
		early = append(early, record{key, string(r.body)})
	}
	replayAll := func(when string) {
		t.Helper()
		for _, rec := range early {
			r := consume(token, tenant, feature, 1, rec.key)
			want := strings.Replace(rec.body, `"replayed":false`, `"replayed":true`, 1)
			if r.status != 200 || string(r.body) != want {
				t.Fatalf("%s, the record for %s answered %d %s; want %s", when, rec.key, r.status, r.body, want)
			}
		}
	}

	var full atomic.Bool
	var refused atomic.Value
	var written atomic.Int64
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !full.Load() {
				r := consume(token, tenant, feature, 1, newIdemKey())
				switch r.status {
				case 200:
					written.Add(1)
					time.Sleep(12 * time.Millisecond) // under the per-key limit
				case 503:
					refused.CompareAndSwap(nil, r)
					full.Store(true)
				case 429:
					time.Sleep(50 * time.Millisecond)
				default:
					t.Errorf("during the fill: %d %v %s", r.status, r.err, r.body)
					full.Store(true)
				}
			}
		}()
	}
	var last int64
	checks := 0
	for deadline := time.Now().Add(5 * time.Minute); !full.Load(); time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			full.Store(true)
			t.Fatal("the store was not full after five minutes")
		}
		n, _ := strconv.ParseInt(s.valkey("dbsize"), 10, 64)
		if n < last {
			t.Errorf("DBSIZE fell from %d to %d during the fill", last, n)
		}
		last = n
		if ev := s.info("stats", "evicted_keys"); ev != 0 {
			t.Fatalf("the store evicted %d keys", ev)
		}
		if checks++; checks%10 == 0 {
			replayAll("during the fill")
		}
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	r := refused.Load().(response)
	if r.code() != "service_unavailable" {
		t.Fatalf("a full store answered %d %s, want 503 service_unavailable (FS-21)", r.status, r.body)
	}
	// The first refusal came while two dozen clients' buffers were in the
	// store's memory too, and the store shrinks idle clients' buffers on its
	// own clock, so a little room comes back now and then. topUp fills it from
	// one client until the store refuses twenty writes in a row.
	topUp := func() {
		t.Helper()
		for run, attempts := 0, 0; run < 20; attempts++ {
			if attempts > 50_000 {
				t.Fatal("the store never refused twenty writes in a row")
			}
			switch r := consume(token, tenant, feature, 1, newIdemKey()); {
			case r.status == 200:
				run = 0
				written.Add(1)
			case r.code() == "service_unavailable":
				run++
			default:
				t.Fatalf("topping up the fill: %d %s", r.status, r.body)
			}
		}
	}
	topUp()
	used, limit := s.info("memory", "used_memory"), s.info("memory", "maxmemory")
	t.Logf("full after %d records: used %d of %d", written.Load(), used, limit)

	// At the boundary the store's memory moves by a few hundred bytes either
	// way, so a write after a refusal may still fit. What must hold is that
	// every refused write changes nothing: across the attempts until a hundred
	// are refused, the balance falls by exactly the number that were taken.
	before, _ := balanceOf(t, token, tenant)
	taken, refusedAgain := 0, 0
	for attempts := 0; refusedAgain < 100; attempts++ {
		if attempts > 50_000 {
			t.Fatalf("only %d of %d writes into the full store were refused", refusedAgain, attempts)
		}
		switch r := consume(token, tenant, feature, 1, newIdemKey()); {
		case r.status == 200:
			taken++
		case r.code() == "service_unavailable":
			refusedAgain++
		default:
			t.Fatalf("a write into the full store: %d %s", r.status, r.body)
		}
	}
	after, _ := balanceOf(t, token, tenant)
	if before[feature]-after[feature] != int64(taken) {
		t.Errorf("%d writes were taken and the balance fell by %d: a refused write moved it", taken, before[feature]-after[feature])
	}
	t.Logf("%d writes refused and %d taken at the boundary", refusedAgain, taken)
	// A retry while the store is full is refused whole, like any write: the
	// store declines the script before it reads the record, so the answer is
	// a 503 to retry, or the recorded replay when it fits, never a second
	// charge.
	topUp()
	retried := 0
	for _, rec := range early[:50] {
		switch r := consume(token, tenant, feature, 1, rec.key); {
		case r.code() == "service_unavailable":
			retried++
		case r.status == 200 && strings.Contains(string(r.body), `"replayed":true`):
		default:
			t.Fatalf("a retry into the full store answered %d %s", r.status, r.body)
		}
	}
	if retried == 0 {
		t.Error("no retry into the full store was refused, so the store was not full")
	}
	if ev := s.info("stats", "evicted_keys"); ev != 0 {
		t.Fatalf("the store evicted %d keys", ev)
	}

	// Room again: the next write is taken, with no restart.
	s.valkey("config", "set", "maxmemory", "64mb")
	if r := consume(token, tenant, feature, 1, newIdemKey()); r.status != 200 {
		t.Fatalf("after the store was given room: %d %s", r.status, r.body)
	}
	replayAll("after the store was given room")
}
