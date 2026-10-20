package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/db"
	"github.com/quotacore/quotacore/internal/store"
)

// readOnlyPeer starts a second instance on s's database in read-only mode.
// It shares s's keys: its own were minted into a database nothing uses.
func (s *stack) readOnlyPeer() *stack {
	s.t.Helper()
	r := newStack(s.t, func(o *stackOptions) {
		o.dbURL = s.dbURL
		o.env["QUOTACORE_STARTUP_MODE"] = "readonly"
	})
	r.runtime, r.admin = s.runtime, s.admin
	return r
}

// tenantKeys is every key of the tenants, with its value and its expiry: what
// a read-only fleet must not change.
func (s *stack) tenantKeys(ids ...string) map[string]string {
	s.t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	for _, id := range ids {
		iter := s.rdb.Scan(ctx, 0, "qc:{t:"+id+"}:*", 1000).Iterator()
		for iter.Next(ctx) {
			k := iter.Val()
			var v string
			switch typ := s.rdb.Type(ctx, k).Val(); typ {
			case "hash":
				v = mustJSON(s.rdb.HGetAll(ctx, k).Val())
			default:
				v = typ + ":" + s.rdb.Get(ctx, k).Val()
			}
			out[k] = fmt.Sprintf("%s expires=%d", v, s.rdb.PExpireTime(ctx, k).Val())
		}
		if err := iter.Err(); err != nil {
			s.t.Fatal(err)
		}
	}
	return out
}

// writes is one call of every route that can write a tenant's key, which a
// read-only instance must answer 503 service_unavailable (IP-11 D5).
func writes(f, plan, other string, tn tenant, version int64) map[string]func(s *stack) response {
	tp := "/v1/admin/tenants/" + tn.ID
	return map[string]func(s *stack) response{
		"consume": func(s *stack) response { return s.consume(s.runtime, f, 1, tn.External, newKey()) },
		"refund": func(s *stack) response {
			return s.do(http.MethodPost, "/v1/refund", s.runtime, metered(f, 1, tn.External), "Idempotency-Key", newKey())
		},
		"check": func(s *stack) response { return s.check(f, tn.External) },
		"tenant create": func(s *stack) response {
			return s.adminDo(http.MethodPost, "/v1/admin/tenants",
				`{"external_id":"`+externalID("ro")+`","display_name":"T","plan_key":"`+plan+`","timezone":"UTC"}`)
		},
		"plan change": func(s *stack) response {
			return s.adminDo(http.MethodPatch, tp, fmt.Sprintf(`{"version":%d,"plan_key":%q}`, version, other))
		},
		"apply-now": func(s *stack) response {
			return s.adminDo(http.MethodPost, "/v1/admin/plans/"+plan+"/apply", `{"reason":"read-only"}`)
		},
		"grant": func(s *stack) response {
			return s.adminDo(http.MethodPost, tp+"/grant", `{"feature_key":"`+f+`","amount":5,"reason":"r"}`)
		},
		"set": func(s *stack) response {
			return s.adminDo(http.MethodPost, tp+"/set", `{"feature_key":"`+f+`","value":3,"reason":"r"}`)
		},
		"force-rollover": func(s *stack) response {
			return s.adminDo(http.MethodPost, tp+"/force-rollover", `{"feature_key":"`+f+`","reason":"r"}`)
		},
		"override set": func(s *stack) response {
			return s.adminDo(http.MethodPost, tp+"/overrides",
				`{"feature_key":"`+f+`","limit_value":20,"interval":null,"apply_at":"immediate","reason":"r"}`)
		},
		"override clear": func(s *stack) response {
			return s.adminDo(http.MethodDelete, tp+"/overrides/"+f, `{"apply_at":"immediate","reason":"r"}`)
		},
	}
}

// TestReadOnlyRefusesEveryTenantWrite is the first half of IP-11 DoD 3: an
// instance started read-only answers every route that can write a tenant's
// key with 503 service_unavailable and changes no key, says so in its status
// and readiness, runs no drain, pass or retention, and still reads.
func TestReadOnlyRefusesEveryTenantWrite(t *testing.T) {
	s := newStack(t)
	f, plan, tn := s.provisioned(100)
	other := uniq("po")
	s.createPlan(other, 50, "monthly", f)
	mustStatus(t, s.consume(s.runtime, f, 10, tn.External, newKey()), http.StatusOK)
	s.flush()
	version := s.version("/v1/admin/tenants/" + tn.ID)
	r := s.readOnlyPeer()
	before := s.tenantKeys(tn.ID)
	tenants := s.rows(`SELECT count(*) FROM tenants`)
	for name, call := range writes(f, plan, other, tn, version) {
		got := call(r)
		if got.Status != http.StatusServiceUnavailable || got.code(t) != "service_unavailable" {
			t.Errorf("%s on a read-only instance answered %d %s", name, got.Status, got.Body)
		}
	}
	if after := s.tenantKeys(tn.ID); mustJSON(after) != mustJSON(before) {
		t.Errorf("a read-only instance changed the tenant's keys:\nbefore %v\nafter  %v", before, after)
	}
	if n := s.rows(`SELECT count(*) FROM tenants`); n != tenants {
		t.Errorf("a read-only instance created a tenant: %d tenants, was %d", n, tenants)
	}
	// Reads still answer, and the instance says what it is.
	mustStatus(t, r.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External+"&feature_key="+f, s.runtime, ""), http.StatusOK)
	if m := mustStatus(t, r.adminDo(http.MethodGet, "/v1/admin/status", ""), http.StatusOK); str(m, "mode") != "readonly" {
		t.Errorf("status mode %q, want readonly", str(m, "mode"))
	}
	if m := mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/status", ""), http.StatusOK); str(m, "mode") != "normal" {
		t.Errorf("a normal instance's status mode %q", str(m, "mode"))
	}
	if got := r.do(http.MethodGet, "/readyz", "", ""); got.Status == http.StatusOK {
		t.Errorf("/readyz on a read-only instance answered %d", got.Status)
	}
	// Nothing in the background runs.
	if ran, err := r.app.Pass.Once(context.Background()); err != nil || ran.Ran {
		t.Errorf("the pass ran on a read-only instance: %+v, %v", ran, err)
	}
	stale := time.Now().Add(-48 * time.Hour)
	if _, err := s.seedDB.Exec(`UPDATE usage_events SET created_at = $1`, stale); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	db.RunRetention(ctx, r.sql, db.RetentionOptions{EventDays: 1, Interval: 10 * time.Millisecond,
		FirstRun: time.Millisecond, Paused: r.app.Data.ReadOnly})
	if n := s.rows(`SELECT count(*) FROM usage_events`); n == 0 {
		t.Error("retention pruned the ledger from a read-only instance")
	}
}

