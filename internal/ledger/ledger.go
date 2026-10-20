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
}

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
	s := &Sink{
		opt:     opt,
		ch:      make(chan dataplane.Event, opt.QueueSize),
		log:     opt.Logger,
		started: make(chan struct{}),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
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
		s.drop(observability.DropError, 1)
		return
	}
	if ev.ID == (uuid.UUID{}) {
		id, err := uuid.NewV7()
		if err != nil {
			s.log.Error("could not mint an event_id", "request_id", ev.RequestID, "error", err)
			s.drop(observability.DropError, 1)
			return
		}
		ev.ID = id
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		s.drop(observability.DropShutdown, 1)
		return
	}
	select {
	case s.ch <- ev:
		s.accepted.Add(1)
		s.metrics.Enqueued.Inc()
	default:
		s.drop(observability.DropQueueFull, 1)
	}
}

func (s *Sink) drop(reason string, n int) {
	s.dropped.Add(int64(n))
	s.metrics.Dropped.WithLabelValues(reason).Add(float64(n))
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
	for {
		batch = batch[:0]
		select {
		case ev := <-s.ch:
			batch = append(batch, ev)
		case <-s.stop:
			s.drain(ctx)
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

// drain writes everything queued and returns. It is the shutdown path, bounded
// by ctx, which Shutdown's own deadline cancels.
func (s *Sink) drain(ctx context.Context) {
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
			return
		}
		if !s.write(ctx, batch) {
			return
		}
	}
}

// abandon counts the batch in hand and everything still queued as shutdown
// drops. Nothing can be enqueued after it runs, because the sink is closed
// first or the process is ending.
func (s *Sink) abandon(batch []dataplane.Event) {
	n := len(batch)
	for {
		select {
		case <-s.ch:
			n++
		default:
			if n > 0 {
				s.log.Warn("ledger events were still queued when the writer stopped", "dropped", n)
				s.drop(observability.DropShutdown, n)
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
		s.drop(observability.DropError, 1)
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
