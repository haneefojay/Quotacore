package observability

import "github.com/prometheus/client_golang/prometheus"

// The classes of quotacore_datastore_errors_total (observability.md section
// 1.3), a closed set. other is for an error none of the four describes, so no
// store error goes uncounted (IP-13 S3).
const (
	ClassTimeout  = "timeout"
	ClassNoScript = "noscript"
	ClassOOM      = "oom"
	ClassRefused  = "refused"
	ClassOther    = "other"
)

// The values of quotacore_script_loads_total's command label: a script run by
// its digest, and a script body sent to the store, which the runner does only
// with SCRIPT LOAD at start and after a NOSCRIPT (IP-13 S2).
const (
	CommandDigest = "evalsha"
	CommandBody   = "eval"
)

// Dependencies is the collector set IP-13 adds for observability.md section
// 1.3, beside the pool wait the snapshot's set already holds: the data store's
// health, latency, errors and memory, the script cache, Postgres's health, and
// the clock's agreement with the store's.
type Dependencies struct {
	DataStoreUp      prometheus.Gauge
	DataStoreLatency prometheus.Histogram
	DataStoreErrors  *prometheus.CounterVec // class
	MemoryUsedRatio  prometheus.Gauge
	ControlPlaneUp   prometheus.Gauge
	ScriptLoads      *prometheus.CounterVec // command
	ScriptCacheHits  prometheus.Counter
	ClockSkew        prometheus.Gauge
}

// NewDependencies builds the collectors.
func NewDependencies() *Dependencies {
	d := &Dependencies{
		DataStoreUp: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: Namespace, Name: "datastore_up",
			Help: "1 when the data store answered the last probe within QUOTACORE_DATASCRIPT_TIMEOUT."}),
		DataStoreLatency: prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: Namespace,
			Name: "datastore_latency_seconds", Help: "Round-trip time of every data-store command.",
			Buckets: LatencyBuckets}),
		DataStoreErrors: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: Namespace,
			Name: "datastore_errors_total", Help: "Data-store command failures, by class."}, []string{"class"}),
		MemoryUsedRatio: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: Namespace,
			Name: "datastore_memory_used_ratio", Help: "The data store's used memory over its maxmemory; 0 when maxmemory is unset."}),
		ControlPlaneUp: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: Namespace, Name: "controlplane_up",
			Help: "1 when Postgres answered the last probe."}),
		ScriptLoads: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: Namespace, Name: "script_loads_total",
			Help: "Scripts run by digest (evalsha) and script bodies sent to the store (eval)."}, []string{"command"}),
		ScriptCacheHits: prometheus.NewCounter(prometheus.CounterOpts{Namespace: Namespace, Name: "script_cache_hits_total",
			Help: "Scripts run by digest that the store found in its script cache."}),
		ClockSkew: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: Namespace, Name: "clock_skew_seconds",
			Help: "This process's clock minus the data store's, corrected for half the probe's round trip."}),
	}
	for _, c := range []string{ClassTimeout, ClassNoScript, ClassOOM, ClassRefused, ClassOther} {
		d.DataStoreErrors.WithLabelValues(c)
	}
	for _, c := range []string{CommandDigest, CommandBody} {
		d.ScriptLoads.WithLabelValues(c)
	}
	return d
}

// Register adds the eight collectors to r.
func (d *Dependencies) Register(r prometheus.Registerer) error {
	return register(r, d.DataStoreUp, d.DataStoreLatency, d.DataStoreErrors, d.MemoryUsedRatio,
		d.ControlPlaneUp, d.ScriptLoads, d.ScriptCacheHits, d.ClockSkew)
}
