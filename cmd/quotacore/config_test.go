package main

import (
	"strings"
	"testing"
	"time"
)

func env(pairs map[string]string) Getenv {
	return func(key string) string { return pairs[key] }
}

func baseEnv() map[string]string {
	return map[string]string{
		"QUOTACORE_DATABASE_URL": "postgresql://quotacore:pw@postgres:5432/quotacore",
		"QUOTACORE_REDIS_URL":    "redis://valkey:6379/0",
	}
}

func TestLoadAppliesDocumentedDefaults(t *testing.T) {
	c, err := Load(env(baseEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", c.ListenAddr)
	}
	if c.LogLevel != "info" || c.LogFormat != "json" {
		t.Errorf("log defaults = %q/%q, want info/json", c.LogLevel, c.LogFormat)
	}
	if c.ShutdownGrace != 30*time.Second {
		t.Errorf("ShutdownGrace = %s, want 30s", c.ShutdownGrace)
	}
	if c.IdempotencyTTL != 24*time.Hour {
		t.Errorf("IdempotencyTTL = %s, want 24h", c.IdempotencyTTL)
	}
	if !c.Migrate {
		t.Error("Migrate = false, want true by default")
	}
}

func TestLoadRefusesAnUnacknowledgedBootstrapKey(t *testing.T) {
	e := baseEnv()
	e["QUOTACORE_BOOTSTRAP_ADMIN_KEY"] = "qc_admin_abc.def"
	_, err := Load(env(e))
	if err == nil {
		t.Fatal("Load accepted a bootstrap key with no acknowledgement, want a refusal")
	}
	if !strings.Contains(err.Error(), "acknowledged") {
		t.Errorf("error = %v, want it to name the acknowledgement", err)
	}
}

func TestLoadAcceptsAnAcknowledgedBootstrapKey(t *testing.T) {
	e := baseEnv()
	e["QUOTACORE_BOOTSTRAP_ADMIN_KEY"] = "qc_admin_abc.def"
	e["QUOTACORE_BOOTSTRAP_ADMIN_KEY_ACK"] = "yes"
	c, err := Load(env(e))
	if err != nil {
		t.Fatalf("Load refused an acknowledged key: %v", err)
	}
	if c.BootstrapAdminKey != "qc_admin_abc.def" || !c.BootstrapKeyIsAcked {
		t.Errorf("bootstrap key not carried through: %+v", c)
	}
}

func TestLoadRefusesAnAcknowledgementWithNoKey(t *testing.T) {
	e := baseEnv()
	e["QUOTACORE_BOOTSTRAP_ADMIN_KEY_ACK"] = "yes"
	if _, err := Load(env(e)); err != nil {
		t.Errorf("Load = %v, want the stray acknowledgement ignored rather than fatal", err)
	}
}

func TestLoadPinsTheIdempotencyWindow(t *testing.T) {
	e := baseEnv()
	e["QUOTACORE_IDEMPOTENCY_TTL"] = "1h"
	_, err := Load(env(e))
	if err == nil {
		t.Fatal("Load accepted a shortened idempotency window, want a refusal")
	}
	if !strings.Contains(err.Error(), "pinned") {
		t.Errorf("error = %v, want it to say the window is pinned", err)
	}
	e["QUOTACORE_IDEMPOTENCY_TTL"] = "24h"
	if _, err := Load(env(e)); err != nil {
		t.Errorf("Load refused the documented 24h window: %v", err)
	}
}

func TestLoadRequiresBothConnectionStrings(t *testing.T) {
	for _, key := range []string{"QUOTACORE_DATABASE_URL", "QUOTACORE_REDIS_URL"} {
		e := baseEnv()
		delete(e, key)
		if _, err := Load(env(e)); err == nil {
			t.Errorf("Load accepted a missing %s, want a refusal", key)
		}
	}
}

func TestLoadRejectsUnreadableValues(t *testing.T) {
	cases := map[string]string{
		"QUOTACORE_SHUTDOWN_GRACE":  "thirty seconds",
		"QUOTACORE_IDEMPOTENCY_TTL": "a day",
		"QUOTACORE_MIGRATE":         "sometimes",
	}
	for key, value := range cases {
		e := baseEnv()
		e[key] = value
		if _, err := Load(env(e)); err == nil {
			t.Errorf("Load accepted %s=%q, want a refusal", key, value)
		}
	}
	e := baseEnv()
	e["QUOTACORE_MIGRATE"] = "false"
	c, err := Load(env(e))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Migrate {
		t.Error("Migrate = true for QUOTACORE_MIGRATE=false")
	}
}

func TestReadinessReportsEveryUnsatisfiedTerm(t *testing.T) {
	s := newReadinessState(termDataStore, termSnapshot, termMigrations, termTenantsSeen, termDraining)
	if s.Ready() {
		t.Error("a state with no satisfied term reports ready")
	}
	s.Set(termDraining, true)
	s.Set(termMigrations, true)
	s.Set(termDataStore, true)
	s.Set(termSnapshot, true)
	if s.Ready() {
		t.Error("ready with the tenant term unsatisfied, want not ready because no tenant exists")
	}
	got := strings.Join(s.Unsatisfied(), ",")
	if got != termTenantsSeen {
		t.Errorf("Unsatisfied = %q, want %q", got, termTenantsSeen)
	}
	s.Set(termTenantsSeen, true)
	if !s.Ready() || len(s.Unsatisfied()) != 0 {
		t.Errorf("Unsatisfied = %v, want empty", s.Unsatisfied())
	}
}
