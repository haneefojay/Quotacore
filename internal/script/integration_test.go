package script

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/store"
)

// Every test in this file runs against a real datastore, because the properties
// being proved are properties of Redis: that a script's writes are atomic, that
// EVALSHA is addressed by content, that a key's expiry can be read back. A fake
// would prove that this code calls a fake the way this code calls a fake.
//
// The skip rule is internal/store's, unchanged: an explicitly requested
// integration environment without a URL is a failure, never a silent skip.

const dataScriptTimeout = 250 * time.Millisecond

// state is a seeded balance hash: the six fields data-model.md Â§3.2 names, in
// the shape the control plane writes them. An End of nil is written as the empty
// string, which is the only encoding available: HMGET cannot tell a missing
// field from an empty one, so an entitlement that never resets has to be spelled
// with a value rather than with an absence.
type state struct {
	Balance int64
	Limit   int64
	Bonus   int64
	Index   int64
	Start   time.Time
	End     *time.Time
}

func integrationRunner(t *testing.T) (*store.Store, *Runner) {
	t.Helper()
	redisURL := os.Getenv("QUOTACORE_REDIS_URL")
	required := os.Getenv("QUOTACORE_REQUIRE_INTEGRATION") != ""
	if redisURL == "" {
		if required {
			t.Fatal("QUOTACORE_REQUIRE_INTEGRATION is set but QUOTACORE_REDIS_URL is unset, so this test would skip silently")
		}
		t.Skip("QUOTACORE_REDIS_URL is unset, so there is no datastore to talk to")
	}
	s, err := store.Open(store.Options{URL: redisURL, DataScriptTimeout: dataScriptTimeout})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if err := Load(ctx, s.Client()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	runner, err := New(Options{Client: s.Client(), Timeout: dataScriptTimeout})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, runner
}

// featureKey names the key after the test that owns it, so a failure points at
// one key in the store and two tests cannot share one. Go's test names contain
// slashes and the key grammar does not, so they are translated.
func featureKey(t *testing.T) string {
	return strings.NewReplacer("/", "-", "#", "-", " ", "-").Replace(t.Name())
}

func keyFor(t *testing.T, feature string) string {
	t.Helper()
	key, err := store.BalanceKey(testTenant, feature)
	if err != nil {
		t.Fatalf("BalanceKey: %v", err)
	}
	return key
}

// idemKeyFor is the record key a client key names, built by the same function
// the runner uses, for the tests that put a script call on the wire by hand.
func idemKeyFor(t *testing.T, clientKey string) string {
	t.Helper()
	key, err := store.IdempotencyKey(testTenant, clientKey)
	if err != nil {
		t.Fatalf("IdempotencyKey: %v", err)
	}
	return key
}

// seed writes the balance hash the way the control plane will, and clears
// anything left by an earlier run of this test.
func seed(t *testing.T, s *store.Store, key string, want state) {
	t.Helper()
	ctx := context.Background()
	if err := s.Client().Del(ctx, key).Err(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	end := ""
	if want.End != nil {
		end = strconv.FormatInt(want.End.UnixMilli(), 10)
	}
	err := s.Client().HSet(ctx, key, map[string]any{
		"balance":      want.Balance,
		"limit":        want.Limit,
		"bonus":        want.Bonus,
		"cycle_index":  want.Index,
		"window_start": want.Start.UnixMilli(),
		"window_end":   end,
	}).Err()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// fields reads the whole hash back, so an assertion can be about every field
// rather than about the one that was expected to move.
func fields(t *testing.T, s *store.Store, key string) map[string]string {
	t.Helper()
	got, err := s.Client().HGetAll(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

func balanceOf(t *testing.T, s *store.Store, key string) int64 {
	t.Helper()
	raw, ok := fields(t, s, key)["balance"]
	if !ok {
		t.Fatalf("the key has no balance at all")
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("balance = %q, which is not a number", raw)
	}
	return n
}

// ends is the address of a window end, for seeding a hash whose window closes.
// A nil End is a window that never closes, and seed writes that as the empty
// string the script reads back.
func ends(at time.Time) *time.Time { return &at }

// fixedWindow is a named window rather than one computed from now, because every
// assertion below is about an instant the test states: a boundary either closed
// or it did not, and that has to be true by construction rather than by whatever
// time the test happened to start.
func fixedWindow(index int64, start time.Time, length time.Duration) cycle.Window {
	end := start.Add(length)
	return cycle.Window{Index: index, Start: start, End: &end}
}

// idemSeq numbers the idempotency keys this file hands out. A distinct key per
// call keeps the tests that predate the record measuring what they were written
// to measure: they are about distinct operations contending over one balance,
// and a shared key would turn every call after the first into a replay, so the
// decrement and the rollover would stop happening at all.
var idemSeq int64

// runNonce is part of every key this file hands out, so a record left in the
// store by an earlier run of the same test - the records live for 24 hours and a
// test's names do not change between runs - is never mistaken for a repeat of
// this run's operation. Without it, a second `go test` against the same store
// would replay the first run's records, and every charge-once assertion would be
// measuring the wrong run.
var runNonce = time.Now().UnixNano()

// idemKey is a client key unique to one call, built from the test's name so a
// leftover record points at one test and is inside the tenant's slot. It is safe
// to call from several goroutines at once, which the contention tests do.
func idemKey(t *testing.T) string {
	return fmt.Sprintf("%s-idem-%d-%d", featureKey(t), runNonce, atomic.AddInt64(&idemSeq, 1))
}

// TestT01AtomicDecrementUnderContention is T-01 and Definition of Done item 1:
// 200 concurrent decrements of a balance of 100 leave it at exactly zero, with
// exactly 100 approvals. A store that let two requests pass a check that only one
// of them could pass would end above zero, and a store that let a denial write
// anyway would end below.
func TestT01AtomicDecrementUnderContention(t *testing.T) {
	s, runner := integrationRunner(t)
	key := keyFor(t, featureKey(t))
	at := time.Now().UTC().Truncate(time.Second)
	end := at.Add(23 * time.Hour)
	seed(t, s, key, state{Balance: 100, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: &end})

	const callers = 200
	req := Request{TenantID: testTenant, FeatureKey: featureKey(t), Amount: 1, Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at}
	// The store's own budget is 250 ms, and this test fires 200 requests at one
	// key at once, so the queue in front of the store is longer than one budget
	// even though each request is served in microseconds. The budget here is
	// therefore several times the store's, and it is the store's budget that
	// TestTheRunnerHonoursItsBudget proves. A contention test that failed on
	// queueing would be measuring the test's fan-out, not the script.
	contention, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var mu sync.Mutex
	var applied, denied, other int
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			call := req
			call.IdempotencyKey = idemKey(t)
			out, err := runner.Consume(contention, call)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				other++
				t.Errorf("Consume: %v", err)
			case out.Decision == Applied:
				applied++
			case out.Decision == Denied:
				denied++
			default:
				other++
				t.Errorf("decision = %q, and a contention test wants only two outcomes", out.Decision)
			}
		}()
	}
	wg.Wait()

	if applied != 100 || denied != 100 || other != 0 {
		t.Errorf("applied %d, denied %d, other %d; want 100, 100, 0", applied, denied, other)
	}
	if got := balanceOf(t, s, key); got != 0 {
		t.Errorf("balance = %d, and 100 decrements of 1 from 100 is 0 (DR-017)", got)
	}
}

// TestT04TheCeilingHolds is T-04 and Definition of Done item 2, for the half this
// phase owns. The admin grant half needs a control-plane operation and is
// IP-09's; what is proved here is the invariant the grant is measured against.
func TestT04TheCeilingHolds(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	feature := featureKey(t)
	key := keyFor(t, feature)
	req := func(amount int64) Request {
		return Request{TenantID: testTenant, FeatureKey: feature, Amount: amount, IdempotencyKey: idemKey(t),
			Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at}
	}
	assertCeiling := func(when string, limit, bonus int64) {
		t.Helper()
		got := balanceOf(t, s, key)
		if got > limit+bonus {
			t.Errorf("%s: balance %d is above the ceiling %d (INV-X5, DR-019)", when, got, limit+bonus)
		}
	}

	t.Run("no bonus", func(t *testing.T) {
		seed(t, s, key, state{Balance: 0, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: ends(at.Add(23 * time.Hour))})
		out, err := runner.Refund(ctx, req(100))
		if err != nil || out.Decision != Applied {
			t.Fatalf("Refund(100) = %+v, %v; want applied", out, err)
		}
		if got := balanceOf(t, s, key); got != 100 {
			t.Errorf("balance = %d, want 100", got)
		}
		assertCeiling("at the ceiling", 100, 0)

		out, err = runner.Refund(ctx, req(1))
		if err != nil {
			t.Fatalf("Refund(1): %v", err)
		}
		if out.Decision != RefusedCeiling {
			t.Errorf("a refund of 1 against a full allowance of 100 was %q, want refused_ceiling (DR-020)", out.Decision)
		}
		if got := balanceOf(t, s, key); got != 100 {
			t.Errorf("balance = %d after a refused refund, and a refusal changes nothing", got)
		}
	})

	t.Run("the bonus is part of the ceiling", func(t *testing.T) {
		seed(t, s, key, state{Balance: 0, Limit: 100, Bonus: 5, Index: 5, Start: at.Add(-time.Hour), End: ends(at.Add(23 * time.Hour))})
		out, err := runner.Refund(ctx, req(105))
		if err != nil || out.Decision != Applied {
			t.Fatalf("Refund(105) = %+v, %v; want applied, because the ceiling is limit + bonus", out, err)
		}
		if got := balanceOf(t, s, key); got != 105 {
			t.Errorf("balance = %d, want 105", got)
		}
		assertCeiling("at the ceiling with a bonus", 100, 5)
		if out, err = runner.Refund(ctx, req(1)); err != nil || out.Decision != RefusedCeiling {
			t.Errorf("a refund of 1 against 105 of 105 was %+v, %v; want refused_ceiling", out, err)
		}
	})

	t.Run("many concurrent refunds cannot pass the ceiling", func(t *testing.T) {
		seed(t, s, key, state{Balance: 0, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: ends(at.Add(23 * time.Hour))})
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := runner.Refund(ctx, req(10)); err != nil {
					t.Errorf("Refund: %v", err)
				}
			}()
		}
		wg.Wait()
		assertCeiling("after 50 concurrent refunds of 10", 100, 0)
		if got := balanceOf(t, s, key); got != 100 {
			t.Errorf("balance = %d, and 50 refunds of 10 against a ceiling of 100 leaves exactly 100", got)
		}
	})
}

