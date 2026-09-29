package observability

import (
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestSevenCollectorsAndNoMore pins the catalogue of observability.md section
// 1.4 to the registry: the seven names below and nothing else. A metric is as
// much a contract as a rule identifier — observability.md is the stated-once
// home, this registry is the code mirror, and a metric that exists in only one
// is a drift the checker cannot see (metrics are not identifiers).
func TestSevenCollectorsAndNoMore(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := New(Options{HitRatio: func() float64 { return 0.5 }}).Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, f.GetName())
	}
	sort.Strings(names)
	want := []string{
		"quotacore_config_cache_entries",
		"quotacore_config_cache_evictions_total",
		"quotacore_config_cache_hit_ratio",
		"quotacore_config_cache_miss_total",
		"quotacore_controlplane_refresh_total",
		"quotacore_dbpool_wait_seconds",
		"quotacore_invalidation_lag_seconds",
	}
	if len(names) != len(want) {
		t.Errorf("metric families = %v, want exactly %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("metric family %d = %q, want %q", i, names[i], want[i])
		}
	}
}

// TestRegisterIsPerRegistry asserts a collector registered twice is a failure,
// which is the property ResolveGaugeFuncExistingName relies on when it proves a
// registry holds exactly these collectors.
func TestRegisterIsPerRegistry(t *testing.T) {
	m := New(Options{HitRatio: func() float64 { return 0 }})
	reg := prometheus.NewRegistry()
	if err := m.Register(reg); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := m.Register(reg); err == nil {
		t.Error("a second Register on one registry must fail")
	}
}

// TestHitRatioCallbackIsConsumed verifies the callback is what a scrape reads,
// so the gauge cannot go stale against the data plane's counters.
func TestHitRatioCallbackIsConsumed(t *testing.T) {
	reg := prometheus.NewRegistry()
	const wantRatio = 0.75
	if err := New(Options{HitRatio: func() float64 { return wantRatio }}).Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "quotacore_config_cache_hit_ratio" {
			continue
		}
		if got := f.GetMetric()[0].GetGauge().GetValue(); got != wantRatio {
			t.Errorf("hit ratio = %v, want %v", got, wantRatio)
		}
		return
	}
	t.Error("quotacore_config_cache_hit_ratio was not gathered")
}
