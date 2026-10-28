package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/quotacore/quotacore/internal/admin"
	"github.com/quotacore/quotacore/internal/auth"
)

const IdempotencyWindow = 24 * time.Hour

type Config struct {
	ListenAddr string
	// MetricsAddr is where /metrics is served, on its own listener (IP-13
	// D8).
	MetricsAddr           string
	DatabaseURL           string
	RedisURL              string
	ConfigCacheEntries    int
	ConfigCacheMB         int
	ConfigRefreshInterval time.Duration
	ConfigRefreshMissRate float64
	DataScriptTimeout     time.Duration
	ControlPlaneTimeout   time.Duration
	LogLevel              string
	LogFormat             string
	ShutdownGrace         time.Duration
	IdempotencyTTL        time.Duration
	Migrate               bool
	BootstrapAdminKey     string
	BootstrapKeyIsAcked   bool

	// The IP-07 keys of deployment.md section 4.
	RequestTimeout      time.Duration
	MaxBodyBytes        int64
	MetadataMaxBytes    int
	KeyCacheEntries     int
	RateLimitEnabled    bool
	RateLimitRuntimeRPS float64
	RateLimitBurst      float64
	RateLimiterMaxKeys  int

	// The IP-09 keys: the per-client-address admin limit (D4) and the
	// proxies whose X-Forwarded-For it believes.
	RateLimitAdminRPM float64
	TrustedProxies    admin.Proxies

	// The IP-10 keys: apply-now's confirmation handshake (DR-046, S11).
	PlanApplyConfirmThreshold int64
	ConfirmationTTL           time.Duration

	// The IP-12 keys of deployment.md section 4.4: the ledger queue and
	// writer, and the retention job. A retention of zero keeps that table.
	EventQueueSize     int
	EventBatchSize     int
	EventFlushInterval time.Duration
	EventRetentionDays int
	AuditRetentionDays int
	RetentionInterval  time.Duration

	// The IP-11 keys: the startup mode of consistency-and-recovery.md
	// section 4.1, and how often the reconciliation pass runs (S1).
	StartupMode       string
	ReconcileInterval time.Duration

	// The IP-16 keys: the reset worker of deployment.md section 4.3 (Q-13).
	WorkerEnabled     bool
	ResetScanInterval time.Duration
	ResetBatchSize    int
}

// The startup modes (deployment.md section 4.1).
const (
	ModeNormal   = "normal"
	ModeReadOnly = "readonly"
)

// maxRefreshInterval bounds QUOTACORE_CONFIG_REFRESH_INTERVAL. A snapshot can
// be stale for at most one interval, and a stale anchor must never outlive the
// shortest first window under a new one, which is an hour (IP-10 D2, S11).
const maxRefreshInterval = time.Hour

type Getenv func(string) string