// TestT08ADenialChangesNothing is T-08 and Definition of Done item 3, over every
// code these three scripts can produce. The catalogue-wide sweep, which needs a
// code to be reachable over HTTP, is IP-07's; the ledger half is IP-12's.
func TestT08ADenialChangesNothing(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	feature := featureKey(t)
	key := keyFor(t, feature)
	end := at.Add(23 * time.Hour)
	seeded := state{Balance: 10, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: &end}
	req := func(amount int64) Request {
		return Request{TenantID: testTenant, FeatureKey: feature, Amount: amount, IdempotencyKey: idemKey(t),
			Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at}
	}

	t.Run("a consume that does not fit", func(t *testing.T) {
		seed(t, s, key, seeded)
		before := fields(t, s, key)
		out, err := runner.Consume(ctx, req(11))
		if err != nil || out.Decision != Denied {
			t.Fatalf("Consume(11) against 10 = %+v, %v; want denied", out, err)
		}
		assertUnchanged(t, s, key, before, "denied")
	})

	t.Run("a refund that breaks the ceiling", func(t *testing.T) {
		seed(t, s, key, seeded)
		before := fields(t, s, key)
		out, err := runner.Refund(ctx, req(91))
		if err != nil || out.Decision != RefusedCeiling {
			t.Fatalf("Refund(91) against 10 of 100 = %+v, %v; want refused_ceiling", out, err)
		}
		assertUnchanged(t, s, key, before, "refused_ceiling")
	})

	t.Run("a hash with a field missing", func(t *testing.T) {
		if err := s.Client().HDel(ctx, key, "window_end").Err(); err != nil {
			t.Fatalf("HDel: %v", err)
		}
		before := fields(t, s, key)
		if _, err := runner.Consume(ctx, req(1)); !errors.Is(err, ErrStateMissing) {
			t.Fatalf("Consume against a hash with no window_end = %v, want the state-missing marker", err)
		}
		assertUnchanged(t, s, key, before, "state_missing")
	})

	t.Run("a hash with a field that is not a number", func(t *testing.T) {
		seed(t, s, key, seeded)
		if err := s.Client().HSet(ctx, key, "balance", "ten").Err(); err != nil {
			t.Fatalf("HSet: %v", err)
		}
		before := fields(t, s, key)
		if _, err := runner.Consume(ctx, req(1)); !errors.Is(err, ErrStateMissing) {
			t.Fatalf("Consume against a balance of %q = %v, want the state-missing marker", "ten", err)
		}
		assertUnchanged(t, s, key, before, "state_missing")
	})

	t.Run("a hash with a negative balance", func(t *testing.T) {
		seed(t, s, key, seeded)
		if err := s.Client().HSet(ctx, key, "balance", -1).Err(); err != nil {
			t.Fatalf("HSet: %v", err)
		}
		before := fields(t, s, key)
		if _, err := runner.Consume(ctx, req(1)); !errors.Is(err, ErrStateMissing) {
			t.Fatalf("Consume against a negative balance = %v, want the state-missing marker", err)
		}
		assertUnchanged(t, s, key, before, "state_missing")
	})
}

