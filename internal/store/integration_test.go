package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// integrationRedis returns the URL from the environment, or skips the test with
// the same rule db_test.go uses: an explicitly-requested integration
// environment that lacks the URL is a failure, not a silent skip, so a green
// pipeline cannot have skipped these tests by accident.
func integrationRedis(t *testing.T) (string, bool) {
	t.Helper()
	url := os.Getenv("QUOTACORE_REDIS_URL")
	required := os.Getenv("QUOTACORE_REQUIRE_INTEGRATION") != ""
	if url == "" {
		if required {
			t.Fatal("QUOTACORE_REQUIRE_INTEGRATION is set but QUOTACORE_REDIS_URL is unset, so this test would skip silently")
		}
		t.Skip("QUOTACORE_REDIS_URL is unset, so there is no datastore to talk to")
	}
	return url, true
}

const invTestTenant = "df125e35-7a6d-4a63-9695-889af4db8da9"

// TestConfigNamespaceAndInvalidationRoundTrip is the store half of the IP-04
// evidence: the config keys live at the addresses data-model.md section 3.4
// names, the version reads understand them, and an invalidation published by
// the store's own method arrives on its own subscription with the documented
// payload shape.
func TestConfigNamespaceAndInvalidationRoundTrip(t *testing.T) {
	url, ok := integrationRedis(t)
	if !ok {
		return
	}
	ctx := context.Background()
	s, err := Open(Options{URL: url, DataScriptTimeout: 250 * time.Millisecond})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	cleanup := func() {
		_ = s.Client().Del(ctx, ConfigVersionKey(), ConfigTenantsKey())
	}
	cleanup()
	defer cleanup()

	// Seed the way the control plane will: version first, then the tenant map.
	if err := s.Client().Set(ctx, ConfigVersionKey(), "1", 0).Err(); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	if err := s.Client().HSet(ctx, ConfigTenantsKey(), invTestTenant, "1").Err(); err != nil {
		t.Fatalf("seed tenants: %v", err)
	}

	if v, err := s.ConfigVersion(ctx); err != nil || v != 1 {
		t.Fatalf("ConfigVersion = %d, %v; want 1", v, err)
	}
	tenants, err := s.TenantVersions(ctx)
	if err != nil {
		t.Fatalf("TenantVersions: %v", err)
	}
	if tenants[invTestTenant] != 1 || len(tenants) != 1 {
		t.Errorf("TenantVersions = %v, want exactly {%s:1}", tenants, invTestTenant)
	}

	sub, err := s.SubscribeInvalidations(ctx)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()
	if err := s.PublishInvalidation(ctx, 2); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case payload := <-sub.Messages():
		want := "{\"version\":\"2\"}"
		if payload != want {
			t.Errorf("payload = %q, want %q (the shape data-model.md section 3.4 states)", payload, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no invalidation arrived within 3s")
	}
}
