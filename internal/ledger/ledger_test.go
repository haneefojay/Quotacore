package ledger

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/observability"
)

var errPermanent = errors.New("permanent")
var errTransient = errors.New("transient")

// fake is an Appender that keeps what it is given. bad marks the events it
// refuses permanently; fail makes the next n calls fail transiently; block,
// when set, holds every call until it is closed.
type fake struct {
	mu      sync.Mutex
	written []dataplane.Event
	dead    []dataplane.Event
	calls   int
	bad     map[string]bool
	fail    int
	block   chan struct{}
}

func (f *fake) Append(ctx context.Context, events []dataplane.Event) (Appended, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return Appended{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail > 0 {
		f.fail--
		return Appended{}, errTransient
	}
	for _, e := range events {
		if f.bad[e.RequestID] {
			return Appended{}, errPermanent
		}
	}
	f.written = append(f.written, events...)
	return Appended{Written: len(events)}, nil
}

func (f *fake) DeadLetter(_ context.Context, ev dataplane.Event, _ error) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dead = append(f.dead, ev)
	return true, nil
}

func (f *fake) Permanent(err error) bool { return errors.Is(err, errPermanent) }

func (f *fake) snapshot() (written, dead []dataplane.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dataplane.Event(nil), f.written...), append([]dataplane.Event(nil), f.dead...)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func event(req string) dataplane.Event {
	return dataplane.Event{RequestID: req, TenantID: "t", FeatureKey: "f", Type: "consumed", Source: "api",
		Amount: 1, Delta: -1, Balance: 9}
}

func newSink(f *fake, queue, batch int) *Sink {
	return New(Options{QueueSize: queue, BatchSize: batch, FlushInterval: 5 * time.Millisecond, Appender: f,
		Logger: quiet(), MinBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond})
}

