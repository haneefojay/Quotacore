package snapshot

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"
)

func planFixture(features int, id string) *LoadedPlan {
	ents := make(map[string]Entitlement, features)
	for i := 0; i < features; i++ {
		ents[fmt.Sprintf("feature_%03d", i)] = Entitlement{Limit: 1000, Interval: 1}
	}
	return &LoadedPlan{ID: id, Key: "starter", Entitlements: ents}
}

// TestCacheRespectsEntryLimit asserts the LRU evicts by recency to the entry
// bound (NFR-T4's shape at the unit level).
func TestCacheRespectsEntryLimit(t *testing.T) {
	c := newTenantCache(3, 1<<30)
	t0 := &Tenant{ID: "a", Plan: &Plan{ID: "p", Key: "k"}}
	t1 := &Tenant{ID: "b", Plan: &Plan{ID: "p", Key: "k"}}
	t2 := &Tenant{ID: "c", Plan: &Plan{ID: "p", Key: "k"}}
	t3 := &Tenant{ID: "d", Plan: &Plan{ID: "p", Key: "k"}}
	c.add(t0)
	c.add(t1)
	c.add(t2)
	if c.len() != 3 {
		t.Fatalf("len = %d, want 3", c.len())
	}
	c.get("a") // make a most-recent
	c.add(t3)
	if c.len() != 3 {
		t.Fatalf("len = %d, want 3 after adding the fourth", c.len())
	}
	if c.get("a") == nil {
		t.Error("the most-recent tenant must survive")
	}
	if c.get("b") != nil {
		t.Error("the least-recent tenant must be evicted")
	}
}

// TestCacheRespectsByteBudget asserts the byte cap evicts too, and that the
// budget counts one copy of an interned plan rather than one per tenant.
func TestCacheRespectsByteBudget(t *testing.T) {
	c := newTenantCache(1000, 300)
	shared := &Plan{ID: "p1", Key: "starter", Entitlements: map[string]Entitlement{"f": {Limit: 1}}}
	for i := 0; i < 50; i++ {
		t := &Tenant{ID: fmt.Sprintf("t%02d", i), Plan: shared}
		c.add(t)
	}
	if len(c.plans) != 1 {
		t.Errorf("interned plans = %d, want 1", len(c.plans))
	}
	if c.len() == 50 {
		t.Error("a small byte budget must have evicted some tenants")
	}
	// The shared plan must be refunded when its last tenant leaves.
	for c.len() > 0 {
		c.evictOldest()
	}
	if len(c.plans) != 0 || c.planMem != 0 {
		t.Errorf("plan table = %d, planMem = %d, want 0 and 0", len(c.plans), c.planMem)
	}
}

// TestCacheSharesInternedPlans asserts the object-level claim that makes the
// memory bound real: N tenants on one plan hold one plan object
// (request-lifecycle.md section 3).
func TestCacheSharesInternedPlans(t *testing.T) {
	c := newTenantCache(50_000, 1<<30)
	plan := &Plan{ID: "p1", Key: "starter"}
	for i := 0; i < 10_000; i++ {
		c.add(&Tenant{ID: fmt.Sprintf("t%05d", i), Plan: plan})
	}
	if len(c.plans) != 1 {
		t.Fatalf("interned plans = %d, want 1", len(c.plans))
	}
	first := c.byID["t00000"].Value.(*cacheEntry).tenant.Plan
	for i := 0; i < 10_000; i++ {
		if id := fmt.Sprintf("t%05d", i); c.byID[id].Value.(*cacheEntry).tenant.Plan != first {
			t.Fatalf("tenant %s does not share the interned plan", id)
		}
	}
	if got := c.plans["p1"].refs; got != 10_000 {
		t.Errorf("plan refs = %d, want 10000", got)
	}
}

// TestRateLimiterInTime asserts the token bucket refills on the caller's clock:
// a burst is allowed, the next call is refused, and time pays the debt back.
func TestRateLimiterInTime(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	l := newRateLimiter(2, clock.now)
	if !l.allow() || !l.allow() {
		t.Fatal("a full bucket must allow its burst")
	}
	if l.allow() {
		t.Fatal("a drained bucket must refuse until refilled")
	}
	clock.advance(500 * time.Millisecond) // half a second at 2/s = one token
	if !l.allow() {
		t.Fatal("half a second at 2/s must restore one token")
	}
	if l.allow() {
		t.Fatal("the refilled token was spent; the bucket must be empty again")
	}
	clock.advance(2 * time.Second) // four tokens, but the burst caps at two
	if !l.allow() || !l.allow() {
		t.Fatal("after two seconds the capped burst must be usable")
	}
	if l.allow() {
		t.Fatal("the burst cap must be respected")
	}
}

// memoryLoader returns one shared plan and one tenant per id, the shape IP-08's
// loader will have when it serves many tenants off one interned plan.
type memoryLoader struct {
	plan *LoadedPlan
}

func (l *memoryLoader) Load(_ context.Context, id string) (*LoadedTenant, error) {
	return &LoadedTenant{
		ID: id, ExternalID: "ext-" + id, State: "active", Timezone: "UTC",
		Anchor: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Plan:   l.plan,
	}, nil
}

// TestSnapshotMemoryWithinNFRT7 is the phase's NFR-T7 evidence: 50,000 tenants
// across 100 features on one interned plan must sit inside the 64 MiB cap. It
// measures HeapAlloc directly rather than trusting the approximate budget: the
// eviction loop only enforces the budget approximately, so the proof has to be
// the actual allocation delta.
func TestSnapshotMemoryWithinNFRT7(t *testing.T) {
	loader := &memoryLoader{plan: planFixture(100, "df125e35-aaaa-4000-8000-0000000000a1")}
	s := New(Options{Loader: loader, LimitEntries: 50_000, ByteCap: 64 << 20, MissRate: 1e9})
	s.WithMetrics(newMetrics())

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	const tenants = 50_000
	for i := 0; i < tenants; i++ {
		id := fmt.Sprintf("df125e35-%05d-4000-8000-000000000001", i)
		if _, err := s.Resolve(id, "feature_050"); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	delta := after.HeapAlloc - before.HeapAlloc
	if delta > 64<<20 {
		t.Fatalf("tenants touched %.1f MiB of heap, over the 64 MiB cap (NFR-T7)", float64(delta)/(1<<20))
	}
	if got := s.cache.len(); got != tenants {
		t.Fatalf("cache holds %d tenants, want all %d cached (no eviction under its own limits)", got, tenants)
	}
	if len(s.cache.plans) != 1 {
		t.Fatalf("interned plans = %d, want 1", len(s.cache.plans))
	}
}
