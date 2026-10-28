package worker

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/snapshot"
)

type tenants []*snapshot.Tenant

func (t tenants) Cached() []*snapshot.Tenant { return t }

type call struct {
	tenant, feature string
	at              time.Time
}

type plane struct {
	mu    sync.Mutex
	calls []call
	fail  map[string]bool
}

func (p *plane) Advance(_ context.Context, t *snapshot.Tenant, feature string, at time.Time) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call{t.ID, feature, at})
	if p.fail[feature] {
		return false, errors.New("store down")
	}
	return true, nil
}

// tenant is one tenant anchored at anchor, holding each feature at its cadence.
func tenant(id, state string, anchor time.Time, features map[string]cycle.Interval) *snapshot.Tenant {
	pins := map[string]snapshot.Pin{}
	ents := map[string]snapshot.Entitlement{}
	for f, iv := range features {
		pins[f] = snapshot.Pin{Interval: iv}
		ents[f] = snapshot.Entitlement{Limit: 100, Interval: iv}
	}
	return snapshot.NewTenant(&snapshot.LoadedTenant{
		ID: id, ExternalID: "e-" + id, State: state, Timezone: "UTC", Anchor: anchor, Pins: pins,
		Plan: &snapshot.LoadedPlan{ID: "p", Key: "p", Version: 1, Entitlements: ents},
	})
}

// TestAScanAdvancesOnlyTheBoundariesThatPassedInItsSpan: an hourly feature
// whose hour began inside (prev, now] is due; a monthly one whose month began
// long ago is not; a never one never is; a suspended tenant is left to the
// request path, which refuses it before the transition.
func TestAScanAdvancesOnlyTheBoundariesThatPassedInItsSpan(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 28, 15, 0, 20, 0, time.UTC)
	prev := now.Add(-time.Minute)
	p := &plane{}
	w := New(Options{
		Tenants: tenants{
			tenant("a", "active", anchor, map[string]cycle.Interval{"calls": cycle.Hourly, "seats": cycle.Monthly, "once": cycle.Never}),
			tenant("b", "suspended", anchor, map[string]cycle.Interval{"calls": cycle.Hourly}),
		},
		Plane: p,
	})
	sc := w.Sweep(context.Background(), prev, now)
	if sc.Tenants != 2 || sc.Due != 1 || sc.Rolled != 1 || sc.Failed != 0 {
		t.Fatalf("%+v", sc)
	}
	if len(p.calls) != 1 || p.calls[0] != (call{"a", "calls", now}) {
		t.Fatalf("calls %+v", p.calls)
	}
	// The next scan's span starts where this one ended, so the same boundary
	// is not due twice.
	if sc := w.Sweep(context.Background(), now, now.Add(time.Minute)); sc.Due != 0 {
		t.Fatalf("the boundary was due again: %+v", sc)
	}
}

// TestAFailureIsCountedAndTheScanGoesOn (state-machines.md section 11): a
// data-store error on one feature neither aborts the batch nor retries.
func TestAFailureIsCountedAndTheScanGoesOn(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 28, 15, 0, 20, 0, time.UTC)
	var ts tenants
	for i := range 5 {
		ts = append(ts, tenant(strconv.Itoa(i), "active", anchor, map[string]cycle.Interval{"calls": cycle.Hourly, "bad": cycle.Hourly}))
	}
	p := &plane{fail: map[string]bool{"bad": true}}
	sc := New(Options{Tenants: ts, Plane: p, Batch: 2}).Sweep(context.Background(), now.Add(-time.Minute), now)
	if sc.Tenants != 5 || sc.Due != 10 || sc.Rolled != 5 || sc.Failed != 5 || len(p.calls) != 10 {
		t.Fatalf("%+v, %d calls", sc, len(p.calls))
	}
}

// TestRunScansOnTheIntervalAndPausesInReadOnlyMode: a running worker scans
// at once and then every interval, and scans nothing while paused.
func TestRunScansOnTheIntervalAndPausesInReadOnlyMode(t *testing.T) {
	var mu sync.Mutex
	clock := time.Date(2026, 10, 28, 14, 59, 59, 0, time.UTC)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(500 * time.Millisecond)
		return clock
	}
	var paused sync.Mutex
	isPaused := true
	p := &plane{}
	w := New(Options{
		Tenants:  tenants{tenant("a", "active", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), map[string]cycle.Interval{"calls": cycle.Hourly})},
		Plane:    p,
		Interval: 20 * time.Millisecond,
		Now:      now,
		Paused:   func() bool { paused.Lock(); defer paused.Unlock(); return isPaused },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	p.mu.Lock()
	n := len(p.calls)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("a paused worker advanced %d", n)
	}
	paused.Lock()
	isPaused = false
	paused.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		n = len(p.calls)
		p.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	// The boundary at 15:00 passed while the worker was paused, and is
	// advanced once when it resumes, not skipped.
	if n != 1 {
		t.Fatalf("%d advances after resuming, want the one boundary the pause covered", n)
	}
}

// TestTheWorkerHasNoDatabaseAccess is INV-WK2 as a source check: the package
// imports no SQL, data-store or script package, so the only state it can move
// is through the data plane's Advance.
func TestTheWorkerHasNoDatabaseAccess(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"database/sql", "internal/db", "internal/store", "internal/script", "github.com/jackc", "github.com/redis"}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			for _, bad := range forbidden {
				if strings.Contains(imp.Path.Value, bad) {
					t.Errorf("%s imports %s", f, imp.Path.Value)
				}
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no source file was checked")
	}
}
