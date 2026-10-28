package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// IP-15 DoD 3, finding 5: a drain that overran its grace used to return before
// the ledger was drained, so every queued event of a request that had already
// answered was lost. These tests drive shutdown with a real listener.

func serveBlocking(t *testing.T, release <-chan struct{}) (*http.Server, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
	})}
	go srv.Serve(l)
	go http.Get("http://" + l.Addr().String() + "/")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the handler")
	}
	return srv, l.Addr().String()
}

func TestASlowDrainStillWritesTheLedger(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv, _ := serveBlocking(t, release)

	var drained bool
	var live error
	var budget time.Duration
	drain := func(ctx context.Context) {
		drained = true
		live = ctx.Err()
		if d, ok := ctx.Deadline(); ok {
			budget = time.Until(d)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := shutdown(srv, &http.Server{}, drain, 50*time.Millisecond, log)
	if err == nil {
		t.Fatal("an overrun drain reported success")
	}
	if !drained {
		t.Fatal("the ledger was not drained after the HTTP drain overran its grace")
	}
	if live != nil {
		t.Fatalf("the ledger was drained with an expired context (%v), so it wrote nothing", live)
	}
	if budget <= time.Second || budget > ledgerDrainAfterOverrun {
		t.Fatalf("the ledger's budget after an overrun is %s, want up to %s", budget, ledgerDrainAfterOverrun)
	}
}

func TestACleanDrainUsesTheGrace(t *testing.T) {
	release := make(chan struct{})
	srv, _ := serveBlocking(t, release)
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()
	var live error
	drain := func(ctx context.Context) { live = ctx.Err() }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := shutdown(srv, &http.Server{}, drain, 5*time.Second, log); err != nil {
		t.Fatalf("a drain inside the grace reported %v", err)
	}
	if live != nil {
		t.Fatalf("the ledger was drained with an expired context: %v", live)
	}
}
