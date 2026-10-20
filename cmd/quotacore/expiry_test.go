package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// noExpiry fails the test unless the tenant-feature's balance hash exists and
// carries no expiry. A hash that expires is a tenant locked out: nothing on
// the request path may materialise it again (DR-045), and no v0.1 phase runs
// a worker that keeps an idle tenant's hash alive.
func (s *stack) noExpiry(tn tenant, feature, after string) {
	s.t.Helper()
	pttl, err := s.rdb.PTTL(context.Background(), s.balanceKey(tn, feature)).Result()
	if err != nil {
		s.t.Fatal(err)
	}
	if pttl != -1 {
		s.t.Errorf("after %s the balance hash has PTTL %s, want -1: no expiry", after, pttl)
	}
}

// TestABalanceHashNeverExpires is the idle-tenant lockout: a hash written
// with `EXPIREAT window_end + 24h` vanishes from a tenant that makes no request
// for a day after its window closes, and that tenant is then answered 503
// until a rebuild. Every path that writes a hash must leave it without one:
// provisioning, the four data-plane routes, grant, set, force-rollover, an
// immediate override, a plan change and a rollover across a boundary.
func TestABalanceHashNeverExpires(t *testing.T) {
	s := newStack(t)
	f, plan := uniq("xf"), uniq("xp")
	s.createFeature(f)
	s.createPlan(plan, 1000, "hourly", f)
	tn := asTenant(t, s.createTenant(externalID("xt"), plan, "UTC"), ent{f, 1000, "hourly", false})
	tp := "/v1/admin/tenants/" + tn.ID
	s.noExpiry(tn, f, "provisioning")

	mustStatus(t, s.consume(s.runtime, f, 30, tn.External, newKey()), http.StatusOK)
	s.noExpiry(tn, f, "a consume")
	mustStatus(t, s.refund(f, 10, tn.External, newKey()), http.StatusOK)
	s.noExpiry(tn, f, "a refund")
	mustStatus(t, s.check(f, tn.External), http.StatusOK)
	s.noExpiry(tn, f, "a check")
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/grant", `{"feature_key":"`+f+`","amount":5,"reason":"x"}`), http.StatusOK)
	s.noExpiry(tn, f, "a grant")
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/set", `{"feature_key":"`+f+`","value":700,"reason":"x"}`), http.StatusOK)
	s.noExpiry(tn, f, "a set")
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/force-rollover", `{"feature_key":"`+f+`","reason":"x"}`), http.StatusOK)
	s.noExpiry(tn, f, "a force-rollover")
	mustStatus(t, s.adminDo(http.MethodPost, tp+"/overrides",
		`{"feature_key":"`+f+`","limit_value":600,"interval":null,"apply_at":"immediate"}`), http.StatusOK)
	s.noExpiry(tn, f, "an immediate override")
	other := uniq("xq")
	s.createPlan(other, 400, "hourly", f)
	mustStatus(t, s.adminDo(http.MethodPatch, tp, fmt.Sprintf(`{"version":%d,"plan_key":%q}`, s.version(tp), other)), http.StatusOK)
	s.noExpiry(tn, f, "a plan change")

	// A request that rolls the window: the hash is moved back one window, as
	// a tenant idle across a boundary leaves it, and a consume rolls it.
	s.seedPrevious(tn, f, 3)
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
	s.noExpiry(tn, f, "a rollover")
}

// TestAnIdleTenantIsStillServedAfterADay is the lockout itself, end to end:
// the hash is left as the last request in a window that closed more than a
// day ago leaves it, and the next request must be served from it, not
// refused because the store dropped it.
func TestAnIdleTenantIsStillServedAfterADay(t *testing.T) {
	s := newStack(t)
	f, plan := uniq("if"), uniq("ip")
	s.createFeature(f)
	s.createPlan(plan, 1000, "hourly", f)
	tn := asTenant(t, s.createTenant(externalID("it"), plan, "UTC"), ent{f, 1000, "hourly", false})
	mustStatus(t, s.consume(s.runtime, f, 30, tn.External, newKey()), http.StatusOK)
	// Whatever expiry the last write left is the one the store would honour
	// a day later; reading it back and waiting it out is the idle day.
	key := s.balanceKey(tn, f)
	ctx := context.Background()
	if exp := s.rdb.PExpireTime(ctx, key).Val(); exp > 0 {
		s.rdb.PExpireAt(ctx, key, time.Now().Add(50*time.Millisecond))
		time.Sleep(100 * time.Millisecond)
	}
	r := s.consume(s.runtime, f, 1, tn.External, newKey())
	if r.Status != http.StatusOK {
		t.Fatalf("an idle tenant's next request = %d %s, want 200", r.Status, r.Body)
	}
}
