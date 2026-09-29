package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quotacore/quotacore/internal/observability"
)

// Options configure a snapshot. The numbers are the deployment.md section 4
// keys: QUOTACORE_CONFIG_CACHE_ENTRIES, QUOTACORE_CONFIG_CACHE_MB,
// QUOTACORE_CONFIG_REFRESH_INTERVAL, QUOTACORE_CONFIG_REFRESH_MISS_RATE_LIMIT
// and QUOTACORE_CONTROLPLANE_TIMEOUT.
type Options struct {
	LimitEntries        int
	ByteCap             int64
	MissRate            float64
	RefreshInterval     time.Duration
	ControlPlaneTimeout time.Duration
	Client              Client // the datastore handle for invalidation and versions
	Loader              Loader // the single bounded control-plane read (IP-08)
	Metrics             *observability.Metrics
	Now                 func() time.Time
}

// Client is the slice of the datastore the snapshot talks to: the invalidation
// channel, the config version and the per-tenant versions. internal/store's
// Store satisfies it, and a test double satisfies it without a datastore.
type Client interface {
	ConfigVersion(ctx context.Context) (int64, error)
	TenantVersions(ctx context.Context) (map[string]int64, error)
	SubscribeInvalidations(ctx context.Context) (InvalidationSource, error)
}

// InvalidationSource is a subscription to the invalidation channel as the
// snapshot sees it. internal/store's InvalidationStream satisfies it.
type InvalidationSource interface {
	Messages() <-chan string
	Close() error
}

// Tenant is one cached tenant row, the payload a miss loads and a hit resolves.
// Plan is the interned plan the cache shares between tenants.
type Tenant struct {
	ID         string
	ExternalID string
	State      string
	Timezone   string
	Anchor     time.Time
	Plan       *Plan
	Overrides  map[string]LoadedOverride
	Version    int64
}

// Snapshot is the bounded, invalidating projection the data plane resolves
// against. Its methods are safe for concurrent use; the resolve path touches
// the cache with a read lock and nothing else, and the miss path spends its
// budget from the limiter before a Loader call.
type Snapshot struct {
	mu       sync.RWMutex
	cache    *tenantCache
	features map[string]bool
	version  int64

	loader      Loader
	client      Client
	refresh     time.Duration
	ctrlTimeout time.Duration
	now         func() time.Time
	limiter     *rateLimiter
	metrics     *observability.Metrics
	hits        atomic.Uint64
	misses      atomic.Uint64
	stop        chan struct{}
	stopped     chan struct{}
	startOnce   sync.Once
	stopOnce    sync.Once
}

// New builds a snapshot. The cache is empty: the first miss loads a tenant
// (request-lifecycle.md section 3). A nil Metrics disables the metric
// side effects, keeping the package usable in tests that do not care.
func New(opts Options) *Snapshot {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if opts.LimitEntries <= 0 {
		opts.LimitEntries = 50_000
	}
	if opts.ByteCap <= 0 {
		opts.ByteCap = 64 << 20
	}
	if opts.MissRate <= 0 {
		opts.MissRate = 20
	}
	if opts.ControlPlaneTimeout <= 0 {
		opts.ControlPlaneTimeout = 3 * time.Second
	}
	s := &Snapshot{
		loader:      opts.Loader,
		client:      opts.Client,
		refresh:     opts.RefreshInterval,
		ctrlTimeout: opts.ControlPlaneTimeout,
		now:         now,
		metrics:     opts.Metrics,
		features:    map[string]bool{},
		stop:        make(chan struct{}),
		stopped:     make(chan struct{}),
	}
	s.limiter = newRateLimiter(opts.MissRate, now)
	s.cache = newTenantCache(opts.LimitEntries, opts.ByteCap)
	if opts.Metrics != nil {
		s.cache.onEvict = func(_ string) { opts.Metrics.CacheEvictions.Inc() }
	}
	return s
}

// Start begins the invalidation subscriber and the periodic reconcile. With a
// nil Client it marks itself stopped so Close is always safe; a process that
// has a datastore but no Loader (the cmd before IP-08) runs the loops and
// resolves nothing, which is the honest state between phases.
func (s *Snapshot) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		go s.subscriberLoop(ctx)
		go s.reconcileLoop()
	})
}

// WithMetrics binds a Metrics handle after construction, which the cmd needs
// when the hit-ratio callback must read this snapshot's counters. Call it once,
// before Start and before any Resolve; it is deliberately outside the hot path.
func (s *Snapshot) WithMetrics(m *observability.Metrics) {
	if m == nil {
		return
	}
	s.metrics = m
	s.cache.onEvict = func(string) { m.CacheEvictions.Inc() }
}

// Close stops the loops. It is idempotent.
func (s *Snapshot) Close() { s.stopOnce.Do(func() { close(s.stop) }) }

