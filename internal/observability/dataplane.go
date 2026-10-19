package observability

import "github.com/prometheus/client_golang/prometheus"

// The data-plane outcome label values (observability.md section 1.1).
// rejected is a 5xx: the service failed closed. A 4xx other than
// quota_exceeded is the caller's error and is counted in
// quotacore_errors_total only.
const (
	OutcomeAllowed  = "allowed"
	OutcomeDenied   = "denied"
	OutcomeReplayed = "replayed"
	OutcomeRejected = "rejected"
)

// DataPlane is the collector set IP-07 owns: the enforcement counters and the
// two series every error and every request lands in. It is separate from
// Metrics so the snapshot's seven collectors stay exactly seven.
//
// No label carries a tenant id, an external_id, a request id or metadata
// (observability.md section 1.7). feature_key is bounded by NFR-T5, code and
// endpoint by the contract, and key_prefix by the number of keys.
type DataPlane struct {
	Errors            *prometheus.CounterVec   // code, endpoint
	Consume           *prometheus.CounterVec   // outcome
	Refund            *prometheus.CounterVec   // outcome
	Check             *prometheus.CounterVec   // outcome
	ConsumeAmount     *prometheus.CounterVec   // feature_key
	Latency           *prometheus.HistogramVec // endpoint, outcome
	BalanceKeyMissing prometheus.Counter
	RateLimited       *prometheus.CounterVec // key_prefix
}

// LatencyBuckets resolve the SLOs of NFR-L1 to NFR-L4 directly: 1 ms, 3 ms,
// 5 ms and 250 ms are bucket bounds, so "p99 under 5 ms" needs no
// interpolation.
var LatencyBuckets = []float64{0.0005, 0.001, 0.002, 0.003, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}

// NewDataPlane builds the collectors. errorCells lists every (code, endpoint)
// pair the contract allows, each warmed at zero so the code label set is the
// catalogue from the first scrape (IP-07 DoD 7).
func NewDataPlane(errorCells [][2]string) *DataPlane {
	vec := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: Namespace, Name: name, Help: help}, labels)
	}
	d := &DataPlane{
		Errors: vec("errors_total",
			"Error envelopes written, by code and by the contract operation that wrote them.", "code", "endpoint"),
		Consume: vec("consume_total", "Every consume call, by outcome.", "outcome"),
		Refund:  vec("refund_total", "Every refund call, by outcome.", "outcome"),
		Check:   vec("check_total", "Advisory checks, by outcome.", "outcome"),
		ConsumeAmount: vec("consume_amount_total",
			"Units consumed by applied consume calls, by feature.", "feature_key"),
		Latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Name: "latency_seconds",
			Help:    "Time from accepting a request to writing its response, by operation and outcome.",
			Buckets: LatencyBuckets,
		}, []string{"endpoint", "outcome"}),
		BalanceKeyMissing: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Name: "balance_key_missing_total",
			Help: "Requests failed closed with 503 because a provisioned balance hash was absent (DR-045). Any occurrence is a P0.",
		}),
		RateLimited: vec("rate_limited_total",
			"Requests refused with 429 rate_limited, by the key's public prefix (DR-040).", "key_prefix"),
	}
	for _, cell := range errorCells {
		d.Errors.WithLabelValues(cell[0], cell[1])
	}
	for _, o := range []string{OutcomeAllowed, OutcomeDenied, OutcomeReplayed, OutcomeRejected} {
		d.Consume.WithLabelValues(o)
		d.Refund.WithLabelValues(o)
	}
	for _, o := range []string{OutcomeAllowed, OutcomeDenied} {
		d.Check.WithLabelValues(o)
	}
	return d
}

// Register adds the collectors to r.
func (d *DataPlane) Register(r prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{
		d.Errors, d.Consume, d.Refund, d.Check, d.ConsumeAmount, d.Latency, d.BalanceKeyMissing, d.RateLimited,
	} {
		if err := r.Register(c); err != nil {
			return err
		}
	}
	return nil
}