// TestTheRebuildRunsReadOnlyThroughout is the rest of IP-11 DoD 3: the fence
// makes "the fleet is read-only for the whole rebuild" something a test can
// break. A rebuild that writes cannot start while a normal instance holds the
// fence, a normal instance cannot start while a rebuild holds it, and an
// instance whose fence connection drops stops writing before the rebuild
// writes, and writes again only once it holds the fence again.
func TestTheRebuildRunsReadOnlyThroughout(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	c, _ := s.commands()
	if _, _, err := c.rebuild(context.Background(), rebuildOptions{apply: true, reason: "drill"}); err == nil ||
		!strings.Contains(err.Error(), "holds the fence") {
		t.Fatalf("a rebuild beside a normal instance: %v", err)
	}

	// Drop the instance's fence connection, and take the fence before it
	// can take it back, as a rebuild starting at that instant would.
	var release func()
	deadline := time.Now().Add(5 * time.Second)
	for release == nil {
		if time.Now().After(deadline) {
			t.Fatal("never took the fence from the instance")
		}
		if pid := s.app.Fence.PID(); pid != 0 {
			if _, err := s.seedDB.Exec(`SELECT pg_terminate_backend($1)`, pid); err != nil {
				t.Fatal(err)
			}
		}
		rel, ok, err := db.ExclusiveFence(context.Background(), s.seedDB)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			release = rel
		}
	}
	took := time.Now()
	for !s.app.Data.ReadOnly() {
		if time.Since(took) > c.grace {
			t.Fatalf("the instance still writes %s after losing its fence, longer than the rebuild's grace %s",
				time.Since(took), c.grace)
		}
		time.Sleep(5 * time.Millisecond)
	}
	before := s.tenantKeys(tn.ID)
	if got := s.consume(s.runtime, f, 1, tn.External, newKey()); got.Status != http.StatusServiceUnavailable {
		t.Errorf("a consume on an instance without its fence answered %d %s", got.Status, got.Body)
	}
	time.Sleep(300 * time.Millisecond)
	if !s.app.Data.ReadOnly() {
		t.Error("the instance went back to writing while the rebuild held the fence")
	}
	if after := s.tenantKeys(tn.ID); mustJSON(after) != mustJSON(before) {
		t.Errorf("the tenant's keys changed while the rebuild held the fence")
	}
	if m := mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/status", ""), http.StatusOK); str(m, "mode") != "readonly" {
		t.Errorf("status mode %q without the fence", str(m, "mode"))
	}

	// A normal start is refused while the rebuild holds the fence.
	if err := startNormal(t, s.dbURL); !errors.Is(err, db.ErrFenced) {
		t.Errorf("a normal start beside a rebuild: %v, want ErrFenced", err)
	}
	// A read-only start is not.
	r := s.readOnlyPeer()
	if !r.app.Data.ReadOnly() {
		t.Error("a read-only peer writes")
	}

	release()
	for s.app.Data.ReadOnly() {
		if time.Since(took) > 5*time.Second {
			t.Fatal("the instance never took its fence back")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
}

// startNormal assembles a normal instance on a database and reports the
// error it refused to start with, if any.
func startNormal(t *testing.T, dbURL string) error {
	t.Helper()
	_, redisURL := integrationURLs(t)
	env := map[string]string{"QUOTACORE_DATABASE_URL": dbURL, "QUOTACORE_REDIS_URL": redisURL}
	cfg, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := db.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func(c *sql.DB) { c.Close() }(conn)
	data, err := store.Open(store.Options{URL: redisURL, DataScriptTimeout: cfg.DataScriptTimeout})
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	app, err := assemble(ctx, cfg, slog.New(slog.DiscardHandler), conn, data, func() bool { return true }, nil,
		assembleOptions{retentionFirstRun: time.Hour, reconcileFirstRun: time.Hour})
	if err == nil {
		app.Close()
	}
	return err
}
