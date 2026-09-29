package snapshot

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/quotacore/quotacore/internal/observability"
	"github.com/quotacore/quotacore/internal/store"
)

// liveClient adapts the real store to Client, the same shape the cmd uses.
type liveClient struct{ *store.Store }

func (c liveClient) SubscribeInvalidations(ctx context.Context) (InvalidationSource, error) {
	return c.Store.SubscribeInvalidations(ctx)
}

// TestInvalidationLagWithinNFRD3 is the NFR-D3 evidence with the real
// datastore: a change the control plane commits and announces is visible to a
// newcomer within one second, and the machinery recorded it as a successful
// refresh. It measures wall clock from publish to eviction, which is the
// promise the phase makes.
func TestInvalidationLagWithinNFRD3(t *testing.T) {
	url := os.Getenv("QUOTACORE_REDIS_URL")
	if os.Getenv("QUOTACORE_REQUIRE_INTEGRATION") != "" && url == "" {
		t.Fatal("QUOTACORE_REQUIRE_INTEGRATION is set but QUOTACORE_REDIS_URL is unset, so this test would skip silently")
	}
	if url == "" {
		t.Skip("QUOTACORE_REDIS_URL is unset, so there is no datastore to talk to")
	}
	ctx := context.Background()
	s, err := store.Open(store.Options{URL: url, DataScriptTimeout: 250 * time.Millisecond})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// The control plane's commit order: qc:cfg:version, qc:cfg:tenants, then the
	// announcement. The snapshot entry for the tenant loads at version 1, so the
	// bump to 2 is exactly the staleness an invalidation must evict.
	seed := func(version int64, tenantVersion int64) {
		_ = s.Client().Set(ctx, store.ConfigVersionKey(), its(version), 0).Err()
		_ = s.Client().HSet(ctx, store.ConfigTenantsKey(), tenantID, its(tenantVersion)).Err()
	}
	seed(1, 1)
	defer s.Client().Del(ctx, store.ConfigVersionKey(), store.ConfigTenantsKey())

	fx := tenantFixture(tenantID)
	fx.Version = 1
	loader := &fakeLoader{byID: map[string]*LoadedTenant{tenantID: fx}}
	metrics := observability.New(observability.Options{HitRatio: func() float64 { return 0 }})
	snap := New(Options{
		Loader:              loader,
		Client:              liveClient{s},
		MissRate:            1e9,
		RefreshInterval:     time.Minute, // the subscriber is the path under test
		ControlPlaneTimeout: 3 * time.Second,
	})
	snap.WithMetrics(metrics)
	snap.Start(ctx)
	defer snap.Close()

	if _, err := snap.Resolve(tenantID, "feature_a"); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}
	if snap.cache.get(tenantID) == nil {
		t.Fatal("the tenant should be cached after the first resolve")
	}

	seed(2, 2)
	start := time.Now()
	if err := s.PublishInvalidation(ctx, 2); err != nil {
		t.Fatalf("publish: %v", err)
	}
	deadline := time.After(1 * time.Second)
	for snap.cache.get(tenantID) != nil {
		select {
		case <-deadline:
			t.Fatalf("the tenant was still cached %.2f s after publish, over NFR-D3's second", time.Since(start).Seconds())
		case <-time.After(5 * time.Millisecond):
		}
	}
	elapsed := time.Since(start)
	if elapsed >= time.Second {
		t.Fatalf("invalidation visible in %.3f s, over NFR-D3's one-second budget", elapsed.Seconds())
	}
	if got := testutil.ToFloat64(metrics.ControlPlaneRefresh.WithLabelValues(observability.RefreshOK)); got < 1 {
		t.Errorf("the applied invalidation was not counted as a successful refresh, count = %v", got)
	}
}

func its(v int64) string { return strconv.FormatInt(v, 10) }
