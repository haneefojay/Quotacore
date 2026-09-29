package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/observability"
)

// fakeClient is the InvalidationSource-capable double the unit tests drive.
// The deciding thing about it: ConfigVersion and TenantVersions are what the
// reconciler and invalidator read, so a fake that returns canned state tests
// those paths without a datastore.
type fakeClient struct {
	mu      sync.Mutex
	version int64
	tenants map[string]int64
	subs    []*fakeSub
}

func (f *fakeClient) ConfigVersion(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, nil
}

func (f *fakeClient) TenantVersions(context.Context) (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int64, len(f.tenants))
	for k, v := range f.tenants {
		out[k] = v
	}
	return out, nil
}

func (f *fakeClient) SubscribeInvalidations(context.Context) (InvalidationSource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &fakeSub{ch: make(chan string, 8)}
	f.subs = append(f.subs, s)
	return s, nil
}

type fakeSub struct{ ch chan string }

func (s *fakeSub) Messages() <-chan string { return s.ch }
func (s *fakeSub) Close() error            { close(s.ch); return nil }
func (s *fakeSub) push(payload string)     { s.ch <- payload }

// failingClient is the same double with a switch, for the DoD item 7 evidence
// that a refresh failure keeps the previous snapshot serving.
type failingClient struct {
	*fakeClient
	failMu sync.Mutex
	fail   bool
}

func (f *failingClient) ConfigVersion(ctx context.Context) (int64, error) {
	f.failMu.Lock()
	flag := f.fail
	f.failMu.Unlock()
	if flag {
		return 0, fmt.Errorf("control plane down")
	}
	return f.fakeClient.ConfigVersion(ctx)
}

func (f *failingClient) TenantVersions(ctx context.Context) (map[string]int64, error) {
	f.failMu.Lock()
	flag := f.fail
	f.failMu.Unlock()
	if flag {
		return nil, fmt.Errorf("control plane down")
	}
	return f.fakeClient.TenantVersions(ctx)
}

type fakeLoader struct {
	mu    sync.Mutex
	calls int
	byID  map[string]*LoadedTenant
	err   error
}

func (l *fakeLoader) Load(_ context.Context, id string) (*LoadedTenant, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	t, ok := l.byID[id]
	if !ok {
		return nil, ErrTenantUnknown
	}
	return t, nil
}

// fakeClock is the injectable time for the limiter and the lag observations.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const (
	tenantID = "df125e35-7a6d-4a63-9695-889af4db8da9"
	otherID  = "df125e35-0000-4000-8000-000000000002"
)

func newMetrics() *observability.Metrics {
	return observability.New(observability.Options{HitRatio: func() float64 { return 0 }})
}

func tenantFixture(id string) *LoadedTenant {
	return &LoadedTenant{
		ID:         id,
		ExternalID: "ext-" + id,
		State:      "active",
		Timezone:   "UTC",
		Anchor:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Version:    2,
		Plan: &LoadedPlan{
			ID:  "df125e35-aaaa-4000-8000-0000000000a1",
			Key: "starter",
			Entitlements: map[string]Entitlement{
				"feature_a": {Limit: 1000, Interval: cycle.Monthly},
				"feature_b": {Limit: 5, Interval: cycle.Hourly},
			},
		},
	}
}

