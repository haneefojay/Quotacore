package observability

import (
	"math"
	"runtime"
	"runtime/metrics"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// MethodOther is the method label of a request whose method no contract
// route uses. A raw method is text the caller chooses, so it is never a label
// value (observability.md section 1.7, IP-13 D5).
const MethodOther = "other"

// HTTP is the request half of observability.md section 1.6: every request on
// every route, the probes and the unmatched path included.
type HTTP struct {
	Requests *prometheus.CounterVec   // endpoint, method, status
	Duration *prometheus.HistogramVec // endpoint
}

// NewHTTP builds the collectors.
func NewHTTP() *HTTP {
	return &HTTP{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: Namespace, Name: "http_requests_total",
			Help: "Every HTTP request, by contract operation, method and status."}, []string{"endpoint", "method", "status"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: Namespace,
			Name: "http_duration_seconds", Help: "Time from accepting a request to writing its response, by contract operation.",
			Buckets: LatencyBuckets}, []string{"endpoint"}),
	}
}

// Observe counts one request. status is the HTTP status written.
func (h *HTTP) Observe(endpoint, method string, status int, elapsed time.Duration) {
	if h == nil {
		return
	}
	h.Requests.WithLabelValues(endpoint, method, strconv.Itoa(status)).Inc()
	h.Duration.WithLabelValues(endpoint).Observe(elapsed.Seconds())
}

// Register adds the two collectors to r.
func (h *HTTP) Register(r prometheus.Registerer) error {
	return register(r, h.Requests, h.Duration)
}

// Build is what quotacore_build_info reports.
type Build struct {
	Version, Commit, ZoneDataVersion string
}

// Process is the process half of observability.md section 1.6, under the
// names that section gives rather than the client library's go_* names, which
// are not registered (IP-13 D2). Each is read when scraped.
type Process struct {
	collectors []prometheus.Collector
}

// GCPauseBuckets bound quotacore_gc_pause_seconds.
var GCPauseBuckets = []float64{0.00001, 0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.05, 0.1}

// NewProcess builds the five collectors; started is when the process started.
func NewProcess(b Build, started time.Time) *Process {
	info := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: Namespace, Name: "build_info",
		Help:        "1, labelled with the build's version, commit and zone-data version.",
		ConstLabels: prometheus.Labels{"version": b.Version, "commit": b.Commit, "zone_data_version": b.ZoneDataVersion}})
	info.Set(1)
	return &Process{collectors: []prometheus.Collector{
		info,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: Namespace, Name: "goroutines",
			Help: "Goroutines that currently exist."}, func() float64 { return float64(runtime.NumGoroutine()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: Namespace, Name: "memory_bytes",
			Help: "Memory the Go runtime holds from the operating system, less what it has released."}, memoryBytes),
		gcPauses{desc: prometheus.NewDesc(prometheus.BuildFQName(Namespace, "", "gc_pause_seconds"),
			"Stop-the-world pauses for garbage collection since the process started.", nil, nil)},
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: Namespace, Name: "uptime_seconds",
			Help: "Seconds since this process started."}, func() float64 { return time.Since(started).Seconds() }),
	}}
}

// Register adds the five collectors to r.
func (p *Process) Register(r prometheus.Registerer) error {
	return register(r, p.collectors...)
}

func memoryBytes() float64 {
	s := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 || s[1].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return float64(s[0].Value.Uint64() - s[1].Value.Uint64())
}

// gcPauses re-buckets the runtime's own pause histogram on each scrape. The
// runtime keeps no sum, so the sum is estimated from each runtime bucket's
// midpoint, which is what its documentation recommends.
type gcPauses struct{ desc *prometheus.Desc }

func (g gcPauses) Describe(ch chan<- *prometheus.Desc) { ch <- g.desc }

func (g gcPauses) Collect(ch chan<- prometheus.Metric) {
	s := []metrics.Sample{{Name: "/sched/pauses/total/gc:seconds"}}
	metrics.Read(s)
	buckets := map[float64]uint64{}
	for _, b := range GCPauseBuckets {
		buckets[b] = 0
	}
	var count uint64
	var sum float64
	if s[0].Value.Kind() == metrics.KindFloat64Histogram {
		h := s[0].Value.Float64Histogram()
		for i, n := range h.Counts {
			if n == 0 {
				continue
			}
			lo, hi := h.Buckets[i], h.Buckets[i+1]
			count += n
			mid := hi
			switch {
			case math.IsInf(lo, -1):
				mid = hi
			case math.IsInf(hi, 1):
				mid = lo
			default:
				mid = (lo + hi) / 2
			}
			sum += mid * float64(n)
			for _, b := range GCPauseBuckets {
				if hi <= b {
					buckets[b] += n
				}
			}
		}
	}
	ch <- prometheus.MustNewConstHistogram(g.desc, count, sum, buckets)
}
