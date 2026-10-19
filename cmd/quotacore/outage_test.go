package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

// logDuration finds the access log line for a request id and returns its
// duration as the server measured it.
func (s *stack) logDuration(t *testing.T, requestID string) time.Duration {
	t.Helper()
	for _, line := range strings.Split(s.logs.String(), "\n") {
		if !strings.Contains(line, requestID) {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil || m["duration_ms"] == nil {
			continue
		}
		return time.Duration(m["duration_ms"].(float64) * float64(time.Millisecond))
	}
	t.Fatalf("no access log line for %s", requestID)
	return 0
}

// TestADeadDataStoreIs503WithinTheBound (IP-07 DoD 5, C9): with a proxy in
// front of the datastore, a store that accepts and never answers is a 503
// service_unavailable inside the 250 ms bound measured server-side, with the
// balance unchanged. A store that runs the command and loses the reply leaves
// the balance unchanged or exactly one deduction lower, and a retry with the
// same key replays rather than charges.
func TestADeadDataStoreIs503WithinTheBound(t *testing.T) {
	_, redisURL := integrationURLs(t)
	p := newProxy(t, hostOf(t, redisURL))
	s := newStack(t, func(o *stackOptions) { o.redisURL = urlVia(t, redisURL, p.addr()) })
	tn := s.tenant(externalID("dead"), "UTC", monthlyAnchor, ent{"dead.f", 100, "monthly", false})
	s.seedCurrent(tn, "dead.f", 100)
	if r := s.consume(s.runtime, "dead.f", 1, tn.External, newKey()); r.Status != 200 {
		t.Fatalf("warm-up: %d %s", r.Status, r.Body)
	}
	before := s.balance(tn, "dead.f")

	p.set("blackhole")
	start := time.Now()
	r := s.consume(s.runtime, "dead.f", 5, tn.External, newKey())
	elapsed := time.Since(start)
	if r.Status != 503 || r.code(t) != "service_unavailable" {
		t.Fatalf("a silent store = %d %s, want 503 service_unavailable", r.Status, r.Body)
	}
	if d := s.logDuration(t, r.Header.Get("X-Request-Id")); d > 260*time.Millisecond {
		t.Errorf("the server took %v to fail closed, want at most the 250 ms bound", d)
	}
	if elapsed > time.Second {
		t.Errorf("the client waited %v", elapsed)
	}
	p.set("pass")
	if b := s.balance(tn, "dead.f"); b != before {
		t.Errorf("balance after a silent store = %d, want %d", b, before)
	}

	key := newKey()
	p.set("deaf")
	r = s.consume(s.runtime, "dead.f", 7, tn.External, key)
	if r.Status != 503 || r.code(t) != "service_unavailable" {
		t.Fatalf("a lost reply = %d %s, want 503", r.Status, r.Body)
	}
	p.set("pass")
	if b := s.balance(tn, "dead.f"); b != before && b != before-7 {
		t.Errorf("after a lost reply the balance is %d, want %d or %d", b, before, before-7)
	}
	var retry response
	for i := 0; i < 10; i++ {
		if retry = s.consume(s.runtime, "dead.f", 7, tn.External, key); retry.Status == 200 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if retry.Status != 200 {
		t.Fatalf("the retry = %d %s", retry.Status, retry.Body)
	}
	if b := s.balance(tn, "dead.f"); b != before-7 {
		t.Errorf("after the retry the balance is %d, want exactly one deduction, %d", b, before-7)
	}
}

func histogramSum(t *testing.T, h interface{ Write(*dto.Metric) error }) float64 {
	t.Helper()
	var m dto.Metric
	if err := h.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleSum()
}

// TestPostgresOutageSparesCachedTenants (IP-04 DoD 1's chaos row): with
// Postgres cut, a cached tenant enforces normally, a tenant the snapshot has
// not seen is 503 control_plane_unavailable, the pool wait does not rise, and
// the uncached tenant recovers when Postgres returns.
func TestPostgresOutageSparesCachedTenants(t *testing.T) {
	baseDB, _ := integrationURLs(t)
	var p *proxy
	s := newStack(t, func(o *stackOptions) {
		p = newProxy(t, hostOf(t, baseDB))
		o.dbURL = urlVia(t, o.dbURL, p.addr())
	})
	cached := s.tenant(externalID("pgc"), "UTC", monthlyAnchor, ent{"pg.f", 100, "monthly", false})
	cold := s.tenant(externalID("pgu"), "UTC", monthlyAnchor, ent{"pg.f", 100, "monthly", false})
	s.seedCurrent(cached, "pg.f", 100)
	s.seedCurrent(cold, "pg.f", 100)
	if r := s.consume(s.runtime, "pg.f", 1, cached.External, newKey()); r.Status != 200 {
		t.Fatalf("warm-up: %d %s", r.Status, r.Body)
	}
	wait := histogramSum(t, s.app.Metrics.DBPoolWait)

	p.set("refuse")
	for i := 0; i < 20; i++ {
		if r := s.consume(s.runtime, "pg.f", 1, cached.External, newKey()); r.Status != 200 {
			t.Fatalf("a cached tenant during the outage = %d %s", r.Status, r.Body)
		}
	}
	r := s.consume(s.runtime, "pg.f", 1, cold.External, newKey())
	if r.Status != 503 || r.code(t) != "control_plane_unavailable" {
		t.Errorf("an uncached tenant during the outage = %d %s, want 503 control_plane_unavailable", r.Status, r.Body)
	}
	if got := histogramSum(t, s.app.Metrics.DBPoolWait); got != wait {
		t.Errorf("the pool wait rose from %v to %v", wait, got)
	}
	if b := s.balance(cached, "pg.f"); b != 79 {
		t.Errorf("the cached tenant's balance = %d, want 79", b)
	}

	p.set("pass")
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := s.do(http.MethodPost, "/v1/check", s.runtime, metered("pg.f", 1, cold.External))
		if r.Status == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the uncached tenant did not recover: %d %s", r.Status, r.Body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