// TestResolveHitDoesNotTouchTheLoader is the behavioural half of DoD item 1:
// a cached tenant is resolved with no loader call, hence no control-plane read
// and no pool wait on the hit path (DR-039).
func TestResolveHitDoesNotTouchTheLoader(t *testing.T) {
	loader := &fakeLoader{byID: map[string]*LoadedTenant{tenantID: tenantFixture(tenantID)}}
	s := New(Options{Loader: loader, MissRate: 1e9})
	s.WithMetrics(newMetrics())
	if _, err := s.Resolve(tenantID, "feature_a"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	loader.mu.Lock()
	afterFirst := loader.calls
	loader.mu.Unlock()
	for i := 0; i < 100; i++ {
		if _, err := s.Resolve(tenantID, "feature_a"); err != nil {
			t.Fatalf("hit %d: %v", i, err)
		}
	}
	loader.mu.Lock()
	defer loader.mu.Unlock()
	if loader.calls != afterFirst {
		t.Errorf("100 hits caused %d loader calls, want 0 after the first fill", loader.calls-afterFirst)
	}
}

// TestResolveMissLoadsExactlyOncePerTenant asserts one Loader call per miss and
// that the second resolve is a hit.
func TestResolveMissLoadsExactlyOncePerTenant(t *testing.T) {
	loader := &fakeLoader{byID: map[string]*LoadedTenant{tenantID: tenantFixture(tenantID)}}
	s := New(Options{Loader: loader, MissRate: 1e9})

	r, err := s.Resolve(tenantID, "feature_a")
	if err != nil {
		t.Fatal(err)
	}
	if !r.InPlan || r.Limit != 1000 || r.Interval != cycle.Monthly {
		t.Errorf("resolution = %+v, want in-plan monthly limit 1000", r)
	}
	r2, err := s.Resolve(tenantID, "feature_a")
	if err != nil {
		t.Fatal(err)
	}
	if r2.InPlan != r.InPlan {
		t.Error("second resolve must agree with the first")
	}
	loader.mu.Lock()
	defer loader.mu.Unlock()
	if loader.calls != 1 {
		t.Errorf("loader calls = %d, want 1", loader.calls)
	}
}

// TestResolveOrderArchivedOverSuspendedOverInPlan pins the decision order of
// DR-015 in one place (request-lifecycle.md section 5): archived beats
// suspended beats in-plan beats not-in-plan.
func TestResolveOrderArchivedOverSuspendedOverInPlan(t *testing.T) {
	loader := &fakeLoader{byID: map[string]*LoadedTenant{tenantID: tenantFixture(tenantID)}}
	s := New(Options{Loader: loader, MissRate: 1e9})
	s.features["feature_a"] = true

	r, err := s.Resolve(tenantID, "feature_a")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Archived || r.InPlan {
		t.Errorf("archived feature must resolve archived over in-plan, got %+v", r)
	}
	delete(s.features, "feature_a")

	s.fill(&LoadedTenant{ID: tenantID, ExternalID: "x", State: "suspended", Timezone: "UTC",
		Plan:    &LoadedPlan{ID: "p", Key: "starter", Entitlements: map[string]Entitlement{"feature_a": {Limit: 1, Interval: cycle.Daily}}},
		Version: 4})
	r, err = s.Resolve(tenantID, "feature_a")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Suspended || r.InPlan {
		t.Errorf("suspended tenant must resolve suspended over in-plan, got %+v", r)
	}
}

// TestResolveNotInPlanAndOverrideSemantics covers the plan membership side of
// the same order, and the override-inherits-plan hole that DR-015 closes.
func TestResolveNotInPlanAndOverrideSemantics(t *testing.T) {
	fx := tenantFixture(tenantID)
	fx.Overrides = map[string]LoadedOverride{"feature_a": {Limit: int64Ptr(500)}}
	loader := &fakeLoader{byID: map[string]*LoadedTenant{tenantID: fx}}
	s := New(Options{Loader: loader, MissRate: 1e9})

	r, err := s.Resolve(tenantID, "feature_a")
	if err != nil {
		t.Fatal(err)
	}
	if !r.InPlan || r.Limit != 500 || r.Interval != cycle.Monthly {
		t.Errorf("override must win the limit and inherit the interval, got %+v", r)
	}
	r, err = s.Resolve(tenantID, "feature_missing")
	if err != nil {
		t.Fatal(err)
	}
	if r.InPlan {
		t.Error("a feature not in the plan must not resolve in-plan")
	}
}

func int64Ptr(v int64) *int64 { return &v }

// TestMissLimitIsRefused asserts the bounded-miss contract: above the rate the
// data plane fails closed rather than waiting or touching the control plane
// (DR-039, request-lifecycle.md section 3).
func TestMissLimitIsRefused(t *testing.T) {
	loader := &fakeLoader{byID: map[string]*LoadedTenant{tenantID: tenantFixture(tenantID)}}
	s := New(Options{Loader: loader, MissRate: 1})
	if _, err := s.Resolve(tenantID, "feature_a"); err != nil {
		t.Fatalf("first miss: %v", err)
	}
	if _, err := s.Resolve(otherID, "feature_a"); !errors.Is(err, ErrMissRefused) {
		t.Fatalf("second miss within the burst must be refused, got %v", err)
	}
}

// TestResolveUnknownTenantAndControlPlaneFailure cover the two typed failure
// outcomes of the miss path: a provable absence is a 404 in the catalogue's
// terms (via ErrTenantUnknown), and any other loader failure is the fail-closed
// control-plane-unreachable (ErrRefreshFailed). No third error escapes.
func TestResolveUnknownTenantAndControlPlaneFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"unknown", ErrTenantUnknown, ErrTenantUnknown},
		{"down", fmt.Errorf("connection refused"), ErrRefreshFailed},
	}
	for _, tc := range cases {
		loader := &fakeLoader{byID: map[string]*LoadedTenant{}, err: tc.err}
		s := New(Options{Loader: loader, MissRate: 1e9})
		_, err := s.Resolve(tenantID, "feature_a")
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: Resolve err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// TestStaleInvalidationIsDiscardedAndCounted is DoD item 5: a message carrying
// a version already seen is not applied, is counted as skipped, and leaves the
// cache untouched.
func TestStaleInvalidationIsDiscardedAndCounted(t *testing.T) {
	fc := &fakeClient{version: 5, tenants: map[string]int64{tenantID: 5}}
	fx := tenantFixture(tenantID)
	loader := &fakeLoader{byID: map[string]*LoadedTenant{tenantID: fx}}
	metrics := newMetrics()
	s := New(Options{Loader: loader, Client: fc, MissRate: 1e9})
	s.WithMetrics(metrics)

	if _, err := s.Resolve(tenantID, "feature_a"); err != nil {
		t.Fatal(err)
	}
	if !s.applyInvalidation(`{"version":"6"}`) {
		t.Fatal("a forward invalidation must apply")
	}
	fc.tenants[tenantID] = 6
	if len(s.cache.byID) != 0 {
		t.Fatalf("forward invalidation must evict the stale tenant, cache = %d entries", len(s.cache.byID))
	}

	if s.applyInvalidation(`{"version":"3"}`) {
		t.Error("a version behind the snapshot must not apply")
	}
	if got := testutil.ToFloat64(metrics.ControlPlaneRefresh.WithLabelValues(observability.RefreshSkipped)); got != 1 {
		t.Errorf("stale message should be counted once as skipped, counted %v", got)
	}
	s.applyInvalidation(`{"version":"5"}`)
	if got := testutil.ToFloat64(metrics.ControlPlaneRefresh.WithLabelValues(observability.RefreshSkipped)); got != 2 {
		t.Errorf("a message equal to the snapshot version is stale too, count = %v", got)
	}
	if s.applyInvalidation(`not json`) {
		t.Error("an unparsable payload must not apply")
	}
}

// TestInvalidationPrunesOnlyTheChangedTenant is the incremental half of
// data-model.md section 3.4: a tenant whose version moved is dropped, a tenant
// that did not move stays, and one that vanished is dropped with it.
func TestInvalidationPrunesOnlyTheChangedTenant(t *testing.T) {
	fc := &fakeClient{version: 2, tenants: map[string]int64{tenantID: 2, otherID: 2}}
	loader := &fakeLoader{byID: map[string]*LoadedTenant{
		tenantID: tenantFixture(tenantID),
		otherID:  tenantFixture(otherID),
	}}
	s := New(Options{Loader: loader, Client: fc, MissRate: 1e9})

	for _, id := range []string{tenantID, otherID} {
		if _, err := s.Resolve(id, "feature_a"); err != nil {
			t.Fatal(err)
		}
	}
	// The cached entries are both version 2 (what cfg:tenants said when they
	// loaded). Moving one tenant's version in cfg:tenants is exactly the change
	// an invalidation message announces.
	fc.tenants[otherID] = 3

	if !s.applyInvalidation(`{"version":"3"}`) {
		t.Fatal("forward invalidation must apply")
	}
	if s.cache.get(tenantID) == nil {
		t.Error("the tenant whose version did not move must stay cached")
	}
	if s.cache.get(otherID) != nil {
		t.Error("the tenant whose version moved must be evicted")
	}

	delete(fc.tenants, tenantID)
	if !s.applyInvalidation(`{"version":"4"}`) {
		t.Fatal("second forward invalidation must apply")
	}
	if s.cache.get(tenantID) != nil {
		t.Error("a tenant deleted from the configuration must be evicted")
	}
}

// TestReconcileFailureKeepsThePreviousSnapshot is DoD item 7: a control-plane
// read that fails during reconcile must leave the cache serving exactly as it
// was — an outage may not clear the enforcement that is still working.
func TestReconcileFailureKeepsThePreviousSnapshot(t *testing.T) {
	fc := &failingClient{fakeClient: &fakeClient{version: 1, tenants: map[string]int64{tenantID: 1}}}
	loader := &fakeLoader{byID: map[string]*LoadedTenant{tenantID: tenantFixture(tenantID)}}
	s := New(Options{Loader: loader, Client: fc, MissRate: 1e9})
	s.WithMetrics(newMetrics())

	if _, err := s.Resolve(tenantID, "feature_a"); err != nil {
		t.Fatal(err)
	}
	fc.failMu.Lock()
	fc.fail = true
	fc.failMu.Unlock()
	s.reconcile()
	fc.failMu.Lock()
	fc.fail = false
	fc.failMu.Unlock()

	r, err := s.Resolve(tenantID, "feature_a")
	if err != nil {
		t.Fatalf("tenant must still resolve after a failed reconcile: %v", err)
	}
	if !r.InPlan {
		t.Error("the snapshot must still hold the tenant after a failed reconcile")
	}
}

// TestResolutionCountsHitsAndMisses asserts the two counters the hit-ratio
// gauge is computed from move with the traffic.
func TestResolutionCountsHitsAndMisses(t *testing.T) {
	metrics := newMetrics()
	s := New(Options{Loader: &fakeLoader{byID: map[string]*LoadedTenant{tenantID: tenantFixture(tenantID)}}, MissRate: 1e9})
	s.WithMetrics(metrics)
	for i := 0; i < 7; i++ {
		s.Resolve(tenantID, "feature_a")
	}
	s.Resolve(otherID, "feature_a") // miss; the tenant is unknown to the loader
	if got := testutil.ToFloat64(metrics.CacheMisses.WithLabelValues(observability.MissUnknown)); got != 1 {
		t.Errorf("unknown misses = %v, want 1", got)
	}
	// Seven tenant resolves: one miss to fill, six hits; one more miss for the
	// unknown tenant. Six hits over eight resolutions.
	if s.HitRatio() != 0.75 {
		t.Errorf("hit ratio = %v, want 0.75", s.HitRatio())
	}
}
