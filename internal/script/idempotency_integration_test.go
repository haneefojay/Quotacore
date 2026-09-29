package script

import (
	"context"
	"errors"
	"math/rand"
	"strconv"
	"sync"
	"testing"
	"time"
)

// This file is IP-06's evidence: the properties that only a real store can
// prove. A record written inside a script, compared against inside the same
// script, and returned without a write are three claims about atomicity and
// ordering rather than about this code's spelling, so a fake would prove that
// the fake was called as expected and nothing more (ADR-0002).

// TestT02FiftyDuplicateDeliveriesAreChargedOnce is T-02 and Definition of Done
// item 1 at the data plane: 50 concurrent deliveries of one Idempotency-Key with
// amount 30 leave the balance at 70, exactly one call is applied, and the other
// 49 replay the state the first recorded. The one charge is a property of the
// store's single execution, not of the runner, so this is the test that a second
// instance cannot break (DR-027).
func TestT02FiftyDuplicateDeliveriesAreChargedOnce(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	feature := featureKey(t)
	key := keyFor(t, feature)
	seed(t, s, key, state{Balance: 100, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: ends(at.Add(23 * time.Hour))})

	clientKey := idemKey(t)
	req := Request{TenantID: testTenant, FeatureKey: feature, Amount: 30, IdempotencyKey: clientKey,
		Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at}

	const callers = 50
	var mu sync.Mutex
	var outcomes []Outcome
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := runner.Consume(ctx, req)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("Consume: %v", err)
			case out.Decision == Applied, out.Decision == Replayed:
				outcomes = append(outcomes, out)
			default:
				t.Errorf("decision = %q, and a duplicate delivery is applied once and replayed after that (T-02)", out.Decision)
			}
		}()
	}
	wg.Wait()

	if len(outcomes) != callers {
		t.Fatalf("%d of %d deliveries were answerable, want every one", len(outcomes), callers)
	}
	applied := 0
	for _, out := range outcomes {
		if out.Decision == Applied {
			applied++
		}
	}
	if applied != 1 {
		t.Errorf("%d deliveries were applied, want exactly 1: the other 49 are replays (DR-027)", applied)
	}
	// The stored state is what every caller was told, so the retries agree with
	// the one that paid. The decision differs - applied for the first, replayed
	// for the rest - and the seven values behind it do not (DR-027).
	first := outcomes[0]
	for i, out := range outcomes {
		if out.Balance != 70 || out.Limit != 100 || out.Bonus != 0 || out.Index != 5 {
			t.Errorf("delivery %d reported balance, limit, bonus, index = %d, %d, %d, %d; want 70, 100, 0, 5",
				i, out.Balance, out.Limit, out.Bonus, out.Index)
		}
		if !out.Start.Equal(first.Start) || (out.End == nil) != (first.End == nil) ||
			(out.End != nil && !out.End.Equal(*first.End)) {
			t.Errorf("delivery %d reported a window the first delivery did not", i)
		}
	}
	if got := balanceOf(t, s, key); got != 70 {
		t.Errorf("balance = %d, and 50 deliveries of 30 from 100 is 70 (T-02)", got)
	}
	record := idemKeyFor(t, clientKey)
	if exists, err := s.Client().Exists(ctx, record).Result(); err != nil || exists != 1 {
		t.Errorf("the record exists %d times (%v), want exactly once", exists, err)
	}
}

