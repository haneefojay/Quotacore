package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// countingResolver maps external ids for fakeLoader's tenants and counts the
// statements it stands for.
type countingResolver struct {
	mu    sync.Mutex
	calls int
	byExt map[string]string
	err   error
}

func (r *countingResolver) TenantIDByExternalID(_ context.Context, ext string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return "", r.err
	}
	id, ok := r.byExt[ext]
	if !ok {
		return "", ErrTenantUnknown
	}
	return id, nil
}

// TestTheFirstSightOfATenantIsBounded (IP-07, decision D3): a cold
// external_id costs one limiter token and two statements, the id read and the
// Load; a warm one costs nothing, nor does one already cached by id; and
// evicting the tenant evicts its external_id with it.
func TestTheFirstSightOfATenantIsBounded(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	loader := &fakeLoader{byID: map[string]*LoadedTenant{tenantID: tenantFixture(tenantID), otherID: tenantFixture(otherID)}}
	resolver := &countingResolver{byExt: map[string]string{"ext-" + tenantID: tenantID, "ext-" + otherID: otherID}}
	s := New(Options{Loader: loader, Resolver: resolver, MissRate: 1, Now: clock.now, LimitEntries: 1})
	s.WithMetrics(newMetrics())

	got, err := s.Tenant("ext-" + tenantID)
	if err != nil || got.ID != tenantID {
		t.Fatalf("cold lookup: %v, %v", got, err)
	}
	if resolver.calls != 1 || loader.calls != 1 {
		t.Errorf("a cold external_id cost %d id reads and %d loads, want 1 and 1", resolver.calls, loader.calls)
	}
	// The one token is spent: a second cold tenant in the same instant is
	// refused without a statement.
	if _, err := s.Tenant("ext-" + otherID); !errors.Is(err, ErrMissRefused) {
		t.Errorf("second cold tenant in the same instant: %v, want ErrMissRefused", err)
	}
	if resolver.calls != 1 || loader.calls != 1 {
		t.Errorf("a refused miss issued a statement: %d, %d", resolver.calls, loader.calls)
	}
	for i := 0; i < 100; i++ {
		if _, err := s.Tenant("ext-" + tenantID); err != nil {
			t.Fatal(err)
		}
	}
	if resolver.calls != 1 || loader.calls != 1 {
		t.Errorf("warm lookups issued statements: %d, %d", resolver.calls, loader.calls)
	}

	// With a bound of one entry, loading the other tenant evicts the first,
	// and its external_id goes with it: the next lookup is cold again.
	clock.advance(time.Second)
	if _, err := s.Tenant("ext-" + otherID); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if _, err := s.Tenant("ext-" + tenantID); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 3 || loader.calls != 3 {
		t.Errorf("after eviction: %d id reads and %d loads, want 3 and 3", resolver.calls, loader.calls)
	}

	// A tenant Resolve already cached by id is cached by external_id too, so
	// it costs nothing.
	s2 := New(Options{Loader: loader, Resolver: resolver, MissRate: 100, Now: clock.now})
	if _, err := s2.Resolve(otherID, "feature_a"); err != nil {
		t.Fatal(err)
	}
	loads, reads := loader.calls, resolver.calls
	if _, err := s2.Tenant("ext-" + otherID); err != nil {
		t.Fatal(err)
	}
	if loader.calls != loads || resolver.calls != reads {
		t.Errorf("a tenant cached by id: %d loads, %d id reads; want none", loader.calls-loads, resolver.calls-reads)
	}
}

func TestExternalIDFailuresAreTyped(t *testing.T) {
	loader := &fakeLoader{byID: map[string]*LoadedTenant{tenantID: tenantFixture(tenantID)}}
	for name, c := range map[string]struct {
		resolverErr, loaderErr, want error
	}{
		"unknown":       {nil, nil, ErrTenantUnknown},
		"resolver down": {fmt.Errorf("connection refused"), nil, ErrRefreshFailed},
		"loader down":   {nil, fmt.Errorf("connection refused"), ErrRefreshFailed},
	} {
		loader.err = c.loaderErr
		resolver := &countingResolver{byExt: map[string]string{"ext-" + tenantID: tenantID}, err: c.resolverErr}
		s := New(Options{Loader: loader, Resolver: resolver, MissRate: 100})
		ext := "ext-" + tenantID
		if name == "unknown" {
			ext = "nobody"
		}
		if _, err := s.Tenant(ext); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	loader.err = nil

	// Decide on a looked-up tenant is Resolve's decision.
	s := New(Options{Loader: loader, Resolver: &countingResolver{byExt: map[string]string{"ext-" + tenantID: tenantID}}, MissRate: 100})
	tn, err := s.Tenant("ext-" + tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if r := s.Decide(tn, "feature_a"); !r.InPlan || r.Limit != 1000 {
		t.Errorf("Decide = %+v", r)
	}
	if r := s.Decide(tn, "nope"); r.InPlan {
		t.Errorf("Decide on a feature outside the plan = %+v", r)
	}
	if len(tn.Features()) != 2 {
		t.Errorf("Features = %v", tn.Features())
	}
}

// TestOnChangeRunsAfterAnAppliedInvalidation: the key cache's hook fires for
// a reconcile and a forward invalidation, and not for a stale one.
func TestOnChangeRunsAfterAnAppliedInvalidation(t *testing.T) {
	client := &fakeClient{tenants: map[string]int64{}}
	s := New(Options{Loader: &fakeLoader{}, Client: client, MissRate: 1})
	fired := make(chan struct{}, 10)
	s.OnChange(func() { fired <- struct{}{} })
	s.reconcile()
	waitFired(t, fired)
	if !s.applyInvalidation(`{"version":"5"}`) {
		t.Fatal("a forward invalidation was not applied")
	}
	waitFired(t, fired)
	if s.applyInvalidation(`{"version":"4"}`) {
		t.Fatal("a stale invalidation was applied")
	}
	select {
	case <-fired:
		t.Error("a stale invalidation fired the hook")
	case <-time.After(50 * time.Millisecond):
	}
}

func waitFired(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("the change hook did not run")
	}
}