// assertUnchanged is T-08's assertion: the balance, the bonus, the cycle index and
// both ends of the window are what they were. The expiry is excluded, and the
// reason is in the scripts: a window that has opened needs its key held until
// window_end + 24h, and the one round trip a request is allowed is the one that
// sets it. The expiry is asserted separately, and correctly, in the key-expiry
// test.
func assertUnchanged(t *testing.T, s *store.Store, key string, before map[string]string, when string) {
	t.Helper()
	after := fields(t, s, key)
	if len(after) != len(before) {
		t.Errorf("after %s the hash has %d fields rather than %d: %v", when, len(after), len(before), after)
	}
	for _, name := range []string{"balance", "limit", "bonus", "cycle_index", "window_start", "window_end"} {
		if after[name] != before[name] {
			t.Errorf("after %s, %s = %q, want %q (DR-025: a denial changes no balance and no cycle state)", when, name, after[name], before[name])
		}
	}
}

// TestT05TheTransitionIsMonotonic is T-05. Two claims: a target behind the store
// is refused and changes nothing, and a jump forwards advances once rather than
// once per boundary it skipped.
func TestT05TheTransitionIsMonotonic(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	feature := featureKey(t)
	key := keyFor(t, feature)
	// A window that closed an hour ago, so the store is behind the caller.
	seed(t, s, key, state{Balance: 40, Limit: 100, Index: 5, Start: at.Add(-25 * time.Hour), End: ends(at.Add(-time.Hour))})

	t.Run("a target behind the store", func(t *testing.T) {
		before := fields(t, s, key)
		out, err := runner.Consume(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, IdempotencyKey: idemKey(t),
			Window: fixedWindow(4, at.Add(-49*time.Hour), 24*time.Hour), Limit: 100, At: at})
		if err != nil {
			t.Fatalf("Consume: %v", err)
		}
		if out.Transition != Stale {
			t.Errorf("transition = %q for a target of 4 against a store at 5, want stale", out.Transition)
		}
		if out.Decision != Applied {
			t.Errorf("decision = %q; a caller that is behind is answered against the live state, not failed (C-3)", out.Decision)
		}
		// The deduction happened; the cycle did not move.
		after := fields(t, s, key)
		if after["balance"] != "39" {
			t.Errorf("balance = %q, want 39", after["balance"])
		}
		for _, name := range []string{"cycle_index", "window_start", "window_end", "limit", "bonus"} {
			if after[name] != before[name] {
				t.Errorf("%s = %q, want %q: a target behind the store rolls nothing (T-05)", name, after[name], before[name])
			}
		}
	})

	t.Run("a jump forwards advances once", func(t *testing.T) {
		// Two indices ahead of the store, over the same window. The indices
		// disagreeing while the instants agree is what a corrected anchor looks
		// like from here (DR-013): the caller's index is ahead, so the hash
		// follows it, and it follows it once.
		out, err := runner.Consume(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, IdempotencyKey: idemKey(t),
			Window: fixedWindow(7, at.Add(-25*time.Hour), 24*time.Hour), Limit: 100, At: at})
		if err != nil {
			t.Fatalf("Consume: %v", err)
		}
		if out.Transition != Rolled {
			t.Errorf("transition = %q, want rolled", out.Transition)
		}
		if out.Index != 7 {
			t.Errorf("index = %d, want 7: the store advances to the target, not one step", out.Index)
		}
		if out.Balance != 99 {
			t.Errorf("balance = %d, want 99: the allowance is re-granted once and the request is then spent from it", out.Balance)
		}
		if got := balanceOf(t, s, key); got != 99 {
			t.Errorf("the key holds %d, want 99", got)
		}
		if after := fields(t, s, key); after["cycle_index"] != "7" || after["bonus"] != "0" {
			t.Errorf("cycle_index, bonus = %q, %q; want 7, 0", after["cycle_index"], after["bonus"])
		}
	})
}