// TestT02AReplayReportsTheRecordedStateNotTheLiveOne is the replay-invisibility
// rule: a replay is answered from the record and never reads the balance, so it
// reports the state the operation left when it was first applied even after the
// balance has moved on. A replay that reported the live state would tell a
// retrying client about a charge that was not its own.
func TestT02AReplayReportsTheRecordedStateNotTheLiveOne(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	feature := featureKey(t)
	key := keyFor(t, feature)
	seed(t, s, key, state{Balance: 100, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: ends(at.Add(23 * time.Hour))})
	call := func(amount int64, clientKey string) Request {
		return Request{TenantID: testTenant, FeatureKey: feature, Amount: amount, IdempotencyKey: clientKey,
			Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at}
	}

	clientKey := idemKey(t)
	first, err := runner.Consume(ctx, call(30, clientKey))
	if err != nil || first.Decision != Applied {
		t.Fatalf("first Consume = %+v, %v; want applied", first, err)
	}
	// Another caller, another key, and the balance moves on without the record
	// for the first charge knowing about it.
	if other, err := runner.Consume(ctx, call(10, idemKey(t))); err != nil || other.Balance != 60 {
		t.Fatalf("second Consume = %+v, %v; want applied at 60", other, err)
	}

	replay, err := runner.Consume(ctx, call(30, clientKey))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Decision != Replayed {
		t.Fatalf("decision = %q, want replayed", replay.Decision)
	}
	if replay.Balance != 70 {
		t.Errorf("replay reported balance %d, want the recorded 70 and not the live 60", replay.Balance)
	}
	if got := balanceOf(t, s, key); got != 60 {
		t.Errorf("the key holds %d after a replay, and a replay writes nothing", got)
	}
}

// TestT03AReusedKeyIsRefusedAndTheRecordSurvives is T-03 and Definition of Done
// item 2: a key that was used for one operation is no longer usable for another,
// and the refusal damages neither the balance nor the record that already exists.
// The last assertion is INV-I2 and the one that is easy to get wrong by
// overwriting the record before comparing (DR-028).
func TestT03AReusedKeyIsRefusedAndTheRecordSurvives(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	feature := featureKey(t)
	key := keyFor(t, feature)
	window := fixedWindow(5, at.Add(-time.Hour), 24*time.Hour)
	seed(t, s, key, state{Balance: 100, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: ends(at.Add(23 * time.Hour))})

	clientKey := idemKey(t)
	if out, err := runner.Consume(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 30,
		IdempotencyKey: clientKey, Window: window, Limit: 100, At: at}); err != nil || out.Decision != Applied {
		t.Fatalf("the first use = %+v, %v; want applied", out, err)
	}
	before := fields(t, s, key)

	t.Run("a different amount", func(t *testing.T) {
		if _, err := runner.Consume(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 31,
			IdempotencyKey: clientKey, Window: window, Limit: 100, At: at}); !errors.Is(err, ErrIdempotencyKeyReuse) {
			t.Fatalf("amount 31 under the same key = %v, want the reuse marker, which is 409", err)
		}
		assertUnchanged(t, s, key, before, "a reused key")
	})

	t.Run("a different feature", func(t *testing.T) {
		otherFeature := feature + "-other"
		otherKey := keyFor(t, otherFeature)
		seed(t, s, otherKey, state{Balance: 100, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: ends(at.Add(23 * time.Hour))})
		if _, err := runner.Consume(ctx, Request{TenantID: testTenant, FeatureKey: otherFeature, Amount: 30,
			IdempotencyKey: clientKey, Window: window, Limit: 100, At: at}); !errors.Is(err, ErrIdempotencyKeyReuse) {
			t.Fatalf("another feature under the same key = %v, want the reuse marker", err)
		}
		if got := balanceOf(t, s, otherKey); got != 100 {
			t.Errorf("the other feature holds %d, and a refusal and a reuse change nothing", got)
		}
		assertUnchanged(t, s, key, before, "a reused key")
	})

	t.Run("a different operation", func(t *testing.T) {
		if _, err := runner.Refund(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 30,
			IdempotencyKey: clientKey, Window: window, Limit: 100, At: at}); !errors.Is(err, ErrIdempotencyKeyReuse) {
			t.Fatalf("a refund under a consume's key = %v, want the reuse marker", err)
		}
		assertUnchanged(t, s, key, before, "a reused key")
	})

	// The record that was already there still answers the operation it belongs
	// to, which is the whole of INV-I2.
	replay, err := runner.Consume(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 30,
		IdempotencyKey: clientKey, Window: window, Limit: 100, At: at})
	if err != nil {
		t.Fatalf("the original call replayed after three refusals: %v", err)
	}
	if replay.Decision != Replayed || replay.Balance != 70 {
		t.Errorf("the original call replayed as %+v, want replayed at 70", replay)
	}
}

