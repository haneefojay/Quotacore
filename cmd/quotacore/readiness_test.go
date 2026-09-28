package main

import (
	"sync"
	"testing"
	"time"
)

// The /readyz handler calls the closure this returns from every request, so the
// closure is concurrent by construction rather than by accident. These tests are
// what the race detector in CI is pointed at; on a machine without a C compiler
// the race build cannot run, and this file is the reason to install one.
func TestReadyFuncLogsTheFirstObservation(t *testing.T) {
	s := newReadinessState("a", "b")
	var calls []string
	f := s.ReadyFuncLogging(func(msg string, terms []string) {
		calls = append(calls, msg)
	})

	if f() {
		t.Fatal("a state with unsatisfied terms must not be ready")
	}
	if len(calls) != 1 || calls[0] != "not ready" {
		t.Fatalf("calls = %v, want exactly one not ready observation", calls)
	}

	// Unchanged: no further logging, however many probes arrive.
	for range 5 {
		f()
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want no logging for an unchanged state", calls)
	}

	s.Set("a", true)
	s.Set("b", true)
	if !f() {
		t.Fatal("all terms satisfied must be ready")
	}
	if len(calls) != 2 || calls[1] != "ready" {
		t.Fatalf("calls = %v, want a second observation saying ready", calls)
	}
}

func TestReadyFuncLoggingIsSafeForConcurrentProbes(t *testing.T) {
	const probes = 16
	s := newReadinessState("data store", "tenant", "draining")

	// The log callback is where two probes would interleave if the closure called
	// it without holding its own lock, so the callback is the thing to watch: it
	// marks itself inside, waits long enough for a second caller to arrive, and
	// records whether one did. The wait is what makes this deterministic rather
	// than a matter of luck.
	var mu sync.Mutex
	inside := false
	overlapped := false
	logged := 0
	f := s.ReadyFuncLogging(func(string, []string) {
		mu.Lock()
		if inside {
			overlapped = true
		}
		inside = true
		logged++
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
		inside = false
		mu.Unlock()
	})

	// One synchronous probe first, so the test never depends on the scheduler
	// deciding to run the goroutines below.
	if f() {
		t.Fatal("a state with unsatisfied terms must not be ready")
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	started := make(chan struct{}, probes)
	for range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// One probe, then announce it. Without this barrier the flip loop
			// below can finish before the goroutine is ever scheduled, and the
			// test would be asserting on the scheduler rather than on the code.
			f()
			started <- struct{}{}
			for {
				select {
				case <-stop:
					return
				default:
					f()
				}
			}
		}()
	}
	for range probes {
		<-started
	}

	// Flip terms while the probes run, so the closure both reads a changing set
	// of terms and writes its remembered one.
	for i := range 200 {
		s.Set("tenant", i%2 == 0)
		s.Set("draining", i%3 != 0)
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if overlapped {
		t.Error("two probes were inside the log callback at once, so log lines can interleave")
	}
	if logged < 2 {
		t.Errorf("logged = %d, want at least 2: the synchronous probe and at least one transition", logged)
	}
}