// TestTheReplyIsTheStateTheStoreHolds proves the two figures a caller reads are
// the two the key holds, which is the whole reason the reply carries the state
// rather than the caller reading it back. A reply that reported the balance from
// before the write would tell a caller who spent the last unit that it still had
// one, and a caller who was told it is stale would have to guess whether the key
// it did not move still expires.
func TestTheReplyIsTheStateTheStoreHolds(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	feature := featureKey(t)
	key := keyFor(t, feature)
	end := at.Add(23 * time.Hour)
	seed(t, s, key, state{Balance: 1, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: &end})

	t.Run("a deduction reports the balance after it", func(t *testing.T) {
		out, err := runner.Consume(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, IdempotencyKey: idemKey(t),
			Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at})
		if err != nil {
			t.Fatalf("Consume: %v", err)
		}
		if out.Decision != Applied {
			t.Fatalf("decision = %q, want applied", out.Decision)
		}
		if out.Balance != 0 {
			t.Errorf("reply balance = %d, want 0: the last unit was spent, and the reply is the state the key is in now", out.Balance)
		}
		if got := balanceOf(t, s, key); got != 0 {
			t.Errorf("the key holds %d, want 0", got)
		}
	})

	t.Run("a credit reports the balance after it", func(t *testing.T) {
		out, err := runner.Refund(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 7, IdempotencyKey: idemKey(t),
			Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at})
		if err != nil {
			t.Fatalf("Refund: %v", err)
		}
		if out.Decision != Applied {
			t.Fatalf("decision = %q, want applied", out.Decision)
		}
		if out.Balance != 7 {
			t.Errorf("reply balance = %d, want 7: the reply is the state the key is in now", out.Balance)
		}
		if got := balanceOf(t, s, key); got != 7 {
			t.Errorf("the key holds %d, want 7", got)
		}
	})

	t.Run("a stale call still sets the expiry of the window that is stored", func(t *testing.T) {
		// The stored window closed an hour ago, so the caller at index 5 is
		// ahead of a store at 4 and is answered stale. The expiry belongs to the
		// window the key really holds, and a stale caller must not be the reason
		// a key that was written without one stays without one.
		_ = s.Client().Persist(ctx, key).Err()
		out, err := runner.Consume(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, IdempotencyKey: idemKey(t),
			Window: fixedWindow(9, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at})
		if err != nil {
			t.Fatalf("Consume: %v", err)
		}
		if out.Transition != Stale {
			t.Fatalf("transition = %q, want stale: the store is at 5 and the target is 9 with a window that has not closed", out.Transition)
		}
		pttl, err := s.Client().PTTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("PTTL: %v", err)
		}
		if pttl <= 0 {
			t.Errorf("PTTL = %s, want a positive expiry: the expiry is set on every call, and a stale verdict is a call (INV-X5)", pttl)
		}
	})
}

// TestT06MissedBoundariesSkippedIsNotMissed: a monthly tenant idle for 97 days is
// in the current window after any call, holding the limit once rather than three
// times.
func TestT06MissedBoundariesSkippedIsNotMissed(t *testing.T) {
	s, runner := integrationRunner(t)
	at := time.Now().UTC()
	anchor := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	feature := featureKey(t)
	key := keyFor(t, feature)
	seed(t, s, key, state{Balance: 3, Limit: 100, Index: 0, Start: anchor, End: ends(anchor.AddDate(0, 1, 0))})

	window := cycle.CurrentWindow(anchor, cycle.Monthly, time.UTC, at)
	if window.Index < 3 {
		t.Fatalf("the fixture is not idle enough: the engine says index %d", window.Index)
	}
	out, err := runner.Consume(context.Background(), Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, IdempotencyKey: idemKey(t),
		Window: window, Limit: 100, At: at})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if out.Transition != Rolled {
		t.Errorf("transition = %q, want rolled after 97 idle days", out.Transition)
	}
	if out.Index != window.Index {
		t.Errorf("index = %d, want the engine's %d (DR-004: the store is a memo, the engine decides)", out.Index, window.Index)
	}
	if out.Balance != 99 {
		t.Errorf("balance = %d, want 99: one allowance, not one per boundary missed", out.Balance)
	}
	if got := balanceOf(t, s, key); got != 99 {
		t.Errorf("the key holds %d, want 99", got)
	}
}