// TestThePropertyAReplayAppliesAtMostOnce is Definition of Done item 4: for any
// amount and any number of duplicate deliveries, across more than one runner
// standing in for more than one instance, the mutation is applied at most once.
// A process-level cache would pass a single-runner version of this and fail this
// one, which is the point: the guarantee is in the store (ADR-0004).
func TestThePropertyAReplayAppliesAtMostOnce(t *testing.T) {
	s, first := integrationRunner(t)
	ctx := context.Background()
	second, err := New(Options{Client: s.Client(), Timeout: dataScriptTimeout})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runners := []*Runner{first, second}
	at := time.Now().UTC().Truncate(time.Second)
	window := fixedWindow(5, at.Add(-time.Hour), 24*time.Hour)

	rng := rand.New(rand.NewSource(20260930))
	for i := 0; i < 25; i++ {
		amount := int64(1 + rng.Intn(50))
		dupes := 2 + rng.Intn(8)
		feature := featureKey(t) + "-" + strconv.Itoa(i)
		key := keyFor(t, feature)
		seed(t, s, key, state{Balance: 1000, Limit: 1000, Index: 5, Start: at.Add(-time.Hour), End: ends(at.Add(23 * time.Hour))})
		clientKey := idemKey(t)
		req := Request{TenantID: testTenant, FeatureKey: feature, Amount: amount, IdempotencyKey: clientKey,
			Window: window, Limit: 1000, At: at}

		applied := 0
		for j := 0; j < dupes; j++ {
			out, err := runners[j%len(runners)].Consume(ctx, req)
			if err != nil {
				t.Fatalf("Consume %d/%d: %v", i, j, err)
			}
			switch out.Decision {
			case Applied:
				applied++
			case Replayed:
			default:
				t.Fatalf("decision = %q, and a duplicate is applied or replayed", out.Decision)
			}
			if out.Balance != 1000-amount {
				t.Fatalf("reply balance = %d, want the one charge of %d from 1000", out.Balance, amount)
			}
		}
		if applied != 1 {
			t.Errorf("amount %d delivered %d times was applied %d times, want once", amount, dupes, applied)
		}
		if got := balanceOf(t, s, key); got != 1000-amount {
			t.Errorf("amount %d delivered %d times left %d, want %d", amount, dupes, got, 1000-amount)
		}
	}
}

// TestTheRecordIsBoundedByTheTwentyFourHourWindow is Definition of Done item 5:
// the record carries the documented window, and a repeat of it does not extend
// that window. The second half is DR-029's actual guarantee - the window is
// measured from first use - and a script that refreshed the TTL on every read
// would let a busy retry loop hold a record, and the memory it costs, forever.
func TestTheRecordIsBoundedByTheTwentyFourHourWindow(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	feature := featureKey(t)
	key := keyFor(t, feature)
	seed(t, s, key, state{Balance: 100, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: ends(at.Add(23 * time.Hour))})
	clientKey := idemKey(t)
	req := Request{TenantID: testTenant, FeatureKey: feature, Amount: 30, IdempotencyKey: clientKey,
		Window: fixedWindow(5, at.Add(-time.Hour), 24*time.Hour), Limit: 100, At: at}

	if out, err := runner.Consume(ctx, req); err != nil || out.Decision != Applied {
		t.Fatalf("Consume = %+v, %v; want applied", out, err)
	}
	record := idemKeyFor(t, clientKey)
	ttl, err := s.Client().TTL(ctx, record).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	// Redis rounds a `SET ... EX 86400` up to the next whole second, so the
	// reading is at most one second above the pinned window and cannot be below
	// it by construction.
	if ttl > 24*time.Hour+time.Second || ttl < 24*time.Hour-time.Minute {
		t.Errorf("the record's TTL is %s, want the pinned 24h window (DR-029)", ttl)
	}

	time.Sleep(1100 * time.Millisecond)
	if out, err := runner.Consume(ctx, req); err != nil || out.Decision != Replayed {
		t.Fatalf("the repeat = %+v, %v; want replayed", out, err)
	}
	after, err := s.Client().TTL(ctx, record).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if after >= ttl {
		t.Errorf("the record's TTL went from %s to %s across a replay, so a repeat extended the window (DR-029)", ttl, after)
	}
	if after <= 0 {
		t.Errorf("the record's TTL is %s after a replay, want it still inside the window", after)
	}
}