// Wait blocks until the loops have exited, for tests that need a deterministic
// teardown.
func (s *Snapshot) Wait() { <-s.stopped }

// HitRatio returns hits over hits-plus-misses, the scrape-time value of
// quotacore_config_cache_hit_ratio.
func (s *Snapshot) HitRatio() float64 {
	h := s.hits.Load()
	m := s.misses.Load()
	if h+m == 0 {
		return 0
	}
	return float64(h) / float64(h+m)
}

// Resolve decides what a request for a feature on a tenant is allowed to do,
// with no I/O on the hit path. A cached tenant is resolved and counted as a
// hit; anything else is a miss that the limiter bound (DR-039).
func (s *Snapshot) Resolve(tenantID, featureKey string) (Resolution, error) {
	if t := s.find(tenantID); t != nil {
		s.hits.Add(1)
		return s.decide(t, featureKey), nil
	}
	s.misses.Add(1)
	return s.resolveMiss(tenantID, featureKey)
}

func (s *Snapshot) find(tenantID string) *Tenant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cache.get(tenantID)
}

// resolveMiss is the bounded miss: refuse under the limiter, else one Loader
// call, cache the tenant, then decide. A concurrent loader may have won the
// race for the same tenant; re-checking under the write lock keeps the cache
// single-versioned without cancelling a bounded read.
func (s *Snapshot) resolveMiss(tenantID, featureKey string) (Resolution, error) {
	if !s.limiter.allow() {
		s.countMiss(observability.MissRefused)
		return Resolution{}, ErrMissRefused
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.ctrlTimeout)
	defer cancel()

	loaded, err := s.loader.Load(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrTenantUnknown) {
			s.countMiss(observability.MissUnknown)
			return Resolution{}, ErrTenantUnknown
		}
		s.countMiss(observability.MissError)
		return Resolution{}, fmt.Errorf("%w: %v", ErrRefreshFailed, err)
	}
	s.countMiss(observability.MissLoaded)
	s.fill(loaded)
	if t := s.find(tenantID); t != nil {
		return s.decide(t, featureKey), nil
	}
	return Resolution{}, ErrStaleSnapshot
}

func (s *Snapshot) countMiss(result string) {
	if s.metrics != nil {
		s.metrics.CacheMisses.WithLabelValues(result).Inc()
	}
}

// fill inserts or refreshes a loaded tenant under the write lock and reflects
// the cache size in the gauge. A nil plan is refused rather than cached: a
// tenant with no plan is configuration that never resolves, and caching it
// would be an expensive way to learn nothing.
func (s *Snapshot) fill(loaded *LoadedTenant) {
	if loaded.Plan == nil {
		return
	}
	plan := &Plan{
		ID:           loaded.Plan.ID,
		Key:          loaded.Plan.Key,
		Entitlements: loaded.Plan.Entitlements,
	}
	t := &Tenant{
		ID:         loaded.ID,
		ExternalID: loaded.ExternalID,
		State:      loaded.State,
		Timezone:   loaded.Timezone,
		Anchor:     loaded.Anchor,
		Plan:       plan,
		Overrides:  loaded.Overrides,
		Version:    loaded.Version,
	}
	s.mu.Lock()
	s.cache.add(t)
	if s.metrics != nil {
		s.metrics.CacheEntries.Set(float64(s.cache.len()))
	}
	s.mu.Unlock()
}

// decide applies the resolution order to a tenant that is in the cache:
// archived over suspended over in-plan over not-in-plan (DR-015), and that
// order lives here, in one place.
func (s *Snapshot) decide(t *Tenant, featureKey string) Resolution {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := Resolution{Timezone: t.Timezone, TenantVersion: t.Version}
	if t.State == "suspended" {
		r.Suspended = true
		return r
	}
	if s.features[featureKey] {
		r.Archived = true
		return r
	}
	e, inPlan := overrideOrPlan(t, featureKey)
	if !inPlan {
		return r
	}
	r.InPlan = true
	r.Limit = e.Limit
	r.Interval = e.Interval
	r.Anchor = t.Anchor
	return r
}

// overrideOrPlan resolves an entitlement the way the tenant row says: an
// override that sets a field wins that field, an override that leaves it nil
// inherits the plan's value, and the resolution is a lie if the override names
// a feature and the plan never did (DR-015: an override can never grant a
// feature the plan excludes).
func overrideOrPlan(t *Tenant, featureKey string) (Entitlement, bool) {
	planEnt, inPlan := t.Plan.Entitlements[featureKey]
	ov, overridden := t.Overrides[featureKey]
	if !overridden {
		return planEnt, inPlan
	}
	e := Entitlement{}
	if ov.Limit != nil {
		e.Limit = *ov.Limit
	} else if inPlan {
		e.Limit = planEnt.Limit
	}
	if ov.Interval != nil {
		e.Interval = *ov.Interval
	} else if inPlan {
		e.Interval = planEnt.Interval
	}
	return e, overridden && inPlan
}

