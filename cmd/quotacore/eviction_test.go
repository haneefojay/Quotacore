package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/quotacore/quotacore/internal/db"
	"github.com/quotacore/quotacore/internal/store"
)

// IP-15 D6, DoD 7: the service refuses a store that may evict (DR-048), and
// goes unready if the policy changes under it. Both tests reconfigure the
// store they run against, so they use a scratch store of their own rather
// than the one every other test shares: QUOTACORE_SCRATCH_REDIS_URL, which
// CI's compose override publishes on its own port.

func scratchStore(t *testing.T) (string, *redis.Client) {
	t.Helper()
	raw := os.Getenv("QUOTACORE_SCRATCH_REDIS_URL")
	if raw == "" {
		if os.Getenv("QUOTACORE_REQUIRE_INTEGRATION") != "" {
			t.Fatal("QUOTACORE_REQUIRE_INTEGRATION is set but QUOTACORE_SCRATCH_REDIS_URL is unset, so this test would skip silently")
		}
		t.Skip("QUOTACORE_SCRATCH_REDIS_URL is unset")
	}
	opt, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() {
		// Leave the scratch store as every other start expects it.
		rdb.ConfigSet(context.Background(), "maxmemory-policy", store.EvictionPolicy)
		rdb.Close()
	})
	return raw, rdb
}

func setPolicy(t *testing.T, rdb *redis.Client, policy string) {
	t.Helper()
	if err := rdb.ConfigSet(context.Background(), "maxmemory-policy", policy).Err(); err != nil {
		t.Fatal(err)
	}
}

func TestTheServiceRefusesAnEvictingStore(t *testing.T) {
	url, rdb := scratchStore(t)
	dbURL, _ := integrationURLs(t)
	setPolicy(t, rdb, "allkeys-lru")

	env := map[string]string{
		"QUOTACORE_DATABASE_URL": freshDatabaseURL(t, dbURL),
		"QUOTACORE_REDIS_URL":    url,
		"QUOTACORE_METRICS_ADDR": "127.0.0.1:0",
	}
	cfg, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	data, err := store.Open(store.Options{URL: cfg.RedisURL, DataScriptTimeout: cfg.DataScriptTimeout})
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	app, err := assemble(ctx, cfg, newLogger(&syncBuffer{}, cfg.LogFormat, cfg.LogLevel), conn, data, nil)
	if err == nil {
		app.Close()
		t.Fatal("the service assembled against an allkeys-lru store, which may discard an idempotency record inside its window (DR-048)")
	}
	if !strings.Contains(err.Error(), "allkeys-lru") || !strings.Contains(err.Error(), "DR-048") {
		t.Fatalf("the refusal does not name the policy and the rule: %v", err)
	}
}

func TestTheProberCatchesAPolicyChange(t *testing.T) {
	url, rdb := scratchStore(t)
	setPolicy(t, rdb, store.EvictionPolicy)
	s := newStack(t, func(o *stackOptions) { o.redisURL = url })

	wait := func(want bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if !slices.Contains(s.app.Ready.Unsatisfied(), termNoEviction) == want {
				return
			}
		}
		t.Fatalf("the eviction term is not %v after five seconds; unsatisfied: %v", want, s.app.Ready.Unsatisfied())
	}
	wait(true)
	if r := s.do("GET", "/readyz", "", ""); r.Status != 200 {
		t.Fatalf("/readyz against a noeviction store = %d, want 200", r.Status)
	}

	setPolicy(t, rdb, "volatile-lru")
	wait(false)
	if r := s.do("GET", "/readyz", "", ""); r.Status != 503 {
		t.Fatalf("/readyz after the policy changed to volatile-lru = %d, want 503", r.Status)
	}
	if !strings.Contains(s.logs.String(), "volatile-lru") {
		t.Fatal("the policy change was not logged with the policy it changed to")
	}

	setPolicy(t, rdb, store.EvictionPolicy)
	wait(true)
}