func flush(t *testing.T, s *Sink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// TestEnqueueNeverBlocks is DoD 3's unit half: with the writer stalled and the
// queue full, every further Enqueue returns at once and is counted as a
// queue_full drop, and the queued events are written in order once the writer
// is released.
func TestEnqueueNeverBlocks(t *testing.T) {
	f := &fake{block: make(chan struct{})}
	s := newSink(f, 8, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	// The writer takes up to a batch off the queue and then blocks in Append,
	// so 8 queued plus at most 4 in hand are accepted.
	start := time.Now()
	for i := range 1000 {
		s.Enqueue(event(string(rune('a' + i%26))))
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("1000 enqueues against a stalled writer took %s", d)
	}
	accepted := s.accepted.Load()
	if accepted < 8 || accepted > 12 {
		t.Errorf("accepted %d, want the queue's 8 plus at most one batch", accepted)
	}
	if got := testutil.ToFloat64(s.Metrics().Dropped.WithLabelValues(observability.DropQueueFull)); int64(got) != 1000-accepted {
		t.Errorf("queue_full drops = %v, want %d", got, 1000-accepted)
	}
	if s.Dropped() != 1000-accepted {
		t.Errorf("Dropped() = %d, want %d", s.Dropped(), 1000-accepted)
	}
	close(f.block)
	flush(t, s)
	written, _ := f.snapshot()
	if int64(len(written)) != accepted {
		t.Errorf("wrote %d, want %d", len(written), accepted)
	}
}

// TestAZeroDeltaIsNeverQueued holds the sink to Q-22: a zero delta is a bug in
// the caller, dropped with reason error and never written.
func TestAZeroDeltaIsNeverQueued(t *testing.T) {
	f := &fake{}
	s := newSink(f, 8, 4)
	ev := event("z")
	ev.Delta = 0
	s.Enqueue(ev)
	if s.Depth() != 0 || testutil.ToFloat64(s.Metrics().Dropped.WithLabelValues(observability.DropError)) != 1 {
		t.Errorf("a zero delta was queued (depth %d)", s.Depth())
	}
}

// TestTheWriterRetriesATransientFailure is D4: a lost connection holds the
// batch and retries it, so nothing is lost and nothing is written twice.
func TestTheWriterRetriesATransientFailure(t *testing.T) {
	f := &fake{fail: 3}
	s := newSink(f, 64, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	for _, r := range []string{"a", "b", "c"} {
		s.Enqueue(event(r))
	}
	flush(t, s)
	written, dead := f.snapshot()
	if len(written) != 3 || len(dead) != 0 || written[0].RequestID != "a" || written[2].RequestID != "c" {
		t.Errorf("written %v, dead %v; want a, b and c in order", written, dead)
	}
	for _, e := range written {
		if e.ID == (uuid.UUID{}) || e.ID.Version() != 7 {
			t.Errorf("event %s has id %s, want a UUIDv7 minted at Enqueue", e.RequestID, e.ID)
		}
	}
	if s.Dropped() != 0 {
		t.Errorf("dropped %d", s.Dropped())
	}
}

// TestAPermanentFailureIsBisectedToItsRow is D4's other branch: one bad row
// in a batch is found by bisection and dead-lettered alone, and every other
// row of the batch is written.
func TestAPermanentFailureIsBisectedToItsRow(t *testing.T) {
	f := &fake{bad: map[string]bool{"e": true}}
	s := newSink(f, 64, 8)
	// Queue before the writer starts, so the eight land in one batch.
	for _, r := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		s.Enqueue(event(r))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	flush(t, s)
	written, dead := f.snapshot()
	if len(written) != 7 || len(dead) != 1 || dead[0].RequestID != "e" {
		t.Fatalf("written %d, dead %v; want 7 written and e dead-lettered", len(written), dead)
	}
	if got := testutil.ToFloat64(s.Metrics().DeadLettered.WithLabelValues(observability.DeadRejected)); got != 1 {
		t.Errorf("rejected dead letters = %v, want 1", got)
	}
}

// TestShutdownWritesWhatIsQueued is D8: Shutdown refuses new events, writes
// what is queued, reports draining while it does, and leaves the writer
// stopped; an event offered after it is a shutdown drop.
func TestShutdownWritesWhatIsQueued(t *testing.T) {
	f := &fake{block: make(chan struct{})}
	s := newSink(f, 64, 4)
	if s.State() != StateStopped {
		t.Errorf("state before Run = %s", s.State())
	}
	go s.Run(context.Background())
	// A sink whose writer never started has nothing to drain with, so wait
	// for it to start.
	<-s.started
	for i := range 10 {
		s.Enqueue(event(string(rune('a' + i))))
	}
	done := make(chan struct{})
	go func() {
		s.Shutdown(context.Background())
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for s.State() != StateDraining && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.State() != StateDraining {
		t.Errorf("state while draining = %s", s.State())
	}
	s.Enqueue(event("late"))
	close(f.block)
	<-done
	written, _ := f.snapshot()
	if len(written) != 10 {
		t.Errorf("wrote %d of the 10 queued", len(written))
	}
	if s.State() != StateStopped {
		t.Errorf("state after Shutdown = %s", s.State())
	}
	if got := testutil.ToFloat64(s.Metrics().Dropped.WithLabelValues(observability.DropShutdown)); got != 1 {
		t.Errorf("shutdown drops = %v, want the late event", got)
	}
}

// TestACancelledWriterCountsWhatItLeaves bounds the drain: when the writer's
// context ends with events still queued, each is a shutdown drop, so the gap
// is counted and never silent.
func TestACancelledWriterCountsWhatItLeaves(t *testing.T) {
	f := &fake{block: make(chan struct{})}
	s := newSink(f, 64, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	for i := range 10 {
		s.Enqueue(event(string(rune('a' + i))))
	}
	cancel()
	<-done
	if got := testutil.ToFloat64(s.Metrics().Dropped.WithLabelValues(observability.DropShutdown)); got != 10 {
		t.Errorf("shutdown drops = %v, want 10", got)
	}
	flush(t, s)
}
