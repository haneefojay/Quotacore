package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/db"
)

// IP-13 DoD 5 (NFR-O4, D7): the two probes depend on what observability.md
// section 4 says, each proved on the shipped assembly's own readiness.

func (s *stack) probe(path string) response {
	s.t.Helper()
	return s.do(http.MethodGet, path, "", "")
}

func (s *stack) live(t *testing.T, when string) {
	t.Helper()
	if r := s.probe("/healthz"); r.Status != http.StatusOK || string(r.Body) != `{"status":"ok"}` {
		t.Errorf("%s: /healthz = %d %s, want 200 {\"status\":\"ok\"}", when, r.Status, r.Body)
	}
}

func (s *stack) readyWithin(t *testing.T, want int, within time.Duration, when string) {
	t.Helper()
	waitFor(t, within, when+": /readyz "+http.StatusText(want), func() bool { return s.probe("/readyz").Status == want })
	r := s.probe("/readyz")
	switch {
	case want == http.StatusOK && string(r.Body) != `{"status":"ready"}`:
		t.Errorf("%s: a ready /readyz has body %s", when, r.Body)
	case want == http.StatusServiceUnavailable && len(r.Body) != 0:
		t.Errorf("%s: an unready /readyz has body %s, want none", when, r.Body)
	}
}

// TestAFreshInstallIsReady: an install with no tenant is ready, because the
// snapshot's first load needs no tenant (finding 2 of the IP-13 plan).
func TestAFreshInstallIsReady(t *testing.T) {
	s := newStack(t)
	if n := s.rows(`SELECT count(*) FROM tenants`); n != 0 {
		t.Fatalf("the fresh install has %d tenants", n)
	}
	s.readyWithin(t, http.StatusOK, 2*time.Second, "a fresh install")
	s.live(t, "a fresh install")
}

// TestReadinessFollowsTheDataStore: with the store cut, /readyz is 503
// within two probe intervals and /healthz is still 200; with the store
// back, /readyz is 200 again within two.
func TestReadinessFollowsTheDataStore(t *testing.T) {
	_, redisURL := integrationURLs(t)
	p := newProxy(t, hostOf(t, redisURL))
	s := newStack(t, func(o *stackOptions) { o.redisURL = urlVia(t, redisURL, p.addr()) })
	s.readyWithin(t, http.StatusOK, 2*time.Second, "before the cut")
	// Two probes of 100 ms, each allowed QUOTACORE_DATASCRIPT_TIMEOUT.
	twoProbes := 2 * (100*time.Millisecond + 250*time.Millisecond)
	p.set("blackhole")
	s.readyWithin(t, http.StatusServiceUnavailable, twoProbes+100*time.Millisecond, "a silent store")
	s.live(t, "a silent store")
	p.set("refuse")
	s.readyWithin(t, http.StatusServiceUnavailable, twoProbes, "a refusing store")
	p.set("pass")
	s.readyWithin(t, http.StatusOK, twoProbes+100*time.Millisecond, "the store back")
}

// TestReadinessIgnoresAPostgresOutage: Postgres is not a readiness term,
// because cached tenants enforce through its outage (NFR-A2).
func TestReadinessIgnoresAPostgresOutage(t *testing.T) {
	baseDB, _ := integrationURLs(t)
	var p *proxy
	s := newStack(t, func(o *stackOptions) {
		p = newProxy(t, hostOf(t, baseDB))
		o.dbURL = urlVia(t, o.dbURL, p.addr())
	})
	f, _, tn := s.provisioned(100)
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
	s.readyWithin(t, http.StatusOK, 2*time.Second, "before the outage")
	p.set("refuse")
	waitFor(t, 5*time.Second, "controlplane_up = 0", func() bool { return s.counter("quotacore_controlplane_up") == 0 })
	for i := 0; i < 5; i++ {
		if r := s.probe("/readyz"); r.Status != http.StatusOK {
			t.Fatalf("/readyz during a Postgres outage = %d, want 200", r.Status)
		}
		time.Sleep(100 * time.Millisecond)
	}
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
	s.live(t, "a Postgres outage")
	p.set("pass")
}

// TestReadinessRefusesReadOnlyAndTheFence: an instance started read-only,
// and one whose fence a rebuild holds, is not ready (IP-11 D5, D6).
func TestReadinessRefusesReadOnlyAndTheFence(t *testing.T) {
	ro := newStack(t, func(o *stackOptions) { o.env["QUOTACORE_STARTUP_MODE"] = "readonly" })
	ro.readyWithin(t, http.StatusServiceUnavailable, time.Second, "started read-only")
	ro.live(t, "started read-only")

	s := newStack(t)
	s.readyWithin(t, http.StatusOK, 2*time.Second, "normal mode")
	conn, err := db.Open(context.Background(), s.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The rebuild waits for the instance's shared hold to go, so the fence
	// connection is terminated first, as IP-11's tests take it.
	if _, err := conn.Exec(`SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory'
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database()) AND pid <> pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	release, ok, err := db.ExclusiveFence(context.Background(), conn)
	if err != nil || !ok {
		t.Fatalf("the rebuild could not take the fence: %v %v", ok, err)
	}
	s.readyWithin(t, http.StatusServiceUnavailable, 2*time.Second, "a rebuild holding the fence")
	s.live(t, "a rebuild holding the fence")
	release()
	s.readyWithin(t, http.StatusOK, 2*time.Second, "the fence released")
}

// TestReadinessDuringADrain: once the draining term is cleared, as main
// clears it on SIGTERM, /readyz is 503 and /healthz is still 200, so an
// orchestrator stops sending traffic without killing the drain.
func TestReadinessDuringADrain(t *testing.T) {
	s := newStack(t)
	s.readyWithin(t, http.StatusOK, 2*time.Second, "before the drain")
	s.app.Ready.Set(termDraining, false)
	s.readyWithin(t, http.StatusServiceUnavailable, 100*time.Millisecond, "draining")
	s.live(t, "draining")
}

// TestLivenessIgnoresEveryDependency: /healthz is 200 with its body with
// the store cut and Postgres cut at once.
func TestLivenessIgnoresEveryDependency(t *testing.T) {
	baseDB, redisURL := integrationURLs(t)
	rp := newProxy(t, hostOf(t, redisURL))
	var pp *proxy
	s := newStack(t, func(o *stackOptions) {
		o.redisURL = urlVia(t, redisURL, rp.addr())
		pp = newProxy(t, hostOf(t, baseDB))
		o.dbURL = urlVia(t, o.dbURL, pp.addr())
	})
	rp.set("refuse")
	pp.set("refuse")
	s.readyWithin(t, http.StatusServiceUnavailable, time.Second, "both stores cut")
	s.live(t, "both stores cut")
}