// subscriberLoop applies invalidations off the channel. It re-subscribes on
// failure and keeps going until Close; a message lost here is recovered by the
// reconcile loop, and the lag observed here is what NFR-D3 budgets to a second.
func (s *Snapshot) subscriberLoop(ctx context.Context) {
	defer close(s.stopped)
	if s.client == nil {
		<-s.stop
		return
	}
	for {
		sub, err := s.client.SubscribeInvalidations(ctx)
		if err != nil {
			s.noteRefresh(false)
			if !sleepOrWait(s.stop, time.Second) {
				return
			}
			continue
		}
		received := s.now()
		msgs := sub.Messages()
	loop:
		for {
			select {
			case <-s.stop:
				sub.Close()
				return
			case payload, ok := <-msgs:
				if !ok {
					break loop
				}
				if s.applyInvalidation(payload) && s.metrics != nil {
					s.metrics.InvalidationLag.Observe(s.now().Sub(received).Seconds())
				}
				received = s.now()
			}
		}
		sub.Close()
	}
}

// reconcileLoop is the correctness backstop: a configuration change that no
// pub/sub message carried is applied within one RefreshInterval, so a lost
// message widens NFR-D3's tail but never breaks the contract
// (data-model.md section 3.4).
func (s *Snapshot) reconcileLoop() {
	if s.client == nil || s.refresh <= 0 {
		<-s.stop
		return
	}
	t := time.NewTicker(s.refresh)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.reconcile()
		}
	}
}

// reconcile reads the config version and the per-tenant versions and prunes to
// them. Whatever fails keeps the previous snapshot (DoD item 7): a control
// plane outage must never clear a cache that is still serving.
func (s *Snapshot) reconcile() {
	ctx, cancel := context.WithTimeout(context.Background(), s.ctrlTimeout)
	defer cancel()
	version, err := s.client.ConfigVersion(ctx)
	if err != nil {
		s.noteRefresh(false)
		return
	}
	latest, err := s.client.TenantVersions(ctx)
	if err != nil {
		s.noteRefresh(false)
		return
	}
	if version > s.version {
		s.mu.Lock()
		s.version = version
		s.mu.Unlock()
	}
	s.pruneTo(latest)
	s.noteRefresh(true)
}

// applyInvalidation handles one channel payload. It reports whether a genuine
// forward invalidation was applied, so the subscriber can observe the lag. A
// stale or unparsable version is discarded and counted, never applied (DoD
// item 5), and counts as a skipped refresh rather than an error: the control
// plane is not misbehaving, the message simply arrived late.
func (s *Snapshot) applyInvalidation(payload string) bool {
	version, err := parseInvalidation(payload)
	if err != nil {
		s.noteSkipped()
		return false
	}
	s.mu.RLock()
	stale := version <= s.version
	s.mu.RUnlock()
	if stale {
		s.noteSkipped()
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.ctrlTimeout)
	defer cancel()
	latest, err := s.client.TenantVersions(ctx)
	if err != nil {
		s.noteRefresh(false)
		return false
	}
	s.mu.Lock()
	s.version = version
	s.mu.Unlock()
	s.pruneTo(latest)
	s.noteRefresh(true)
	return true
}

// pruneTo drops every cached tenant whose effective version no longer matches
// the control plane, records the new global version, and reports the size.
func (s *Snapshot) pruneTo(latest map[string]int64) {
	s.mu.Lock()
	s.cache.prune(latest)
	if s.metrics != nil {
		s.metrics.CacheEntries.Set(float64(s.cache.len()))
	}
	s.mu.Unlock()
}

// noteRefresh is the single door for the refresh-outcome counter, so every
// reconcile and invalidation path counts exactly once.
func (s *Snapshot) noteRefresh(ok bool) {
	if s.metrics == nil {
		return
	}
	if ok {
		s.metrics.ControlPlaneRefresh.WithLabelValues(observability.RefreshOK).Inc()
	} else {
		s.metrics.ControlPlaneRefresh.WithLabelValues(observability.RefreshError).Inc()
	}
}

func (s *Snapshot) noteSkipped() {
	if s.metrics != nil {
		s.metrics.ControlPlaneRefresh.WithLabelValues(observability.RefreshSkipped).Inc()
	}
}

// sleepOrWait sleeps for d, or returns false when the snapshot is closed.
func sleepOrWait(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return false
	case <-time.After(d):
		return true
	}
}

// invalidationPayload mirrors the shape the control plane publishes
// (data-model.md section 3.4): {"version":"4471"}.
type invalidationPayload struct {
	Version string `json:"version"`
}

func parseInvalidation(payload string) (int64, error) {
	var p invalidationPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return 0, err
	}
	return strconv.ParseInt(p.Version, 10, 64)
}
