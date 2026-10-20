package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/store"
)

// TestTheGuardRefusesEveryTenantWrite is the unit half of IP-11 DoD 3: with
// the store read-only, every script the store can run and Provision are
// refused before a byte leaves the process, and what writes no tenant key is
// not. The store's address is one nothing listens on, so a command the guard
// lets through fails to dial instead (D5).
func TestTheGuardRefusesEveryTenantWrite(t *testing.T) {
	data, err := store.Open(store.Options{URL: "redis://127.0.0.1:1/0", DataScriptTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	runner, err := script.New(script.Options{Client: data.Client(), Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	end := now.Add(time.Hour)
	win := cycle.Window{Index: 0, Start: now.Add(-time.Minute), End: &end}
	tenant := uuid.NewString()
	req := script.Request{TenantID: tenant, FeatureKey: "f", IdempotencyKey: "k", Amount: 1, Window: win, Limit: 10, At: now}
	rw := script.Rewrite{TenantID: tenant, FeatureKey: "f", Window: win, Limit: 10, Expect: script.Expectation{Any: true}}
	calls := map[string]func() error{
		"consume":  func() error { _, err := runner.Consume(ctx, req); return err },
		"refund":   func() error { _, err := runner.Refund(ctx, req); return err },
		"check":    func() error { _, err := runner.Check(ctx, req); return err },
		"grant":    func() error { _, err := runner.Grant(ctx, req); return err },
		"set":      func() error { _, err := runner.Set(ctx, req); return err },
		"relimit":  func() error { _, err := runner.Relimit(ctx, req); return err },
		"reanchor": func() error { _, err := runner.Reanchor(ctx, rw); return err },
		"rollover": func() error { _, err := runner.Rollover(ctx, rw); return err },
		"rebuild": func() error {
			return runner.Rebuild(ctx, script.Restore{TenantID: tenant, FeatureKey: "f", Window: win, Balance: 1, Limit: 10})
		},
		"provision": func() error {
			return data.Provision(ctx, tenant, []store.Opening{{Feature: "f", Limit: 10, Start: win.Start, End: win.End}})
		},
	}
	data.SetReadOnly(true)
	for name, call := range calls {
		if err := call(); !errors.Is(err, store.ErrReadOnly) {
			t.Errorf("%s while read-only: %v, want ErrReadOnly", name, err)
		}
	}
	// Reads, and writes to keys that are not a tenant's, pass the guard and
	// meet the unreachable store.
	if _, err := data.ReadBalances(ctx, tenant, []string{"f"}); err == nil || errors.Is(err, store.ErrReadOnly) {
		t.Errorf("a read while read-only: %v", err)
	}
	if err := data.PutConfirmation(ctx, "token", store.Confirmation{PlanID: "p"}, time.Minute); err == nil ||
		errors.Is(err, store.ErrReadOnly) {
		t.Errorf("a confirmation token while read-only: %v", err)
	}
	data.SetReadOnly(false)
	for name, call := range calls {
		if err := call(); errors.Is(err, store.ErrReadOnly) {
			t.Errorf("%s refused as read-only in normal mode", name)
		}
	}
}