func Load(getenv Getenv) (Config, error) {
	c := Config{
		ListenAddr:            valueOr(getenv, "QUOTACORE_LISTEN_ADDR", ":8080"),
		MetricsAddr:           valueOr(getenv, "QUOTACORE_METRICS_ADDR", "127.0.0.1:9090"),
		DatabaseURL:           getenv("QUOTACORE_DATABASE_URL"),
		RedisURL:              getenv("QUOTACORE_REDIS_URL"),
		ConfigCacheEntries:    50000,
		ConfigCacheMB:         64,
		ConfigRefreshInterval: 30 * time.Second,
		ConfigRefreshMissRate: 20,
		DataScriptTimeout:     250 * time.Millisecond,
		ControlPlaneTimeout:   3 * time.Second,
		LogLevel:              valueOr(getenv, "QUOTACORE_LOG_LEVEL", "info"),
		LogFormat:             valueOr(getenv, "QUOTACORE_LOG_FORMAT", "json"),
		Migrate:               true,
		RequestTimeout:        2 * time.Second,
		MaxBodyBytes:          65536,
		MetadataMaxBytes:      4096,
		KeyCacheEntries:       10000,
		RateLimitAdminRPM:     60,
		RateLimitEnabled:      true,
		RateLimitRuntimeRPS:   2000,
		RateLimitBurst:        4000,
		RateLimiterMaxKeys:    100000,

		PlanApplyConfirmThreshold: 50,
		ConfirmationTTL:           900 * time.Second,

		EventQueueSize:     10000,
		EventBatchSize:     200,
		EventFlushInterval: 100 * time.Millisecond,
		EventRetentionDays: 90,
		AuditRetentionDays: 400,
		RetentionInterval:  time.Hour,

		StartupMode:       ModeNormal,
		ReconcileInterval: 5 * time.Minute,

		WorkerEnabled:     true,
		ResetScanInterval: 60 * time.Second,
		ResetBatchSize:    500,
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("QUOTACORE_DATABASE_URL is required and is not set")
	}
	if c.RedisURL == "" {
		return c, fmt.Errorf("QUOTACORE_REDIS_URL is required and is not set")
	}
	for name, addr := range map[string]string{"QUOTACORE_LISTEN_ADDR": c.ListenAddr, "QUOTACORE_METRICS_ADDR": c.MetricsAddr} {
		if err := unprivileged(name, addr); err != nil {
			return c, err
		}
	}
	if raw := getenv("QUOTACORE_CONFIG_CACHE_ENTRIES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return c, fmt.Errorf("QUOTACORE_CONFIG_CACHE_ENTRIES is %q, which is not a positive integer", raw)
		}
		c.ConfigCacheEntries = n
	}
	if raw := getenv("QUOTACORE_CONFIG_CACHE_MB"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return c, fmt.Errorf("QUOTACORE_CONFIG_CACHE_MB is %q, which is not a positive integer", raw)
		}
		c.ConfigCacheMB = n
	}
	if raw := getenv("QUOTACORE_CONFIG_REFRESH_INTERVAL"); raw != "" {
		d, err := parseDuration(raw, "QUOTACORE_CONFIG_REFRESH_INTERVAL")
		if err != nil {
			return c, err
		}
		if d >= maxRefreshInterval {
			return c, fmt.Errorf("QUOTACORE_CONFIG_REFRESH_INTERVAL is %q, which is not below one hour; a snapshot that stale could serve a window of an anchor that has moved", raw)
		}
		c.ConfigRefreshInterval = d
	}
	if raw := getenv("QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			return c, fmt.Errorf("QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD is %q, which is not a positive integer", raw)
		}
		c.PlanApplyConfirmThreshold = n
	}
	if raw := getenv("QUOTACORE_CONFIRMATION_TTL"); raw != "" {
		d, err := parseDuration(raw, "QUOTACORE_CONFIRMATION_TTL")
		if err != nil {
			return c, err
		}
		if d < time.Second {
			return c, fmt.Errorf("QUOTACORE_CONFIRMATION_TTL is %q, which is shorter than a second", raw)
		}
		c.ConfirmationTTL = d
	}
	if raw := getenv("QUOTACORE_CONFIG_REFRESH_MISS_RATE_LIMIT"); raw != "" {
		r, err := strconv.ParseFloat(raw, 64)
		if err != nil || r < 0 {
			return c, fmt.Errorf("QUOTACORE_CONFIG_REFRESH_MISS_RATE_LIMIT is %q, which is not a non-negative rate per second", raw)
		}
		c.ConfigRefreshMissRate = r
	}
	if raw := getenv("QUOTACORE_DATASCRIPT_TIMEOUT"); raw != "" {
		d, err := parseDuration(raw, "QUOTACORE_DATASCRIPT_TIMEOUT")
		if err != nil {
			return c, err
		}
		c.DataScriptTimeout = d
	}
	if raw := getenv("QUOTACORE_CONTROLPLANE_TIMEOUT"); raw != "" {
		d, err := parseDuration(raw, "QUOTACORE_CONTROLPLANE_TIMEOUT")
		if err != nil {
			return c, err
		}
		c.ControlPlaneTimeout = d
	}
	if raw := getenv("QUOTACORE_SHUTDOWN_GRACE"); raw != "" {
		d, err := parseDuration(raw, "QUOTACORE_SHUTDOWN_GRACE")
		if err != nil {
			return c, err
		}
		c.ShutdownGrace = d
	} else {
		c.ShutdownGrace = 30 * time.Second
	}
	if raw := getenv("QUOTACORE_IDEMPOTENCY_TTL"); raw != "" {
		d, err := parseDuration(raw, "QUOTACORE_IDEMPOTENCY_TTL")
		if err != nil {
			return c, err
		}
		c.IdempotencyTTL = d
	} else {
		c.IdempotencyTTL = IdempotencyWindow
	}
	if c.IdempotencyTTL != IdempotencyWindow {
		return c, fmt.Errorf("QUOTACORE_IDEMPOTENCY_TTL is %s and is pinned to %s, because the window is a customer-facing guarantee; a deployment that tries to shorten it is refused at startup",
			c.IdempotencyTTL, IdempotencyWindow)
	}
	if raw := getenv("QUOTACORE_MIGRATE"); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return c, fmt.Errorf("QUOTACORE_MIGRATE is %q, which is not a boolean", raw)
		}
		c.Migrate = b
	}
	if raw := getenv("QUOTACORE_REQUEST_TIMEOUT"); raw != "" {
		d, err := parseDuration(raw, "QUOTACORE_REQUEST_TIMEOUT")
		if err != nil {
			return c, err
		}
		c.RequestTimeout = d
	}
	for _, k := range []struct {
		key string
		dst *int
	}{
		{"QUOTACORE_METADATA_MAX_BYTES", &c.MetadataMaxBytes},
		{"QUOTACORE_KEY_CACHE_ENTRIES", &c.KeyCacheEntries},
		{"QUOTACORE_RATE_LIMITER_MAX_KEYS", &c.RateLimiterMaxKeys},
	} {
		if raw := getenv(k.key); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				return c, fmt.Errorf("%s is %q, which is not a positive integer", k.key, raw)
			}
			*k.dst = n
		}
	}
	if raw := getenv("QUOTACORE_MAX_BODY_BYTES"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			return c, fmt.Errorf("QUOTACORE_MAX_BODY_BYTES is %q, which is not a positive integer", raw)
		}
		c.MaxBodyBytes = n
	}
	if int64(c.MetadataMaxBytes) > c.MaxBodyBytes {
		return c, fmt.Errorf("QUOTACORE_METADATA_MAX_BYTES (%d) is larger than QUOTACORE_MAX_BODY_BYTES (%d), so the metadata cap could never be reached", c.MetadataMaxBytes, c.MaxBodyBytes)
	}
	for _, k := range []struct {
		key string
		dst *float64
	}{
		{"QUOTACORE_RATELIMIT_RUNTIME_RPS", &c.RateLimitRuntimeRPS},
		{"QUOTACORE_RATELIMIT_RUNTIME_BURST", &c.RateLimitBurst},
		{"QUOTACORE_RATELIMIT_ADMIN_RPM", &c.RateLimitAdminRPM},
	} {
		if raw := getenv(k.key); raw != "" {
			r, err := strconv.ParseFloat(raw, 64)
			if err != nil || r <= 0 {
				return c, fmt.Errorf("%s is %q, which is not a positive number", k.key, raw)
			}
			*k.dst = r
		}
	}
	if raw := getenv("QUOTACORE_RATELIMIT_ENABLED"); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return c, fmt.Errorf("QUOTACORE_RATELIMIT_ENABLED is %q, which is not a boolean", raw)
		}
		c.RateLimitEnabled = b
	}
	if err := loadLedger(getenv, &c); err != nil {
		return c, err
	}
	if err := loadReconcile(getenv, &c); err != nil {
		return c, err
	}
	if err := loadWorker(getenv, &c); err != nil {
		return c, err
	}
	proxies, err := admin.ParseProxies(getenv("QUOTACORE_TRUSTED_PROXIES"))
	if err != nil {
		return c, fmt.Errorf("QUOTACORE_TRUSTED_PROXIES: %w", err)
	}
	c.TrustedProxies = proxies
	c.BootstrapAdminKey = getenv("QUOTACORE_BOOTSTRAP_ADMIN_KEY")
	c.BootstrapKeyIsAcked = getenv("QUOTACORE_BOOTSTRAP_ADMIN_KEY_ACK") != ""
	if c.BootstrapAdminKey != "" && !c.BootstrapKeyIsAcked {
		return c, fmt.Errorf("refusing to start with a bootstrap admin key that has not been acknowledged; set QUOTACORE_BOOTSTRAP_ADMIN_KEY_ACK to any non-empty value to acknowledge it, and remove both from the environment once the key is issued")
	}
	// A malformed key refuses start rather than starting with no way in. The
	// message never contains the value (DR-043).
	if c.BootstrapAdminKey != "" {
		tok, err := auth.Parse(c.BootstrapAdminKey)
		if err != nil || tok.Label != auth.Admin {
			return c, fmt.Errorf("refusing to start: QUOTACORE_BOOTSTRAP_ADMIN_KEY is not an admin key in the qc_admin_<prefix>.<secret> format (ADR-0018); deployment.md section 4.1 shows how to mint one")
		}
	}
	return c, nil
}

