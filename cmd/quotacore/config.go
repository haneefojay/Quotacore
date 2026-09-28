package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const IdempotencyWindow = 24 * time.Hour

type Config struct {
	ListenAddr          string
	DatabaseURL         string
	RedisURL            string
	LogLevel            string
	LogFormat           string
	ShutdownGrace       time.Duration
	IdempotencyTTL      time.Duration
	Migrate             bool
	BootstrapAdminKey   string
	BootstrapKeyIsAcked bool
}

type Getenv func(string) string

func Load(getenv Getenv) (Config, error) {
	c := Config{
		ListenAddr:  valueOr(getenv, "QUOTACORE_LISTEN_ADDR", ":8080"),
		DatabaseURL: getenv("QUOTACORE_DATABASE_URL"),
		RedisURL:    getenv("QUOTACORE_REDIS_URL"),
		LogLevel:    valueOr(getenv, "QUOTACORE_LOG_LEVEL", "info"),
		LogFormat:   valueOr(getenv, "QUOTACORE_LOG_FORMAT", "json"),
		Migrate:     true,
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("QUOTACORE_DATABASE_URL is required and is not set")
	}
	if c.RedisURL == "" {
		return c, fmt.Errorf("QUOTACORE_REDIS_URL is required and is not set")
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
	c.BootstrapAdminKey = getenv("QUOTACORE_BOOTSTRAP_ADMIN_KEY")
	c.BootstrapKeyIsAcked = getenv("QUOTACORE_BOOTSTRAP_ADMIN_KEY_ACK") != ""
	if c.BootstrapAdminKey != "" && !c.BootstrapKeyIsAcked {
		return c, fmt.Errorf("refusing to start with a bootstrap admin key that has not been acknowledged; set QUOTACORE_BOOTSTRAP_ADMIN_KEY_ACK to any non-empty value to acknowledge it, and remove both from the environment once the key is issued")
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
