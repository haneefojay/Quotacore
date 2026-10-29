package main

import (
	"strings"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/script"
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

// TestAPrivilegedPortIsRefused (IP-13 DoD 8, D13): neither listener may
// take a port below 1024, which would need a capability the service does not
// run with; the metrics listener defaults to loopback.
func TestAPrivilegedPortIsRefused(t *testing.T) {
	for _, key := range []string{"QUOTACORE_LISTEN_ADDR", "QUOTACORE_METRICS_ADDR"} {
		for addr, ok := range map[string]bool{":80": false, "127.0.0.1:443": false, ":1023": false, ":1024": true, "127.0.0.1:0": true, "nonsense": false} {
			e := baseEnv()
			e[key] = addr
			_, err := Load(env(e))
			if ok != (err == nil) {
				t.Errorf("%s=%s: Load error = %v, want accepted=%v", key, addr, err, ok)
			}
		}
	}
	c, err := Load(env(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetricsAddr != "127.0.0.1:9090" || !loopback(c.MetricsAddr) || loopback(c.ListenAddr) {
		t.Errorf("MetricsAddr = %q, ListenAddr = %q", c.MetricsAddr, c.ListenAddr)
	}
}

func TestLoadAppliesSnapshotDefaults(t *testing.T) {
	c, err := Load(env(baseEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ConfigCacheEntries != 50000 || c.ConfigCacheMB != 64 {
		t.Errorf("config cache defaults = %d entries / %d MB, want 50000 / 64", c.ConfigCacheEntries, c.ConfigCacheMB)
	}
	if c.ConfigRefreshInterval != 30*time.Second || c.ConfigRefreshMissRate != 20 {
		t.Errorf("refresh defaults = %s at %.0f/s, want 30s at 20/s", c.ConfigRefreshInterval, c.ConfigRefreshMissRate)
	}
	if c.DataScriptTimeout != 250*time.Millisecond {
		t.Errorf("DataScriptTimeout = %s, want 250ms", c.DataScriptTimeout)
	}
	if c.ControlPlaneTimeout != 3*time.Second {
		t.Errorf("ControlPlaneTimeout = %s, want 3s", c.ControlPlaneTimeout)
	}
}

func TestLoadAcceptsOverridesForIP04Keys(t *testing.T) {
	e := baseEnv()
	e["QUOTACORE_CONFIG_CACHE_ENTRIES"] = "1234"
	e["QUOTACORE_CONFIG_CACHE_MB"] = "8"
	e["QUOTACORE_CONFIG_REFRESH_INTERVAL"] = "5s"
	e["QUOTACORE_CONFIG_REFRESH_MISS_RATE_LIMIT"] = "2.5"
	e["QUOTACORE_DATASCRIPT_TIMEOUT"] = "100ms"
	e["QUOTACORE_CONTROLPLANE_TIMEOUT"] = "7s"
	c, err := Load(env(e))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ConfigCacheEntries != 1234 || c.ConfigCacheMB != 8 ||
		c.ConfigRefreshInterval != 5*time.Second || c.ConfigRefreshMissRate != 2.5 ||
		c.DataScriptTimeout != 100*time.Millisecond || c.ControlPlaneTimeout != 7*time.Second {
		t.Errorf("override values not carried through: %+v", c)
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

// wellFormedAdminKey is an admin key in ADR-0018's format, minted for the
// tests and never valid anywhere else.
const wellFormedAdminKey = "qc_admin_0123456789abcdef.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestLoadAcceptsAnAcknowledgedBootstrapKey(t *testing.T) {
	e := baseEnv()
	e["QUOTACORE_BOOTSTRAP_ADMIN_KEY"] = wellFormedAdminKey
	e["QUOTACORE_BOOTSTRAP_ADMIN_KEY_ACK"] = "yes"
	c, err := Load(env(e))
	if err != nil {
		t.Fatalf("Load refused an acknowledged key: %v", err)
	}
	if c.BootstrapAdminKey != wellFormedAdminKey || !c.BootstrapKeyIsAcked {
		t.Errorf("bootstrap key not carried through: %+v", c)
	}
}

// TestAMalformedBootstrapKeyRefusesToStart is part of IP-09 DoD 7: a key that
// is not an admin key in the token format refuses start, acknowledged or not,
// and the refusal never repeats the value.
func TestAMalformedBootstrapKeyRefusesToStart(t *testing.T) {
	for _, bad := range []string{
		"qc_admin_abc.def",
		"qc_runtime_0123456789abcdef.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"correct horse battery staple",
	} {
		e := baseEnv()
		e["QUOTACORE_BOOTSTRAP_ADMIN_KEY"] = bad
		e["QUOTACORE_BOOTSTRAP_ADMIN_KEY_ACK"] = "yes"
		_, err := Load(env(e))
		if err == nil {
			t.Errorf("Load accepted a malformed bootstrap key")
			continue
		}
		if strings.Contains(err.Error(), bad) {
			t.Errorf("the refusal repeats the key: %v", err)
		}
	}
}

func TestLoadReadsTheAdminLimitAndTrustedProxies(t *testing.T) {
	c, err := Load(env(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.RateLimitAdminRPM != 60 || len(c.TrustedProxies) != 0 {
		t.Errorf("defaults = %v rpm, %v proxies; want 60 and none", c.RateLimitAdminRPM, c.TrustedProxies)
	}
	e := baseEnv()
	e["QUOTACORE_RATELIMIT_ADMIN_RPM"] = "120"
	e["QUOTACORE_TRUSTED_PROXIES"] = "10.0.0.0/8, 192.168.1.7"
	if c, err = Load(env(e)); err != nil {
		t.Fatal(err)
	}
	if c.RateLimitAdminRPM != 120 || len(c.TrustedProxies) != 2 {
		t.Errorf("got %v rpm and %v; want 120 and two proxies", c.RateLimitAdminRPM, c.TrustedProxies)
	}
	e["QUOTACORE_TRUSTED_PROXIES"] = "not-a-network"
	if _, err := Load(env(e)); err == nil {
		t.Error("Load accepted a trusted proxy that is not an address or a network")
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

// The window is pinned in three places and none of them reads the others: this
// package refuses a deployment that disagrees, internal/script stamps the TTL on
// every record, and the Lua compares a caller's replay against the same span. If
// the first two ever drifted, a deployment would start clean and then write
// records with a different lifetime than the one it validated, so the equality is
// asserted rather than assumed (DR-029).
func TestThePinnedWindowMatchesTheScriptPackage(t *testing.T) {
	if IdempotencyWindow != script.IdempotencyWindow {
		t.Errorf("cmd/quotacore pins %s, internal/script records for %s", IdempotencyWindow, script.IdempotencyWindow)
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
		"QUOTACORE_SHUTDOWN_GRACE":                 "thirty seconds",
		"QUOTACORE_IDEMPOTENCY_TTL":                "a day",
		"QUOTACORE_MIGRATE":                        "sometimes",
		"QUOTACORE_CONFIG_CACHE_ENTRIES":           "lots",
		"QUOTACORE_CONFIG_CACHE_MB":                "0",
		"QUOTACORE_CONFIG_REFRESH_INTERVAL":        "every minute",
		"QUOTACORE_CONFIG_REFRESH_MISS_RATE_LIMIT": "fast",
		"QUOTACORE_DATASCRIPT_TIMEOUT":             "soon",
		"QUOTACORE_CONTROLPLANE_TIMEOUT":           "now",
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

// TestLoadRefusesValuesThatAreNotPositive (found by the v0.1 code review): a
// zero miss rate was read as the default 20/s, and a zero or negative timeout
// was a deadline that had already passed.
func TestLoadRefusesValuesThatAreNotPositive(t *testing.T) {
	for _, c := range [][2]string{
		{"QUOTACORE_CONFIG_REFRESH_MISS_RATE_LIMIT", "0"},
		{"QUOTACORE_CONFIG_REFRESH_MISS_RATE_LIMIT", "-1"},
		{"QUOTACORE_CONFIG_REFRESH_INTERVAL", "0s"},
		{"QUOTACORE_DATASCRIPT_TIMEOUT", "0s"},
		{"QUOTACORE_DATASCRIPT_TIMEOUT", "-250ms"},
		{"QUOTACORE_CONTROLPLANE_TIMEOUT", "0s"},
		{"QUOTACORE_SHUTDOWN_GRACE", "-1s"},
		{"QUOTACORE_REQUEST_TIMEOUT", "0s"},
	} {
		e := baseEnv()
		e[c[0]] = c[1]
		if _, err := Load(env(e)); err == nil {
			t.Errorf("Load accepted %s=%q, want a refusal", c[0], c[1])
		}
	}
}

func TestReadinessReportsEveryUnsatisfiedTerm(t *testing.T) {
	s := newReadinessState(termDataStore, termMigrations, termDraining)
	normal := false
	s.Watch(termNormal, func() bool { return normal })
	if s.Ready() {
		t.Error("a state with no satisfied term reports ready")
	}
	s.Set(termDraining, true)
	s.Set(termMigrations, true)
	s.Set(termDataStore, true)
	if got := strings.Join(s.Unsatisfied(), ","); got != termNormal {
		t.Errorf("Unsatisfied = %q, want %q", got, termNormal)
	}
	normal = true
	if !s.Ready() || len(s.Unsatisfied()) != 0 {
		t.Errorf("Unsatisfied = %v, want empty", s.Unsatisfied())
	}
}

func TestLoadAppliesTheEnforcementPathDefaults(t *testing.T) {
	c, err := Load(env(baseEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.RequestTimeout != 2*time.Second || c.MaxBodyBytes != 65536 || c.MetadataMaxBytes != 4096 ||
		c.KeyCacheEntries != 10000 || !c.RateLimitEnabled || c.RateLimitRuntimeRPS != 2000 ||
		c.RateLimitBurst != 4000 || c.RateLimiterMaxKeys != 100000 {
		t.Errorf("IP-07 defaults differ from deployment.md section 4: %+v", c)
	}
}

func TestLoadAcceptsOverridesForIP07Keys(t *testing.T) {
	e := baseEnv()
	e["QUOTACORE_REQUEST_TIMEOUT"] = "500ms"
	e["QUOTACORE_MAX_BODY_BYTES"] = "8192"
	e["QUOTACORE_METADATA_MAX_BYTES"] = "1024"
	e["QUOTACORE_KEY_CACHE_ENTRIES"] = "50"
	e["QUOTACORE_RATELIMIT_ENABLED"] = "false"
	e["QUOTACORE_RATELIMIT_RUNTIME_RPS"] = "10"
	e["QUOTACORE_RATELIMIT_RUNTIME_BURST"] = "20"
	e["QUOTACORE_RATE_LIMITER_MAX_KEYS"] = "7"
	c, err := Load(env(e))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.RequestTimeout != 500*time.Millisecond || c.MaxBodyBytes != 8192 || c.MetadataMaxBytes != 1024 ||
		c.KeyCacheEntries != 50 || c.RateLimitEnabled || c.RateLimitRuntimeRPS != 10 ||
		c.RateLimitBurst != 20 || c.RateLimiterMaxKeys != 7 {
		t.Errorf("override values not carried through: %+v", c)
	}
	for key, bad := range map[string]string{
		"QUOTACORE_REQUEST_TIMEOUT":       "soon",
		"QUOTACORE_MAX_BODY_BYTES":        "0",
		"QUOTACORE_KEY_CACHE_ENTRIES":     "-1",
		"QUOTACORE_RATELIMIT_RUNTIME_RPS": "fast",
		"QUOTACORE_RATELIMIT_ENABLED":     "maybe",
		"QUOTACORE_METADATA_MAX_BYTES":    "999999",
	} {
		e := baseEnv()
		e[key] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "QUOTACORE_") {
			t.Errorf("%s=%q: err = %v, want a refusal naming the key", key, bad, err)
		}
	}
}

// TestLoadReadsThePlanLifecycleKeys: IP-10's two keys default to 50 tenants
// and 15 minutes, carry an override through, and refuse a value that would
// disable the confirmation or outlive the drain's bound; the refresh interval
// is refused at an hour, the shortest first window the drain's stale-anchor
// argument relies on (D2).
func TestLoadReadsThePlanLifecycleKeys(t *testing.T) {
	c, err := Load(env(baseEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PlanApplyConfirmThreshold != 50 || c.ConfirmationTTL != 15*time.Minute {
		t.Errorf("defaults: threshold %d, TTL %v", c.PlanApplyConfirmThreshold, c.ConfirmationTTL)
	}
	e := baseEnv()
	e["QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD"] = "7"
	e["QUOTACORE_CONFIRMATION_TTL"] = "2m"
	if c, err = Load(env(e)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PlanApplyConfirmThreshold != 7 || c.ConfirmationTTL != 2*time.Minute {
		t.Errorf("overrides not carried through: threshold %d, TTL %v", c.PlanApplyConfirmThreshold, c.ConfirmationTTL)
	}
	for key, bad := range map[string]string{
		"QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD": "0",
		"QUOTACORE_CONFIRMATION_TTL":             "500ms",
		"QUOTACORE_CONFIG_REFRESH_INTERVAL":      "1h",
	} {
		e := baseEnv()
		e[key] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s=%q: err = %v, want a refusal naming the key", key, bad, err)
		}
	}
}

// TestLoadReadsTheLedgerKeys is S8: the six deployment.md section 4.4 keys,
// their defaults, and a refusal naming the key for a value the ledger cannot
// run with.
func TestLoadReadsTheLedgerKeys(t *testing.T) {
	c, err := Load(env(baseEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.EventQueueSize != 10000 || c.EventBatchSize != 200 || c.EventFlushInterval != 100*time.Millisecond ||
		c.EventRetentionDays != 90 || c.AuditRetentionDays != 400 || c.RetentionInterval != time.Hour {
		t.Errorf("defaults: %d %d %v %d %d %v", c.EventQueueSize, c.EventBatchSize, c.EventFlushInterval,
			c.EventRetentionDays, c.AuditRetentionDays, c.RetentionInterval)
	}
	e := baseEnv()
	e["QUOTACORE_EVENT_QUEUE_SIZE"] = "64"
	e["QUOTACORE_EVENT_BATCH_SIZE"] = "64"
	e["QUOTACORE_EVENT_FLUSH_INTERVAL"] = "5ms"
	e["QUOTACORE_EVENT_RETENTION_DAYS"] = "0"
	e["QUOTACORE_AUDIT_RETENTION_DAYS"] = "30"
	e["QUOTACORE_RETENTION_INTERVAL"] = "10m"
	if c, err = Load(env(e)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.EventQueueSize != 64 || c.EventBatchSize != 64 || c.EventFlushInterval != 5*time.Millisecond ||
		c.EventRetentionDays != 0 || c.AuditRetentionDays != 30 || c.RetentionInterval != 10*time.Minute {
		t.Errorf("overrides not carried through: %+v", c)
	}
	for key, bad := range map[string]string{
		"QUOTACORE_EVENT_QUEUE_SIZE":     "0",
		"QUOTACORE_EVENT_BATCH_SIZE":     "20000",
		"QUOTACORE_EVENT_FLUSH_INTERVAL": "30s",
		"QUOTACORE_EVENT_RETENTION_DAYS": "-1",
		"QUOTACORE_AUDIT_RETENTION_DAYS": "a year",
		"QUOTACORE_RETENTION_INTERVAL":   "30s",
	} {
		e := baseEnv()
		e[key] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s=%q: err = %v, want a refusal naming the key", key, bad, err)
		}
	}
}

// TestLoadReadsTheReconciliationKeys: the startup mode is normal unless it
// is readonly, anything else refuses to start, and the pass's interval is
// five minutes and never under one (IP-11 D2, D5).
func TestLoadReadsTheReconciliationKeys(t *testing.T) {
	c, err := Load(env(baseEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.StartupMode != ModeNormal || c.ReconcileInterval != 5*time.Minute || c.Settle() != time.Minute {
		t.Errorf("defaults: %q %v settle %v", c.StartupMode, c.ReconcileInterval, c.Settle())
	}
	e := baseEnv()
	e["QUOTACORE_STARTUP_MODE"] = "readonly"
	e["QUOTACORE_RECONCILE_INTERVAL"] = "90s"
	e["QUOTACORE_EVENT_FLUSH_INTERVAL"] = "10s"
	if c, err = Load(env(e)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.StartupMode != ModeReadOnly || c.ReconcileInterval != 90*time.Second || c.Settle() != 100*time.Second {
		t.Errorf("overrides: %q %v settle %v", c.StartupMode, c.ReconcileInterval, c.Settle())
	}
	for key, value := range map[string]string{
		"QUOTACORE_STARTUP_MODE":       "maintenance",
		"QUOTACORE_RECONCILE_INTERVAL": "30s",
	} {
		e := baseEnv()
		e[key] = value
		if _, err := Load(env(e)); err == nil {
			t.Errorf("Load accepted %s=%q, want a refusal", key, value)
		}
	}
	e = baseEnv()
	e["QUOTACORE_RECONCILE_INTERVAL"] = "often"
	if _, err := Load(env(e)); err == nil {
		t.Error("Load accepted an unreadable QUOTACORE_RECONCILE_INTERVAL")
	}
}

// TestTheRebuildFlagsAreRead: a write needs a reason, the operator's
// references name a tenant and a feature, and a set names a whole number.
func TestTheRebuildFlagsAreRead(t *testing.T) {
	o, err := parseRebuild([]string{"--apply", "--reason", "drill", "--accept", "acme:tokens", "--set", "beta:seats=4",
		"--ledger-restored-at", "2026-10-20T10:00:00Z", "--report", "out.json"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.apply || o.reason != "drill" || o.acceptRefs[0] != "acme:tokens" || o.setRefs["beta:seats"] != 4 ||
		o.restoredAt.IsZero() || o.reportFile != "out.json" {
		t.Errorf("parsed %+v", o)
	}
	if o, err := parseRebuild(nil); err != nil || o.apply {
		t.Errorf("no flags is a dry run: %+v, %v", o, err)
	}
	for _, args := range [][]string{
		{"--apply"},
		{"--accept", "acme:tokens"},
		{"--set", "acme:tokens=4"},
		{"--reason", "r", "--accept", "acme"},
		{"--reason", "r", "--set", "acme:tokens=-1"},
		{"--reason", "r", "--set", "acme:tokens"},
		{"--ledger-restored-at", "yesterday"},
		{"extra"},
	} {
		if _, err := parseRebuild(args); err == nil {
			t.Errorf("parseRebuild accepted %v", args)
		}
	}
}

// TestLoadReadsTheWorkerKeys: the reset worker's three keys of deployment.md
// section 4.3, on by default at 60 s and 500 (Q-13), and a refusal naming
// the key for a value it cannot run with.
func TestLoadReadsTheWorkerKeys(t *testing.T) {
	c, err := Load(env(baseEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.WorkerEnabled || c.ResetScanInterval != time.Minute || c.ResetBatchSize != 500 {
		t.Errorf("defaults: %v %v %d", c.WorkerEnabled, c.ResetScanInterval, c.ResetBatchSize)
	}
	e := baseEnv()
	e["QUOTACORE_WORKER_ENABLED"] = "false"
	e["QUOTACORE_RESET_SCAN_INTERVAL"] = "5s"
	e["QUOTACORE_RESET_BATCH_SIZE"] = "50"
	if c, err = Load(env(e)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.WorkerEnabled || c.ResetScanInterval != 5*time.Second || c.ResetBatchSize != 50 {
		t.Errorf("overrides: %v %v %d", c.WorkerEnabled, c.ResetScanInterval, c.ResetBatchSize)
	}
	for key, bad := range map[string]string{
		"QUOTACORE_WORKER_ENABLED":      "sometimes",
		"QUOTACORE_RESET_SCAN_INTERVAL": "500ms",
		"QUOTACORE_RESET_BATCH_SIZE":    "0",
	} {
		e := baseEnv()
		e[key] = bad
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s=%q: err = %v, want a refusal naming the key", key, bad, err)
		}
	}
}