// TestOnlyOneOfManyConcurrentCallersRollsTheCycle is the rollover half of
// INV-C2: fifty requests that all discover the boundary has closed re-grant it
// once between them, not fifty times.
func TestOnlyOneOfManyConcurrentCallersRollsTheCycle(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	feature := featureKey(t)
	key := keyFor(t, feature)
	seed(t, s, key, state{Balance: 7, Limit: 1000, Index: 5, Start: at.Add(-25 * time.Hour), End: ends(at.Add(-time.Hour))})
	req := Request{TenantID: testTenant, FeatureKey: feature, Amount: 1,
		Window: fixedWindow(6, at.Add(-time.Hour), 24*time.Hour), Limit: 1000, At: at}

	const callers = 50
	var mu sync.Mutex
	rolled, current := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			call := req
			call.IdempotencyKey = idemKey(t)
			out, err := runner.Consume(ctx, call)
			if err != nil {
				t.Errorf("Consume: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch out.Transition {
			case Rolled:
				rolled++
			case Current:
				current++
			default:
				t.Errorf("transition = %q, want rolled or current", out.Transition)
			}
		}()
	}
	wg.Wait()
	if rolled != 1 {
		t.Errorf("%d callers reported a rollover, want exactly 1: the allowance is re-granted once (DR-045)", rolled)
	}
	if current != callers-1 {
		t.Errorf("%d callers saw the store already rolled, want %d", current, callers-1)
	}
	// One re-grant of 1000, then every one of the 50 callers spends 1 from it:
	// 1000 - 50. A re-grant per caller would leave this key at 49,950, which is
	// the failure this test exists to catch.
	if got := balanceOf(t, s, key); got != 1000-callers {
		t.Errorf("balance = %d, want %d: one re-grant of 1000 less the %d that were spent (DR-045)", got, 1000-callers, callers)
	}
}

// TestT10ARestartReReadsAndGrantsNothing is T-10 and Definition of Done item 5, for
// the half this phase can reach. A runner that has never seen the key behaves
// exactly like a process that has just started: it reads the state, re-derives
// the window from what the caller gave it, and grants nothing. The process-level
// form of T-10, a SIGKILL under traffic compared against a snapshot, is IP-07's
// because it needs a route to send traffic to.
func TestT10ARestartReReadsAndGrantsNothing(t *testing.T) {
	s, first := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	feature := featureKey(t)
	key := keyFor(t, feature)
	seed(t, s, key, state{Balance: 40, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: ends(at.Add(23 * time.Hour))})
	req := Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, IdempotencyKey: idemKey(t),
		Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at}

	if _, err := first.Consume(ctx, req); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	before := fields(t, s, key)

	// A new runner, and a store whose script cache has been flushed, which is
	// what a restart looks like from here.
	if err := s.Client().ScriptFlush(ctx).Err(); err != nil {
		t.Fatalf("ScriptFlush: %v", err)
	}
	second, err := New(Options{Client: s.Client(), Timeout: dataScriptTimeout})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := second.Check(ctx, req)
	if err != nil {
		t.Fatalf("Check after a flush: %v, and a flushed cache must recover without a restart (ADR-0002)", err)
	}
	if out.Transition != Current {
		t.Errorf("transition = %q after a restart, want current: the same window grants nothing twice", out.Transition)
	}
	if out.Balance != 39 {
		t.Errorf("balance = %d, want 39: a restart re-reads the state, it does not re-grant it", out.Balance)
	}
	assertUnchanged(t, s, key, before, "a restart")
}

// TestDefinitionOfDoneItemSixABalanceThatIsNotThereIsNeverCreated is the
// fail-closed rule, DR-045, and the single most important line in the phase. A
// balance is materialised at provisioning and at plan assignment; a request that
// finds no hash gets 503 and the hash is still not there afterwards, so a
// customer cannot be handed a full allowance by being the first to ask.
func TestDefinitionOfDoneItemSixABalanceThatIsNotThereIsNeverCreated(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	for name, op := range map[string]func(context.Context, Request) (Outcome, error){
		"consume": runner.Consume,
		"refund":  runner.Refund,
		"check":   runner.Check,
	} {
		t.Run(name, func(t *testing.T) {
			feature := featureKey(t) + "-" + name
			key := keyFor(t, feature)
			if err := s.Client().Del(ctx, key).Err(); err != nil {
				t.Fatalf("clear: %v", err)
			}
			out, err := op(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, IdempotencyKey: idemKey(t),
				Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at})
			if !errors.Is(err, ErrStateMissing) {
				t.Fatalf("%s against no balance = %+v, %v; want the state-missing marker, which is 503 (DR-045)", name, out, err)
			}
			if exists, err := s.Client().Exists(ctx, key).Result(); err != nil || exists != 0 {
				t.Errorf("the key exists after a %s against nothing (%d): a request created a balance", name, exists)
			}
			// Nor is one created by a request that rolls a cycle, because a hash
			// that is not there has no cycle to roll.
			after := fields(t, s, key)
			if len(after) != 0 {
				t.Errorf("the hash holds %v, and it must not exist at all", after)
			}
		})
	}
}

