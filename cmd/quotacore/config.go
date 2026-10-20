package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/quotacore/quotacore/internal/admin"
	"github.com/quotacore/quotacore/internal/auth"
)

const IdempotencyWindow = 24 * time.Hour

type Config struct {
	ListenAddr            string
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
}

type Getenv func(string) string

func Load(getenv Getenv) (Config, error) {
	c := Config{
		ListenAddr:            valueOr(getenv, "QUOTACORE_LISTEN_ADDR", ":8080"),
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
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("QUOTACORE_DATABASE_URL is required and is not set")
	}
	if c.RedisURL == "" {
		return c, fmt.Errorf("QUOTACORE_REDIS_URL is required and is not set")
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
		c.ConfigRefreshInterval = d
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