// loadLedger reads the six keys of deployment.md section 4.4, refusing a value
// that would make the queue unbounded or useless, the writer spin or stall, or
// the retention job run so often it becomes load.
func loadLedger(getenv Getenv, c *Config) error {
	for _, k := range []struct {
		key string
		dst *int
		min int
	}{
		{"QUOTACORE_EVENT_QUEUE_SIZE", &c.EventQueueSize, 1},
		{"QUOTACORE_EVENT_BATCH_SIZE", &c.EventBatchSize, 1},
		{"QUOTACORE_EVENT_RETENTION_DAYS", &c.EventRetentionDays, 0},
		{"QUOTACORE_AUDIT_RETENTION_DAYS", &c.AuditRetentionDays, 0},
	} {
		if raw := getenv(k.key); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < k.min {
				return fmt.Errorf("%s is %q, which is not an integer of at least %d", k.key, raw, k.min)
			}
			*k.dst = n
		}
	}
	if c.EventBatchSize > c.EventQueueSize {
		return fmt.Errorf("QUOTACORE_EVENT_BATCH_SIZE (%d) is larger than QUOTACORE_EVENT_QUEUE_SIZE (%d), so a batch could never fill", c.EventBatchSize, c.EventQueueSize)
	}
	if raw := getenv("QUOTACORE_EVENT_FLUSH_INTERVAL"); raw != "" {
		d, err := parseDuration(raw, "QUOTACORE_EVENT_FLUSH_INTERVAL")
		if err != nil {
			return err
		}
		if d < time.Millisecond || d > 10*time.Second {
			return fmt.Errorf("QUOTACORE_EVENT_FLUSH_INTERVAL is %q, which is outside 1ms to 10s", raw)
		}
		c.EventFlushInterval = d
	}
	if raw := getenv("QUOTACORE_RETENTION_INTERVAL"); raw != "" {
		d, err := parseDuration(raw, "QUOTACORE_RETENTION_INTERVAL")
		if err != nil {
			return err
		}
		if d < time.Minute {
			return fmt.Errorf("QUOTACORE_RETENTION_INTERVAL is %q, which is shorter than a minute", raw)
		}
		c.RetentionInterval = d
	}
	return nil
}

