package script

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/rolling"
	"github.com/quotacore/quotacore/internal/store"
)

// otherTenant is a second tenant, beside testTenant, for the binding tests.
const otherTenant = "7c1e2f40-3b5a-4d68-9e07-2a4f6b8c0d13"

// TestT19TheScriptRefusesAForeignTenant is IP-20 DoD 2: the runner is called
// directly, with no handler in front of it, for testTenant's keys and a
// binding to another tenant. Every data-plane script answers the foreign
// decision before its first command, so nothing in the tenant's slot is read
// or written: the balance hash, the bucket hash and the idempotency records
// are as they were, and the same calls with the right binding apply (DR-053).
func TestT19TheScriptRefusesAForeignTenant(t *testing.T) {
	s, runner := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)
	feature := featureKey(t)
	key := keyFor(t, feature)
	end := at.Add(23 * time.Hour)
	seed(t, s, key, state{Balance: 50, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: &end})
	window := fixedWindow(5, at.Add(-time.Hour), 24*time.Hour)
	rollingFeature := feature + "-r"
	bucket, err := store.BucketCounterKey(testTenant, rollingFeature)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Client().Del(ctx, bucket).Err()
	w, _ := rolling.Parse("rolling_5m")

	clientKey := idemKey(t)
	anchored := func(binding string) Request {
		return Request{TenantID: testTenant, FeatureKey: feature, IdempotencyKey: clientKey, Amount: 1,
			Window: window, Limit: 100, At: at, Binding: binding}
	}
	sliding := func(binding string) RollingRequest {
		return RollingRequest{TenantID: testTenant, FeatureKey: rollingFeature, IdempotencyKey: clientKey, Amount: 1,
			Limit: 10, Window: w, At: at, Binding: binding}
	}
	calls := []struct {
		name string
		run  func(binding string) error
	}{
		{"consume", func(b string) error { _, err := runner.Consume(ctx, anchored(b)); return err }},
		{"refund", func(b string) error { _, err := runner.Refund(ctx, anchored(b)); return err }},
		{"check", func(b string) error { _, err := runner.Check(ctx, anchored(b)); return err }},
		{"consume_boolean", func(b string) error { _, err := runner.ConsumeBoolean(ctx, anchored(b)); return err }},
		{"check_boolean", func(b string) error { _, err := runner.CheckBoolean(ctx, anchored(b)); return err }},
		{"consume_rolling", func(b string) error { _, err := runner.ConsumeRolling(ctx, sliding(b)); return err }},
		{"check_rolling", func(b string) error { _, err := runner.CheckRolling(ctx, sliding(b)); return err }},
		{"refund_rolling", func(b string) error { _, err := runner.RefundRolling(ctx, sliding(b)); return err }},
	}
	idem := idemKeyFor(t, clientKey)
	before := fields(t, s, key)
	for _, c := range calls {
		if err := c.run(otherTenant); !errors.Is(err, ErrForeignTenant) {
			t.Errorf("%s bound to another tenant: %v, want ErrForeignTenant", c.name, err)
		}
	}
	assertUnchanged(t, s, key, before, "a foreign binding")
	for _, k := range []string{bucket, idem} {
		if n, err := s.Client().Exists(ctx, k).Result(); err != nil || n != 0 {
			t.Errorf("%s exists after only foreign calls: %d %v", k, n, err)
		}
	}

	// The tenant's own binding, and no binding, are the ordinary path.
	if out, err := runner.Consume(ctx, anchored(testTenant)); err != nil || out.Decision != Applied || out.Balance != 49 {
		t.Fatalf("consume bound to its own tenant: %+v %v", out, err)
	}
	if out, err := runner.Check(ctx, Request{TenantID: testTenant, FeatureKey: feature, Amount: 1,
		Window: window, Limit: 100, At: at}); err != nil || !out.Allowed() {
		t.Fatalf("an unbound check: %+v %v", out, err)
	}
	own := sliding(testTenant)
	own.IdempotencyKey = idemKey(t) // the anchored consume above recorded clientKey
	if out, err := runner.ConsumeRolling(ctx, own); err != nil || out.Decision != Applied {
		t.Fatalf("a rolling consume bound to its own tenant: %+v %v", out, err)
	}
}

// TestT19AScriptCallWithNoBindingFailsClosed: a caller that sends the seven
// arguments of the release before this one, with no binding at all, is
// refused as invalid and changes nothing. A missing binding is never read as
// an unbound key (DR-053).
func TestT19AScriptCallWithNoBindingFailsClosed(t *testing.T) {
	s, _ := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)
	feature := featureKey(t)
	key := keyFor(t, feature)
	end := at.Add(23 * time.Hour)
	seed(t, s, key, state{Balance: 50, Limit: 100, Index: 5, Start: at.Add(-time.Hour), End: &end})
	before := fields(t, s, key)
	clientKey := idemKey(t)
	raw, err := s.Client().EvalSha(ctx, consumeBody.digest, []string{key, idemKeyFor(t, clientKey)},
		"1", "5", strconv.FormatInt(at.Add(-time.Hour).UnixMilli(), 10), strconv.FormatInt(end.UnixMilli(), 10),
		"100", strconv.FormatInt(at.UnixMilli(), 10), Fingerprint(Consume, testTenant, feature, 1)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decode(Consume, raw); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a consume with no binding argument decoded as %v, want ErrInvalidArgument", err)
	}
	assertUnchanged(t, s, key, before, "a call with no binding")
}

// TestT19TheBalanceReadRefusesAForeignTenant: the balance route runs no
// script, so the binding is checked at the store's edge, before a key is
// built (DR-053).
func TestT19TheBalanceReadRefusesAForeignTenant(t *testing.T) {
	s, _ := integrationRunner(t)
	ctx := context.Background()
	at := time.Now().UTC()
	feature := featureKey(t)
	end := at.Add(time.Hour)
	seed(t, s, keyFor(t, feature), state{Balance: 7, Limit: 10, Start: at, End: &end})
	if _, err := s.ReadBalancesAs(ctx, otherTenant, testTenant, []string{feature}); !errors.Is(err, store.ErrForeignTenant) {
		t.Errorf("a foreign binding's balance read: %v, want ErrForeignTenant", err)
	}
	for _, binding := range []string{"", testTenant} {
		got, err := s.ReadBalancesAs(ctx, binding, testTenant, []string{feature})
		if err != nil || len(got) != 1 || got[0].Balance != 7 {
			t.Errorf("the balance read bound to %q: %+v %v", binding, got, err)
		}
	}
}