// TestDefinitionOfDoneItemSevenTheKeyOutlivesItsWindow is the expiry rule: a
// finite window's key lives until 24 hours after the window closes, which is
// strictly later than any rollover could need it, and an entitlement that never
// resets has a key that never expires (INV-X6, DR-048).
func TestDefinitionOfDoneItemSevenTheKeyOutlivesItsWindow(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)

	t.Run("a window that ends", func(t *testing.T) {
		feature := featureKey(t) + "-ends"
		key := keyFor(t, feature)
		end := at.Add(23 * time.Hour)
		seed(t, s, key, state{Balance: 10, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: &end})
		_, err := runner.Consume(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, IdempotencyKey: idemKey(t),
			Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at})
		if err != nil {
			t.Fatalf("Consume: %v", err)
		}
		want := end.Add(24 * time.Hour)
		// PTTL is answered by the store at one instant and compared here at
		// another, and EXPIREAT rounds the expiry down to a whole second, so the
		// two ends of the call bound what the reading can legitimately be. The
		// bounds are the two instants either side of the call plus a second of
		// rounding; anything outside them is a figure the script did not write.
		before := time.Now()
		pttl, err := s.Client().PTTL(ctx, key).Result()
		after := time.Now()
		if err != nil {
			t.Fatalf("PTTL: %v", err)
		}
		low, high := want.Sub(after)-time.Second, want.Sub(before)+time.Second
		if pttl < low || pttl > high {
			t.Errorf("PTTL = %s, want window_end + 24h, which is between %s and %s from now",
				pttl, low.Truncate(time.Second), high.Truncate(time.Second))
		}
		if pttl <= time.Until(end) {
			t.Errorf("PTTL = %s, and the key must outlive the window it serves", pttl)
		}
		// The Go side computes the same figure, so the two cannot drift without a
		// test noticing.
		exp, ok := store.KeyExpiry(&end)
		if !ok || !exp.Equal(want) {
			t.Errorf("store.KeyExpiry = %v, %v; want %v, true", exp, ok, want)
		}
	})

	t.Run("an entitlement that never ends", func(t *testing.T) {
		feature := featureKey(t) + "-never"
		key := keyFor(t, feature)
		seed(t, s, key, state{Balance: 10, Limit: 100, Index: 0, Start: at.Add(-time.Hour)})
		_, err := runner.Consume(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, IdempotencyKey: idemKey(t),
			Window: cycle.Window{Index: 0, Start: at.Add(-time.Hour)}, Limit: 100, At: at})
		if err != nil {
			t.Fatalf("Consume: %v", err)
		}
		pttl, err := s.Client().PTTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("PTTL: %v", err)
		}
		if pttl != -1 {
			t.Errorf("PTTL = %s, and a `never` entitlement has no expiry (DR-008)", pttl)
		}
	})
}

// TestDefinitionOfDoneItemNineOneRequestIsOneCommand counts the commands on the
// wire, on a real store, through a real client. The unit test proves it with a
// fake; this proves it with the thing the request actually does.
func TestDefinitionOfDoneItemNineOneRequestIsOneCommand(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	feature := featureKey(t)
	key := keyFor(t, feature)
	seed(t, s, key, state{Balance: 100, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: ends(at.Add(23 * time.Hour))})

	counter := &commandCounter{}
	s.Client().AddHook(counter)

	for i := 0; i < 3; i++ {
		if _, err := runner.Consume(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, IdempotencyKey: idemKey(t),
			Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at}); err != nil {
			t.Fatalf("Consume: %v", err)
		}
	}
	got := counter.recorded()
	if len(got) != 3 {
		t.Fatalf("three consumes produced %d commands, %v; one each and nothing else (performance.md Â§3)", len(got), got)
	}
	for i, name := range got {
		if !strings.EqualFold(name, "evalsha") {
			t.Errorf("command %d was %q, want evalsha: a whole body on the wire is a second copy of the rules (NFR-S8)", i+1, name)
		}
	}
}

// TestAFlushedScriptCacheIsRecoveredFromWithoutARestart is the lazy half of
// ADR-0002 against the real store, and the reason the runner holds a digest rather
// than a body.
func TestAFlushedScriptCacheIsRecoveredFromWithoutARestart(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	feature := featureKey(t)
	key := keyFor(t, feature)
	seed(t, s, key, state{Balance: 100, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: ends(at.Add(23 * time.Hour))})
	req := Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, IdempotencyKey: idemKey(t),
		Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at}

	if err := s.Client().ScriptFlush(ctx).Err(); err != nil {
		t.Fatalf("ScriptFlush: %v", err)
	}
	// A raw EVALSHA first, so the assertion is about the runner and not about a
	// script that happens to still be cached.
	raw, err := s.Client().EvalSha(ctx, consumeBody.digest, []string{key, idemKeyFor(t, req.IdempotencyKey)}, args(Consume, req)...).Result()
	if !isNoScript(err) {
		t.Fatalf("EvalSha after a flush returned %v, want NOSCRIPT", err)
	}
	if _, ok := raw.([]any); ok {
		t.Errorf("a flushed store answered a table, so it was not flushed")
	}

	out, err := runner.Consume(ctx, req)
	if err != nil {
		t.Fatalf("Consume after a flush: %v", err)
	}
	if out.Balance != 99 {
		t.Errorf("balance = %d, want 99", out.Balance)
	}
}

// TestTheStoreHoldsTheBytesThisBinaryEmbeds proves NFR-S8's claim that the
// scripts are the ones that were tested: the digest the store computes for the
// embedded body is the digest the runner asks for. Load refuses a mismatch, so
// this is asserted by a direct comparison as well, in case Load ever stops.
func TestTheStoreHoldsTheBytesThisBinaryEmbeds(t *testing.T) {
	s, _ := integrationRunner(t)
	ctx := context.Background()
	for _, b := range bodies() {
		got, err := s.Client().ScriptLoad(ctx, b.text).Result()
		if err != nil {
			t.Fatalf("SCRIPT LOAD: %v", err)
		}
		if got != b.digest {
			t.Errorf("the store computed %s for the %s script, and this binary computed %s", got, b.op, b.digest)
		}
	}
}

