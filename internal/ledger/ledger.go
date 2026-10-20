// Package ledger is the usage ledger's queue and writer (ADR-0016,
// request-lifecycle.md step 9): a bounded channel the request path hands
// events to without waiting, and one goroutine that drains it in batches.
//
// It holds no SQL and no data-store handle. What it writes through is the
// Appender interface, which internal/db implements, so the writer appends and
// has no path to a balance (security-model.md section 2, IP-12 DoD 7), and
// internal/store's source check keeps it that way.
package ledger

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/observability"
)

// Appended is what one Append did with a batch.
type Appended struct {
	// Written is the rows newly inserted into usage_events.
	Written int
	// Present is the events whose event_id was already in the ledger: a
	// redelivery, which changes nothing (INV-W1's ledger reading).
	Present int
	// Dead is the events the ledger refused and that were kept as dead
	// letters in the same transaction, with the reason each was refused.
	// An event already dead-lettered is not listed again.
	Dead []Dead
}

// Dead is one event the ledger refused.
type Dead struct {
	ID     uuid.UUID
	Reason string // observability.DeadDuplicate or observability.DeadRejected
}

// Appender is the writer's one dependency, implemented by internal/db.
type Appender interface {
	// Append writes the batch in one transaction: each event is inserted,
	// found already present by its event_id, or dead-lettered. An error is
	// the whole batch failing, and nothing was committed.
	Append(ctx context.Context, events []dataplane.Event) (Appended, error)
	// DeadLetter keeps one event the ledger refused outright, with the
	// refusal. It reports whether the row is new.
	DeadLetter(ctx context.Context, ev dataplane.Event, cause error) (bool, error)
	// Permanent reports whether err is a refusal that a retry would meet
	// again, as opposed to a lost connection or a timeout.
	Permanent(err error) bool
}

// Gap is what the sink dropped for one tenant and feature over an interval,
// written to usage_event_gaps so a rebuild knows which cycles it cannot call
// exact (IP-11 D8). An empty TenantID is every tenant: what an overflowing
// tally writes.
type Gap struct {
	TenantID    string
	Feature     string
	Reason      string
	Events      int64
	First, Last time.Time
}

// Sessions is the writer's loss record (IP-11 D8), implemented by
// internal/db: one ledger_sessions row for the writer's life, refreshed on a
// heartbeat, and the gap tallies written with it.
type Sessions interface {
	OpenSession(ctx context.Context, id uuid.UUID, host string) error
	// Heartbeat records the gaps and refreshes the session in one
	// transaction. settledThrough is zero when it has not advanced.
	Heartbeat(ctx context.Context, id uuid.UUID, settledThrough time.Time, gaps []Gap) error
	// CloseSession records the gaps and closes the session after a clean
	// drain.
	CloseSession(ctx context.Context, id uuid.UUID, gaps []Gap) error
}

// maxTallies bounds the gap tally. One more tenant-feature than this is
// folded into a single tally with no tenant, meaning every tenant.
const maxTallies = 4096

// The writer's states, the values of Status.worker.
const (
	StateStopped  = "stopped"
	StateRunning  = "running"
	StateDraining = "draining"
)

// Options configure a Sink. The zero values are not the defaults; cmd/quotacore
// reads deployment.md section 4.4 and passes them.
type Options struct {
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	Appender      Appender
	Logger        *slog.Logger
	// MinBackoff and MaxBackoff bound the retry of a batch after a transient
	// error. Zero means 100 ms and 30 s.
	MinBackoff, MaxBackoff time.Duration
	// Sessions, when set, keeps the loss record (IP-11 D8); Host names the
	// session. Heartbeat is how often it is refreshed, zero meaning five
	// seconds, and Lag how long before an instant an accepted event may have
	// happened, zero meaning five seconds: the request's own time between its
	// accepted instant and its enqueue.
	Sessions  Sessions
	Host      string
	Heartbeat time.Duration
	Lag       time.Duration
}

