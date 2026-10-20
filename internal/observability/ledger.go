package observability

import "github.com/prometheus/client_golang/prometheus"

// The drop reasons of quotacore_event_sink_dropped_total (observability.md
// section 1.5), a closed set: a full queue, a row that could not even be
// dead-lettered, and an event still queued when the process stopped.
const (
	DropQueueFull = "queue_full"
	DropError     = "error"
	DropShutdown  = "shutdown"
)

// The dead-letter reasons, the values of usage_event_dead_letters.reason
// (data-model.md section 2.12).
const (
	DeadDuplicate = "duplicate_idempotency_key"
	DeadRejected  = "rejected"
)

// Ledger is the collector set IP-12 owns: the ledger rows of observability.md
// section 1.5 that the writer can know, plus the two dead-letter series. It is
// separate from Metrics and DataPlane so each set stays the set its phase
// asserts. The reversal and reconciliation counters of the same table are
// IP-11's.
type Ledger struct {
	Enqueued          prometheus.Counter
	Written           prometheus.Counter
	Dropped           *prometheus.CounterVec // reason
	QueueDepth        prometheus.GaugeFunc
	WriteLatency      prometheus.Histogram
	DuplicateDetected prometheus.Counter
	DeadLettered      *prometheus.CounterVec // reason
	DeadLetters       *prometheus.GaugeVec   // reason
}

// NewLedger builds the collectors. depth is read on scrape, so the gauge is
// the queue's length at that instant rather than a value someone remembered
// to set.
func NewLedger(depth func() float64) *Ledger {
	counter := func(name, help string) prometheus.Counter {
		return prometheus.NewCounter(prometheus.CounterOpts{Namespace: Namespace, Name: name, Help: help})
	}
	l := &Ledger{
		Enqueued: counter("event_enqueued_total", "Ledger events accepted onto the bounded queue."),
		Written:  counter("event_written_total", "Ledger rows actually appended."),
		Dropped: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: Namespace, Name: "event_sink_dropped_total",
			Help: "Ledger events dropped, by reason. Non-zero is a visible gap in history (FS-10)."}, []string{"reason"}),
		QueueDepth: prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: Namespace, Name: "event_queue_depth",
			Help: "Ledger events queued and not yet written."}, depth),
		WriteLatency: prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: Namespace,
			Name: "event_write_latency_seconds", Help: "Time to write one ledger batch.",
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}}),
		DuplicateDetected: counter("event_duplicate_detected_total",
			"Second deductions under one idempotency key that the ledger's partial unique index refused (DR-030, FS-22)."),
		DeadLettered: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: Namespace, Name: "event_dead_lettered_total",
			Help: "Ledger events refused and kept in the dead-letter table, by reason (INV-U3)."}, []string{"reason"}),
		DeadLetters: prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: Namespace, Name: "event_dead_letters",
			Help: "Dead-letter rows outstanding, by reason, as of the last retention tick."}, []string{"reason"}),
	}
	for _, r := range []string{DropQueueFull, DropError, DropShutdown} {
		l.Dropped.WithLabelValues(r)
	}
	for _, r := range []string{DeadDuplicate, DeadRejected} {
		l.DeadLettered.WithLabelValues(r)
		l.DeadLetters.WithLabelValues(r)
	}
	return l
}

// Register adds the eight collectors to r.
func (l *Ledger) Register(r prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{l.Enqueued, l.Written, l.Dropped, l.QueueDepth, l.WriteLatency,
		l.DuplicateDetected, l.DeadLettered, l.DeadLetters} {
		if err := r.Register(c); err != nil {
			return err
		}
	}
	return nil
}