// loadReconcile reads the two IP-11 keys. A mode that is neither value is
// refused rather than read as normal, because starting an instance that
// writes during a rebuild is the failure the mode exists to prevent.
func loadReconcile(getenv Getenv, c *Config) error {
	if raw := getenv("QUOTACORE_STARTUP_MODE"); raw != "" {
		if raw != ModeNormal && raw != ModeReadOnly {
			return fmt.Errorf("QUOTACORE_STARTUP_MODE is %q, which is neither %s nor %s", raw, ModeNormal, ModeReadOnly)
		}
		c.StartupMode = raw
	}
	if raw := getenv("QUOTACORE_RECONCILE_INTERVAL"); raw != "" {
		d, err := parseDuration(raw, "QUOTACORE_RECONCILE_INTERVAL")
		if err != nil {
			return err
		}
		if d < time.Minute {
			return fmt.Errorf("QUOTACORE_RECONCILE_INTERVAL is %q, which is shorter than a minute", raw)
		}
		c.ReconcileInterval = d
	}
	return nil
}

// loadWorker reads the three reset-worker keys (deployment.md section 4.3,
// Q-13). A scan more often than a second would be load rather than accuracy,
// and a batch of zero would scan nothing.
func loadWorker(getenv Getenv, c *Config) error {
	if raw := getenv("QUOTACORE_WORKER_ENABLED"); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("QUOTACORE_WORKER_ENABLED is %q, which is not true or false", raw)
		}
		c.WorkerEnabled = b
	}
	if raw := getenv("QUOTACORE_RESET_SCAN_INTERVAL"); raw != "" {
		d, err := parseDuration(raw, "QUOTACORE_RESET_SCAN_INTERVAL")
		if err != nil {
			return err
		}
		if d < time.Second {
			return fmt.Errorf("QUOTACORE_RESET_SCAN_INTERVAL is %q, which is shorter than a second", raw)
		}
		c.ResetScanInterval = d
	}
	if raw := getenv("QUOTACORE_RESET_BATCH_SIZE"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return fmt.Errorf("QUOTACORE_RESET_BATCH_SIZE is %q, which is not an integer of at least 1", raw)
		}
		c.ResetBatchSize = n
	}
	return nil
}

// Settle is how old a ledger row must be before the reconciliation pass reads
// it, and how long the pass waits before confirming a disagreement: the larger
// of a minute and ten flush intervals (IP-11 D3), so an event still in another
// instance's queue is never reported.
func (c Config) Settle() time.Duration { return max(time.Minute, 10*c.EventFlushInterval) }

func valueOr(getenv Getenv, key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseDuration(raw, key string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s is %q, which is not a duration such as 30s or 24h", key, raw)
	}
	return d, nil
}

func OSGetenv(key string) string { return os.Getenv(key) }

// unprivileged refuses a port below 1024, which only a privileged process
// can bind, so the service never needs a capability to start (NFR-S6, IP-13
// D13). Port 0, an ephemeral port, is allowed.
func unprivileged(name, addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s is %q, which is not a host:port address", name, addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%s is %q, whose port is not a number from 0 to 65535", name, addr)
	}
	if n > 0 && n < 1024 {
		return fmt.Errorf("%s is %q; a port below 1024 needs privileges the service does not run with, so choose 1024 or above", name, addr)
	}
	return nil
}