// TestDefinitionOfDoneItemEightNoRequestFieldIsEverScriptText is the injection
// test. The Go types make it impossible - the tenant and the feature are already
// key components and the amount is already an integer - so the values are put on
// the wire by hand, as the text a hostile caller would send, and the store is
// asked what it made of them.
func TestDefinitionOfDoneItemEightNoRequestFieldIsEverScriptText(t *testing.T) {
	s, _ := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	feature := featureKey(t)
	key := keyFor(t, feature)
	seed(t, s, key, state{Balance: 100, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: ends(at.Add(23 * time.Hour))})
	before := fields(t, s, key)

	window := args(Consume, Request{Amount: 1, Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at})
	// The record key is a second key the script reads. It is a real, empty one,
	// so the hostile amount is refused by the amount check before the lookup and
	// the assertion is about the amount rather than about a missing key.
	hostile := []any{
		`1) ; redis.call('SET','qc:pwned','1') --`,
		window[1], window[2], window[3], window[4], window[5], window[6],
	}
	raw, err := s.Client().EvalSha(ctx, consumeBody.digest, []string{key, idemKeyFor(t, idemKey(t))}, hostile...).Result()
	if err != nil {
		t.Fatalf("EvalSha: %v", err)
	}
	out, err := decode(Consume, raw)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a hostile amount was answered %+v, %v; want invalid, because an amount is a number", out, err)
	}
	if exists, err := s.Client().Exists(ctx, "qc:pwned").Result(); err != nil || exists != 0 {
		t.Errorf("qc:pwned exists, so a request field was executed as script text")
	}
	assertUnchanged(t, s, key, before, "a hostile amount")

	// The same in the key, which is the one place a caller's text reaches the
	// store as a name rather than as a value. A key is a key: the script never
	// parses it, so the fragment is part of a name and nothing else.
	fragments := []string{
		`x'); redis.call('SET','qc:pwned2','1`,
		`x" or "1"=="1`,
		`x}` + strings.Repeat("a", 512),
	}
	for _, fragment := range fragments {
		hostileKey := key + "-" + fragment
		raw, err := s.Client().EvalSha(ctx, consumeBody.digest, []string{hostileKey, idemKeyFor(t, idemKey(t))}, window...).Result()
		if err != nil {
			t.Fatalf("EvalSha with a hostile key: %v", err)
		}
		if _, err := decode(Consume, raw); err != nil && !errors.Is(err, ErrStateMissing) && !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("a key of the shape %q was answered %v, and only a missing state or a bad argument is acceptable", fragment, err)
		}
		if exists, err := s.Client().Exists(ctx, "qc:pwned2").Result(); err != nil || exists != 0 {
			t.Errorf("qc:pwned2 exists after a request with a hostile key")
		}
		// The hostile key is a key like any other, so clean up after it.
		if err := s.Client().Del(ctx, hostileKey).Err(); err != nil {
			t.Errorf("Del: %v", err)
		}
	}
}

// TestTheScriptsWorkIsNotProportionalToTheTenant is the bounded-work rule. Two
// hundred sibling hashes under the same tenant, one hash touched, and the cost of
// the call is the same. A script that walked the keyspace, or read the whole hash
// family, would show it here.
func TestTheScriptsWorkIsNotProportionalToTheTenant(t *testing.T) {
	s, runner := integrationRunner(t)
	at := time.Now().UTC().Truncate(time.Second)
	end := at.Add(23 * time.Hour)
	feature := featureKey(t)
	key := keyFor(t, feature)
	seed(t, s, key, state{Balance: 100000, Limit: 100000, Index: 5, Start: at.Add(-time.Hour), End: &end})
	req := Request{TenantID: testTenant, FeatureKey: feature, Amount: 1, IdempotencyKey: idemKey(t),
		Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100000, At: at}

	alone := measureConsume(t, runner, req)
	seedSiblings(t, s, t.Name(), 200)
	crowded := measureConsume(t, runner, req)

	// Three is deliberately loose. A script that read 200 hashes would be two
	// orders of magnitude slower, and a generous bound keeps this from failing
	// on a noisy machine while still failing on a linear walk.
	if crowded > 3*alone {
		t.Errorf("a consume took %s alone and %s with 200 sibling hashes, so the work grows with the tenant (NFR-S2)", alone, crowded)
	}
	t.Logf("a consume took %s alone and %s with 200 sibling hashes", alone, crowded)
}

func seedSiblings(t *testing.T, s *store.Store, prefix string, count int) {
	t.Helper()
	ctx := context.Background()
	var keys []string
	t.Cleanup(func() {
		if len(keys) > 0 {
			_ = s.Client().Del(ctx, keys...).Err()
		}
	})
	for i := 0; i < count; i++ {
		feature := fmt.Sprintf("%s-sibling-%d", prefix, i)
		key := keyFor(t, feature)
		keys = append(keys, key)
		if err := s.Client().HSet(ctx, key, map[string]any{
			"balance": 50, "limit": 100, "bonus": 0, "cycle_index": 5,
			"window_start": time.Now().UnixMilli(), "window_end": time.Now().Add(24 * time.Hour).UnixMilli(),
		}).Err(); err != nil {
			t.Fatalf("seed sibling: %v", err)
		}
	}
}

