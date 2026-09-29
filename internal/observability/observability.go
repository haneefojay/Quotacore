// Package observability owns the metrics the two planes emit. The metric names
// and their labels are the catalogue in observability.md section 1.4; this
// package is the code mirror of that table, and the seven collectors below are
// the whole of it. IP-13 mounts a /metrics endpoint; this package only defines
// the collectors and a Register method, so a process that does not mount the
// route (every process before IP-13) pays nothing extra and the collectors are
// still testable.
//
// The data plane must not grow an unbounded label (observability.md section
// 1.5): every label that exists here has its value set chosen from a closed
// list, and the one per-tenant series the data plane would invite is excluded
// by construction.
package observability

import (
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// Namespace is the single product prefix for every metric (observability.md
	// section 1.4); no other prefix exists.
	Namespace = "quotacore"

	// MissResult values are the closed set of what a bounded miss can be.
	// `loaded` the loader supplied the tenant, `unknown` the loader proved the
	// tenant does not exist (404 territory), `refused` the miss limiter said no
	// (fail-closed 503), `error` the control plane was unreachable (503) or the
	// refresh machinery failed.
	MissLoaded  = "loaded"
	MissUnknown = "unknown"
	MissRefused = "refused"
	MissError   = "error"

	// RefreshResult values are the closed set of what a control-plane refresh
	// can be: an invalidation, a periodic reconcile, a feature-registry load.
	RefreshOK      = "ok"
	RefreshError   = "error"
	RefreshSkipped = "skipped"
)

// Metrics is the seven collectors of observability.md section 1.4, as a unit so
// that "these and only these" is asserted once, on any registry.
type Metrics struct {
	// CacheEntries is the number of tenants currently in the snapshot, an
	// upper-bounded gauge whose limit is QUOTACORE_CONFIG_CACHE_ENTRIES.
	CacheEntries prometheus.Gauge

	// CacheEvictions is the count of tenants evicted to respect the cache's
	// entry and byte bounds.
	CacheEvictions prometheus.Counter

	// CacheHitRatio is hits over hits-plus-misses, computed on scrape from the
	// data plane's own counters, so it is always self-consistent with them.
	CacheHitRatio prometheus.GaugeFunc

	// CacheMisses counts bounded misses by outcome. Its leading-indicator role
	// is documented in observability.md section 1.4: a rise under flat traffic
	// is the first sign a control-plane outage is reaching the data plane.
	CacheMisses *prometheus.CounterVec

	// InvalidationLag measures, in seconds, how long it took from an
	// invalidation message arriving on the channel to the snapshot being
	// consistent with it. The p99 budget is NFR-D3, one second.
	InvalidationLag prometheus.Histogram

	// ControlPlaneRefresh counts control-plane reads by outcome: invalidation
	// applies, periodic reconciles and feature-registry loads succeed, fail or
	// are skipped as stale.
	ControlPlaneRefresh *prometheus.CounterVec

	// DBPoolWait is the time the control-plane connection pool spends waiting
	// for a connection (observability.md section 1.4, quotation in
	// deployment.md section 4). It is the mechanism by which a control-plane
	// problem could reach the data plane, and the thing to watch exactly
	// because that coupling must not exist.
	DBPoolWait prometheus.Histogram
}

// Options to New. HitRatio supplies the callback a scrape invokes; it reads the
// data plane's hit and miss counters directly.
type Options struct {
	HitRatio func() float64
}

// New builds the seven collectors. It does not register them; Register does
// that, so tests and the eventual /metrics handler share one decision.
func New(opts Options) *Metrics {
	gauge := func(name, help string) prometheus.Gauge {
		return prometheus.NewGauge(prometheus.GaugeOpts{Namespace: Namespace, Name: name, Help: help})
	}
	counter := func(name, help string) prometheus.Counter {
		return prometheus.NewCounter(prometheus.CounterOpts{Namespace: Namespace, Name: name, Help: help})
	}
	counterVec := func(name, help, label string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: Namespace, Name: name, Help: help}, []string{label})
	}
	histogram := func(name, help string, buckets []float64) prometheus.Histogram {
		return prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: Namespace, Name: name, Help: help, Buckets: buckets})
	}
	hitRatio := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: Namespace,
		Name:      "config_cache_hit_ratio",
		Help:      "Snapshot cache hit ratio, computed on scrape as hits over hits plus misses.",
	}, opts.HitRatio)
	cacheMisses := counterVec("config_cache_miss_total",
		"Bounded configuration-cache misses by outcome; the leading indicator of a control-plane outage reaching the data plane.",
		"result")
	controlPlaneRefresh := counterVec("controlplane_refresh_total",
		"Control-plane reads by outcome: invalidation applies, reconciles and feature-registry loads.",
		"result")
	// Warm the closed label sets to their zero values, so a metric with nothing
	// to say still appears in a scrape. A missing series and a zero series are
	// indistinguishable in an alert and different on a dashboard; the label
	// values are a contract (MissResult, RefreshResult above), and warming them
	// pins that set here.
	for _, v := range []string{MissLoaded, MissUnknown, MissRefused, MissError} {
		cacheMisses.WithLabelValues(v)
	}
	for _, v := range []string{RefreshOK, RefreshError, RefreshSkipped} {
		controlPlaneRefresh.WithLabelValues(v)
	}
	return &Metrics{
		CacheEntries:   gauge("config_cache_entries", "Tenants currently in the snapshot, bounded by QUOTACORE_CONFIG_CACHE_ENTRIES."),
		CacheEvictions: counter("config_cache_evictions_total", "Tenants evicted to respect the snapshot's entry and byte bounds."),
		CacheHitRatio:  hitRatio,
		CacheMisses:    cacheMisses,
		InvalidationLag: histogram("invalidation_lag_seconds",
			"Time from an invalidation message arriving to the snapshot being consistent with it.",
			[]float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5}),
		ControlPlaneRefresh: controlPlaneRefresh,
		DBPoolWait: histogram("dbpool_wait_seconds",
			"The control-plane connection pool's time spent waiting for a connection.",
			[]float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2}),
	}
}

// Register adds the seven collectors to r. A second call fails: a collector can
// be registered once, which is a property tests rely on to prove "exactly
// seven".
func (m *Metrics) Register(r prometheus.Registerer) error {
	collectors := []prometheus.Collector{
		m.CacheEntries, m.CacheEvictions, m.CacheHitRatio, m.CacheMisses,
		m.InvalidationLag, m.ControlPlaneRefresh, m.DBPoolWait,
	}
	for _, c := range collectors {
		if err := r.Register(c); err != nil {
			return err
		}
	}
	return nil
}