// Sink is the bounded queue and its writer. It implements
// dataplane.EventSink.
type Sink struct {
	opt     Options
	ch      chan dataplane.Event
	metrics *observability.Ledger
	log     *slog.Logger

	// closed refuses new events once a shutdown has begun. Enqueue holds the
	// read side, so an event is either queued before the drain starts or
	// counted as a shutdown drop; it can never land in a channel nobody reads.
	mu     sync.RWMutex
	closed bool

	state    atomic.Value // string
	accepted atomic.Int64 // events put on the queue
	settled  atomic.Int64 // events written, present, dead-lettered or dropped by the writer
	dropped  atomic.Int64 // every drop, over every reason

	started chan struct{}
	stop    chan struct{} // closed to ask the writer to drain and return
	done    chan struct{} // closed when the writer has returned
	once    sync.Once

	// The loss record (IP-11 D8): the session, and the drops not yet written
	// as gaps, by tenant, feature and reason.
	session   uuid.UUID
	opened    bool
	settledAt time.Time
	tallyMu   sync.Mutex
	tallies   map[gapKey]*Gap
}

type gapKey struct{ tenant, feature, reason string }

// New builds a Sink. Run starts its writer.
func New(opt Options) *Sink {
	if opt.MinBackoff <= 0 {
		opt.MinBackoff = 100 * time.Millisecond
	}
	if opt.MaxBackoff <= 0 {
		opt.MaxBackoff = 30 * time.Second
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Heartbeat <= 0 {
		opt.Heartbeat = 5 * time.Second
	}
	if opt.Lag <= 0 {
		opt.Lag = 5 * time.Second
	}
	s := &Sink{
		opt:     opt,
		ch:      make(chan dataplane.Event, opt.QueueSize),
		log:     opt.Logger,
		started: make(chan struct{}),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		tallies: map[gapKey]*Gap{},
	}
	s.metrics = observability.NewLedger(func() float64 { return float64(len(s.ch)) })
	s.state.Store(StateStopped)
	return s
}

// Metrics is the sink's collector set, for a registry.
func (s *Sink) Metrics() *observability.Ledger { return s.metrics }

// State is running, draining or stopped (Status.worker, D8).
func (s *Sink) State() string { return s.state.Load().(string) }

// Depth is the queue's current length (Status.queue_depth).
func (s *Sink) Depth() int { return len(s.ch) }

// Dropped is every event this sink has dropped, over every reason
// (Status.ledger_dropped_total).
func (s *Sink) Dropped() int64 { return s.dropped.Load() }

// Enqueue hands an event to the queue without waiting. It never blocks and
// never fails the caller: a full queue drops the event and counts it, because
// the mutation has already been applied and the response written (FS-10).
func (s *Sink) Enqueue(ev dataplane.Event) {
	if ev.Delta == 0 {
		// Every call site already skips a zero delta (Q-22); one that reaches
		// here is a bug, and the table would refuse it anyway.
		s.log.Error("a ledger event with a zero delta was handed to the sink",
			"request_id", ev.RequestID, "event_type", ev.Type)
		s.drop(observability.DropError, ev)
		return
	}
	if ev.ID == (uuid.UUID{}) {
		id, err := uuid.NewV7()
		if err != nil {
			s.log.Error("could not mint an event_id", "request_id", ev.RequestID, "error", err)
			s.drop(observability.DropError, ev)
			return
		}
		ev.ID = id
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		s.drop(observability.DropShutdown, ev)
		return
	}
	select {
	case s.ch <- ev:
		s.accepted.Add(1)
		s.metrics.Enqueued.Inc()
	default:
		s.drop(observability.DropQueueFull, ev)
	}
}

// drop counts the events dropped and tallies them by tenant and feature, so
// the gap is recorded rather than only counted (IP-11 D8).
func (s *Sink) drop(reason string, evs ...dataplane.Event) {
	n := len(evs)
	s.dropped.Add(int64(n))
	s.metrics.Dropped.WithLabelValues(reason).Add(float64(n))
	s.tallyMu.Lock()
	defer s.tallyMu.Unlock()
	for _, ev := range evs {
		at := ev.OccurredAt
		if at.IsZero() {
			at = time.Now()
		}
		k := gapKey{ev.TenantID, ev.FeatureKey, reason}
		g, ok := s.tallies[k]
		if !ok && len(s.tallies) >= maxTallies {
			k = gapKey{"", "", reason}
			g, ok = s.tallies[k]
		}
		if !ok {
			g = &Gap{TenantID: k.tenant, Feature: k.feature, Reason: reason, First: at, Last: at}
			s.tallies[k] = g
		}
		g.Events++
		if at.Before(g.First) {
			g.First = at
		}
		if at.After(g.Last) {
			g.Last = at
		}
	}
}

// takeGaps empties the tally.
func (s *Sink) takeGaps() []Gap {
	s.tallyMu.Lock()
	defer s.tallyMu.Unlock()
	out := make([]Gap, 0, len(s.tallies))
	for _, g := range s.tallies {
		out = append(out, *g)
	}
	clear(s.tallies)
	return out
}

// restoreGaps puts gaps that could not be written back into the tally.
func (s *Sink) restoreGaps(gaps []Gap) {
	s.tallyMu.Lock()
	defer s.tallyMu.Unlock()
	for _, g := range gaps {
		k := gapKey{g.TenantID, g.Feature, g.Reason}
		if have, ok := s.tallies[k]; ok {
			have.Events += g.Events
			if g.First.Before(have.First) {
				have.First = g.First
			}
			if g.Last.After(have.Last) {
				have.Last = g.Last
			}
			continue
		}
		gg := g
		s.tallies[k] = &gg
	}
}

// heartbeat refreshes the session and writes the tally. settled_through
// advances only at an instant when everything accepted has been settled, less
// the lag a request can spend between its accepted instant and its enqueue,
// so every event that happened before it is written, dead-lettered or a gap.
func (s *Sink) heartbeat(ctx context.Context) {
	if s.opt.Sessions == nil {
		return
	}
	now := time.Now()
	if !s.opened {
		id, err := uuid.NewV7()
		if err != nil {
			return
		}
		if err := s.opt.Sessions.OpenSession(ctx, id, s.opt.Host); err != nil {
			s.log.Warn("could not open the ledger session; the next heartbeat tries again", "error", err)
			return
		}
		s.session, s.opened = id, true
	}
	if s.accepted.Load() == s.settled.Load() {
		s.settledAt = now.Add(-s.opt.Lag)
	}
	gaps := s.takeGaps()
	if err := s.opt.Sessions.Heartbeat(ctx, s.session, s.settledAt, gaps); err != nil {
		s.restoreGaps(gaps)
		s.log.Warn("could not refresh the ledger session", "error", err)
	}
}

// closeSession records the last gaps and closes the session after a clean
// drain.
func (s *Sink) closeSession(ctx context.Context) {
	if s.opt.Sessions == nil || !s.opened {
		return
	}
	gaps := s.takeGaps()
	if err := s.opt.Sessions.CloseSession(ctx, s.session, gaps); err != nil {
		s.log.Warn("could not close the ledger session; a rebuild will read it as a crash", "error", err)
	}
}

// Run is the writer. It returns once Shutdown has drained the queue or ctx is
// cancelled; on cancellation whatever is still queued is counted as a
// shutdown drop.
func (s *Sink) Run(ctx context.Context) {
	s.state.Store(StateRunning)
	close(s.started)
	defer close(s.done)
	defer s.state.Store(StateStopped)
	batch := make([]dataplane.Event, 0, s.opt.BatchSize)
	timer := time.NewTimer(s.opt.FlushInterval)
	defer timer.Stop()
	beat := time.NewTicker(s.opt.Heartbeat)
	defer beat.Stop()
	s.heartbeat(ctx)
	for {
		batch = batch[:0]
		select {
		case ev := <-s.ch:
			batch = append(batch, ev)
		case <-beat.C:
			s.heartbeat(ctx)
			continue
		case <-s.stop:
			if s.drain(ctx) {
				s.closeSession(ctx)
			}
			return
		case <-ctx.Done():
			s.abandon(nil)
			return
		}
		// Fill the batch until it is full or the flush interval has passed
		// since its first event: the latency cost of a ledger row, paid off
		// the path (deployment.md section 4.4).
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(s.opt.FlushInterval)
	fill:
		for len(batch) < s.opt.BatchSize {
			select {
			case ev := <-s.ch:
				batch = append(batch, ev)
			case <-timer.C:
				break fill
			case <-s.stop:
				break fill
			case <-ctx.Done():
				s.abandon(batch)
				return
			}
		}
		if !s.write(ctx, batch) {
			return
		}
	}
}

// drain writes everything queued and reports whether it finished. It is the
// shutdown path, bounded by ctx, which Shutdown's own deadline cancels.
func (s *Sink) drain(ctx context.Context) bool {
	for {
		batch := make([]dataplane.Event, 0, s.opt.BatchSize)
	collect:
		for len(batch) < s.opt.BatchSize {
			select {
			case ev := <-s.ch:
				batch = append(batch, ev)
			default:
				break collect
			}
		}
		if len(batch) == 0 {
			return true
		}
		if !s.write(ctx, batch) {
			return false
		}
	}
}

// abandon counts the batch in hand and everything still queued as shutdown
// drops. Nothing can be enqueued after it runs, because the sink is closed
// first or the process is ending.
func (s *Sink) abandon(batch []dataplane.Event) {
	lost := append([]dataplane.Event(nil), batch...)
	for {
		select {
		case ev := <-s.ch:
			lost = append(lost, ev)
		default:
			if n := len(lost); n > 0 {
				s.log.Warn("ledger events were still queued when the writer stopped", "dropped", n)
				s.drop(observability.DropShutdown, lost...)
				s.settled.Add(int64(n))
			}
			return
		}
	}
}

// write appends one batch, retrying a transient failure with capped backoff
// for as long as ctx lives, and bisecting a permanent one down to the rows
// that cause it. It reports false when ctx ended first, after abandoning what
// was left.
func (s *Sink) write(ctx context.Context, batch []dataplane.Event) bool {
	backoff := s.opt.MinBackoff
	for {
		start := time.Now()
		out, err := s.opt.Appender.Append(ctx, batch)
		s.metrics.WriteLatency.Observe(time.Since(start).Seconds())
		switch {
		case err == nil:
			s.record(out)
			s.settled.Add(int64(len(batch)))
			return true
		case ctx.Err() != nil:
			s.abandon(batch)
			return false
		case s.opt.Appender.Permanent(err):
			if len(batch) == 1 {
				s.deadLetter(ctx, batch[0], err)
				s.settled.Add(1)
				return true
			}
			half := len(batch) / 2
			if !s.write(ctx, batch[:half]) {
				s.abandon(batch[half:])
				return false
			}
			return s.write(ctx, batch[half:])
		}
		// A lost connection or a timeout: hold the batch and try again, so a
		// Postgres outage fills the queue and drops new events as queue_full
		// rather than losing the ones already taken (D4).
		s.log.Warn("could not write a ledger batch; retrying", "events", len(batch),
			"retry_in", backoff.String(), "error", err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			s.abandon(batch)
			return false
		}
		backoff = min(backoff*2, s.opt.MaxBackoff)
	}
}

func (s *Sink) record(out Appended) {
	s.metrics.Written.Add(float64(out.Written))
	for _, d := range out.Dead {
		s.deadCounted(d.ID, d.Reason, nil)
	}
}

func (s *Sink) deadCounted(id uuid.UUID, reason string, cause error) {
	s.metrics.DeadLettered.WithLabelValues(reason).Inc()
	if reason == observability.DeadDuplicate {
		s.metrics.DuplicateDetected.Inc()
	}
	attrs := []any{"event_id", id.String(), "reason", reason}
	if cause != nil {
		attrs = append(attrs, "error", cause.Error())
	}
	s.log.Error("the ledger refused an event; it is kept as a dead letter", attrs...)
}

// deadLetter keeps one event the ledger refused outright. A row that cannot
// even be dead-lettered is the one drop with reason error, and its event_id
// is in the log line, so the gap is attributable.
func (s *Sink) deadLetter(ctx context.Context, ev dataplane.Event, cause error) {
	created, err := s.opt.Appender.DeadLetter(ctx, ev, cause)
	if err != nil {
		s.log.Error("a ledger event could be neither appended nor dead-lettered, and is dropped",
			"event_id", ev.ID.String(), "error", errors.Join(cause, err).Error())
		s.drop(observability.DropError, ev)
		return
	}
	if created {
		s.deadCounted(ev.ID, observability.DeadRejected, cause)
	}
}

// Shutdown stops taking events, writes what is queued, and returns when the
// writer has finished or ctx ends, whichever is first; what is left then is
// counted as a shutdown drop. The writer must have been started with Run.
func (s *Sink) Shutdown(ctx context.Context) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	select {
	case <-s.started:
	default:
		s.abandon(nil)
		return
	}
	s.once.Do(func() {
		s.state.Store(StateDraining)
		close(s.stop)
	})
	select {
	case <-s.done:
	case <-ctx.Done():
	}
}

// Flush returns once every event accepted before it was called has been
// written, found present, dead-lettered or dropped, or ctx ends. It exists so a
// test can read the ledger without polling it with a request.
func (s *Sink) Flush(ctx context.Context) error {
	target := s.accepted.Load()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for s.settled.Load() < target {
		select {
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
