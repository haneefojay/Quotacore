package observability

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Intervals are the values of rolled_over_total's interval label, the six
// cadences of the contract. never is among them although it never rolls, so
// the closed set is the contract's and not whatever happened to roll.
var Intervals = []string{"hourly", "daily", "weekly", "monthly", "yearly", "never"}

// Cycles is the collector set IP-13 adds to observability.md section 1.1 for
// what a script's transition saw, plus the gauge of pending re-anchors IP-10
// handed on (section 1.4). Every script execution that carries a transition is
// observed once, by the runner, so no caller can count it a second time.
type Cycles struct {
	RolledOver       *prometheus.CounterVec // interval
	RolloverStale    prometheus.Counter
	CeilingViolation prometheus.Counter
	ReanchorsPending prometheus.Gauge

	// skew is the clock-skew threshold. A store one cycle ahead whose window
	// opened within it of the caller's reading is a boundary race between
	// synchronised instances, not a defect (IP-13 D3).
	skew time.Duration
}

// SkewThreshold is the clock-skew figure the stale counter and the clock-skew
// alert share. It is an operator default with no source (Q-26).
const SkewThreshold = time.Second

// NewCycles builds the collectors.
func NewCycles() *Cycles {
	c := &Cycles{
		RolledOver: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: Namespace, Name: "rolled_over_total",
			Help: "Cycle transitions applied by a script on the request path, by the entitlement's interval."}, []string{"interval"}),
		RolloverStale: prometheus.NewCounter(prometheus.CounterOpts{Namespace: Namespace, Name: "rollover_stale_total",
			Help: "Transitions refused because the stored cycle index was ahead of the caller's. Non-zero is a clock or data defect (P1)."}),
		CeilingViolation: prometheus.NewCounter(prometheus.CounterOpts{Namespace: Namespace, Name: "ceiling_violation_total",
			Help: "Observed balance above limit plus bonus. Always zero; non-zero is a P0 (DR-019)."}),
		ReanchorsPending: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: Namespace, Name: "reanchors_pending",
			Help: "Tenant re-anchors committed in Postgres and not yet applied to the fast store, as of the last drain tick."}),
		skew: SkewThreshold,
	}
	for _, iv := range Intervals {
		c.RolledOver.WithLabelValues(iv)
	}
	return c
}

// Transition is what one script execution reported, with the window its
// caller asked about. The observability package takes it as plain values, so it
// needs to know nothing about the scripts.
type Transition struct {
	Interval string
	Rolled   bool
	Stale    bool
	Replayed bool
	// Target is the index the caller computed, Index and Start the cycle the
	// store is on, and At the caller's clock reading.
	Target, Index int64
	Start, At     time.Time
	// Balance, Limit and Bonus are the state the execution left.
	Balance, Limit, Bonus int64
}

// Observe counts one execution. A replay transitioned nothing, so it counts
// nothing.
func (c *Cycles) Observe(t Transition) {
	if c == nil || t.Replayed {
		return
	}
	if t.Rolled {
		c.RolledOver.WithLabelValues(t.Interval).Inc()
	}
	if t.Stale && t.Index > t.Target && !c.race(t) {
		c.RolloverStale.Inc()
	}
	c.Ceiling(t.Balance, t.Limit, t.Bonus)
}

// race is the one stale verdict that is not a defect: the store is one cycle
// ahead, and that cycle opened less than the skew threshold after the caller's
// reading, which is what two correctly synchronised instances produce at a
// boundary.
func (c *Cycles) race(t Transition) bool {
	if t.Index != t.Target+1 {
		return false
	}
	lead := t.Start.Sub(t.At)
	return lead > 0 && lead <= c.skew
}

// Ceiling counts a state that breaks DR-019's ceiling, wherever it was read.
func (c *Cycles) Ceiling(balance, limit, bonus int64) {
	if c != nil && balance > limit+bonus {
		c.CeilingViolation.Inc()
	}
}

// Register adds the four collectors to r.
func (c *Cycles) Register(r prometheus.Registerer) error {
	return register(r, c.RolledOver, c.RolloverStale, c.CeilingViolation, c.ReanchorsPending)
}

func register(r prometheus.Registerer, cs ...prometheus.Collector) error {
	for _, c := range cs {
		if err := r.Register(c); err != nil {
			return err
		}
	}
	return nil
}