// measureConsume runs the call a few times and takes the median, because a single
// timing on a shared machine is a coin toss and a coin toss is not a test.
func measureConsume(t *testing.T, runner *Runner, req Request) time.Duration {
	t.Helper()
	// Each sample is a distinct operation, so the samples measure a consume and
	// not a replay: a repeat of one key would be served from the record and
	// would be the one call this test is not about.
	var samples []time.Duration
	for i := 0; i < 11; i++ {
		call := req
		call.IdempotencyKey = idemKey(t)
		start := time.Now()
		if _, err := runner.Consume(context.Background(), call); err != nil {
			t.Fatalf("Consume: %v", err)
		}
		samples = append(samples, time.Since(start))
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[len(samples)/2]
}

// TestT09AFailedRequestLeavesNothingBehind is T-09 and Definition of Done item 4,
// at the only level a test can reach without a process to kill: the wire. A
// command that arrives in full is applied in full, and a command that arrives in
// part is not applied at all, and neither leaves a hash with half its fields.
func TestT09AFailedRequestLeavesNothingBehind(t *testing.T) {
	s, _ := integrationRunner(t)
	addr, ok := wireAddress(t)
	if !ok {
		return
	}
	at := time.Now().UTC().Truncate(time.Second)
	feature := featureKey(t)
	key := keyFor(t, feature)
	seedEnd := at.Add(23 * time.Hour)
	seed(t, s, key, state{Balance: 100, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: &seedEnd})

	// EVALSHA takes the number of keys between the digest and the key, exactly
	// as EVAL does. A test that leaves it out asks the server to read the key
	// itself as a count, which fails with an integer error that has nothing to
	// do with the script.
	// The command is rebuilt for each attempt, because each attempt is a
	// distinct operation under its own Idempotency-Key. A shared key would turn
	// every attempt after the first into a replay, and the balance the test
	// waits for would never move again.
	full := func() []byte {
		clientKey := idemKey(t)
		return respCommand("EVALSHA", consumeBody.digest, "2", key, idemKeyFor(t, clientKey),
			"30", "5", strconv.FormatInt(at.Add(-time.Hour).UnixMilli(), 10),
			strconv.FormatInt(at.Add(23*time.Hour).UnixMilli(), 10), "100", strconv.FormatInt(at.UnixMilli(), 10),
			Fingerprint(Consume, testTenant, feature, 30))
	}

	t.Run("a command that arrives in full is applied in full", func(t *testing.T) {
		dropConnection(t, addr, full(), false)
		waitFor(t, s, key, 70)
		assertWholeHash(t, s, key)
	})

	t.Run("a command that arrives in part is not applied at all", func(t *testing.T) {
		dropConnection(t, addr, full(), true)
		// Nothing to wait for: the server had an incomplete command, so there is
		// no state to catch up with. A short pause makes a failure legible
		// instead of a race against the next line.
		time.Sleep(200 * time.Millisecond)
		if got := balanceOf(t, s, key); got != 70 {
			t.Errorf("balance = %d, want 70: a half-sent command must not be applied", got)
		}
		assertWholeHash(t, s, key)
	})

	t.Run("repeated failures never produce a third value", func(t *testing.T) {
		// Each attempt starts from the same 100, so the outcome is decided by
		// whether the bytes arrived rather than by how many have gone before.
		// Letting the balance carry over would make a half-applied write
		// indistinguishable from a whole one, which is the thing being tested.
		for i := 0; i < 10; i++ {
			half := i%2 == 0
			end := at.Add(23 * time.Hour)
			seed(t, s, key, state{Balance: 100, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: &end})
			dropConnection(t, addr, full(), half)
			// A whole command is applied and lands at 70; an incomplete one is
			// discarded and the balance stays at 100. Nothing between the two is
			// a value this key may ever hold (T-09).
			want := int64(100)
			if !half {
				want = 70
			}
			waitFor(t, s, key, want)
			assertWholeHash(t, s, key)
		}
	})
}

// assertWholeHash is the other half of T-09: a failure must not leave a hash with
// some of its fields written.
func assertWholeHash(t *testing.T, s *store.Store, key string) {
	t.Helper()
	got := fields(t, s, key)
	want := []string{"balance", "limit", "bonus", "cycle_index", "window_start", "window_end"}
	if len(got) != len(want) {
		t.Errorf("the hash holds %d fields rather than %d: %v", len(got), len(want), got)
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("the hash has no %s after an aborted request: %v", name, got)
		}
	}
}

func waitFor(t *testing.T, s *store.Store, key string, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if balanceOf(t, s, key) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := balanceOf(t, s, key); got != want {
		t.Errorf("balance = %d, want %d", got, want)
	}
}

// dropConnection writes a command and closes the socket without reading the
// reply, which is what a killed process leaves behind: the request in flight and
// nobody to answer. When half is set, only half the bytes are written, so the
// server has an incomplete command and must discard it.
//
// The close is a graceful one. A reset would be a lie about the test: it can
// discard bytes the server has not read yet, and then the full-write case would
// sometimes look like the partial one and the assertion would be untestable.
func dropConnection(t *testing.T, addr string, command []byte, half bool) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	payload := command
	if half {
		payload = command[:len(command)/2]
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// wireAddress is the host and port a raw connection needs. It is a separate
// helper because a test that cannot reach the wire says so and skips rather than
// passing quietly.
func wireAddress(t *testing.T) (string, bool) {
	t.Helper()
	redisURL := os.Getenv("QUOTACORE_REDIS_URL")
	if redisURL == "" {
		t.Skip("QUOTACORE_REDIS_URL is unset")
	}
	parsed, err := url.Parse(redisURL)
	if err != nil {
		t.Fatalf("parse %q: %v", redisURL, err)
	}
	if parsed.Scheme != "redis" {
		t.Skipf("the datastore is %s, and this test needs a plain TCP connection", parsed.Scheme)
	}
	host := parsed.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "6379")
	}
	return host, true
}

// respCommand builds a RESP array, because the point of the test is to control
// exactly which bytes reach the store and a client library would not.
func respCommand(args ...string) []byte {
	var out strings.Builder
	fmt.Fprintf(&out, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&out, "$%d\r\n%s\r\n", len(arg), arg)
	}
	return []byte(out.String())
}

// commandCounter counts the commands a request puts on the wire, which is the
// only place the round-trip rule can be observed.
type commandCounter struct {
	mu    sync.Mutex
	names []string
}

func (c *commandCounter) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c *commandCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.mu.Lock()
		c.names = append(c.names, cmd.Name())
		c.mu.Unlock()
		return next(ctx, cmd)
	}
}

func (c *commandCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		c.mu.Lock()
		for _, cmd := range cmds {
			c.names = append(c.names, cmd.Name())
		}
		c.mu.Unlock()
		return next(ctx, cmds)
	}
}

func (c *commandCounter) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.names...)
}
