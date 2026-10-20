package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Embed the time-zone database so a boundary is correct without a system
	// database and without egress, and so a host that updates its zone data
	// cannot shift a customer's boundaries retroactively (NFR-OPS8, IP-03 DoD 7).
	_ "time/tzdata"

	"github.com/quotacore/quotacore/internal/auth"
	"github.com/quotacore/quotacore/internal/db"
	"github.com/quotacore/quotacore/internal/snapshot"
	"github.com/quotacore/quotacore/internal/store"
)

const (
	termDataStore   = "data store answered within the timeout"
	termSnapshot    = "snapshot is populated"
	termMigrations  = "migrations are current"
	termDraining    = "not draining"
	termTenantsSeen = "at least one tenant is known"
)

// snapshotClient adapts *store.Store to snapshot.Client. The store returns its
// concrete subscription type; the adapter is the one place the two closed
// worlds meet, so snapshot keeps a test-double-facing interface and store keeps
// a concrete method, and neither imports the other.
type snapshotClient struct{ *store.Store }

func (c snapshotClient) SubscribeInvalidations(ctx context.Context) (snapshot.InvalidationSource, error) {
	return c.Store.SubscribeInvalidations(ctx)
}

func main() {
	if err := run(); err != nil {
		slog.Error("refusing to start", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := Load(OSGetenv)
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	slog.SetDefault(log)

	state := newReadinessState(termDataStore, termSnapshot, termMigrations, termTenantsSeen, termDraining)
	state.Set(termDraining, true)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbConn, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer dbConn.Close()

	if err := db.Migrate(ctx, dbConn, db.MigrateOptions{Disabled: !cfg.Migrate}); err != nil {
		return err
	}
	current, err := db.MigrationsCurrent(ctx, dbConn)
	if err != nil {
		return err
	}
	state.Set(termMigrations, current)

	if err := bootstrap(ctx, cfg, log, dbConn); err != nil {
		return err
	}

	data, err := store.Open(store.Options{URL: cfg.RedisURL, DataScriptTimeout: cfg.DataScriptTimeout})
	if err != nil {
		return err
	}
	defer data.Close()

	// The datastore is the fail-closed half of availability (NFR-A3): if it does
	// not answer within the control-plane timeout at startup, the process still
	// starts, /readyz reports the term unsatisfied, and every enforcement that
	// reaches the pool fails closed with 503 rather than the service pretending
	// it can enforce.
	pingCtx, cancelPing := context.WithTimeout(ctx, cfg.ControlPlaneTimeout)
	if err := data.Ping(pingCtx); err != nil {
		log.Warn("data store did not answer at startup; readiness will report it", "error", err)
	} else {
		state.Set(termDataStore, true)
	}
	cancelPing()

	app, err := assemble(ctx, cfg, log, dbConn, data, state.ReadyFuncLogging(func(msg string, terms []string) {
		if msg == "ready" {
			log.Info("readiness changed", "ready", true)
			return
		}
		log.Warn("readiness changed", "ready", false, "unsatisfied", terms)
	}), nil)
	if err != nil {
		return err
	}
	defer app.Close()
	handler := app.Handler

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		stop()
	}

	state.Set(termDraining, false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	log.Info("draining", "grace", cfg.ShutdownGrace.String())
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	log.Info("stopped")
	return nil
}

// bootstrap installs the operator-supplied admin key on an empty api_keys
// table (D3). The key was already checked for its format by Load; only its
// prefix ever reaches a log line (DR-043, NFR-S9).
func bootstrap(ctx context.Context, cfg Config, log *slog.Logger, dbConn *sql.DB) error {
	if cfg.BootstrapAdminKey == "" {
		has, err := db.HasKeys(ctx, dbConn)
		if err != nil {
			return fmt.Errorf("check for api keys: %w", err)
		}
		if !has {
			log.Warn("no API key exists and no bootstrap key is set, so the admin API has no way in; set QUOTACORE_BOOTSTRAP_ADMIN_KEY and its acknowledgement (deployment.md section 4.1)")
		}
		return nil
	}
	tok, err := auth.Parse(cfg.BootstrapAdminKey)
	if err != nil {
		return errors.New("refusing to start: QUOTACORE_BOOTSTRAP_ADMIN_KEY is not an admin key in the token format")
	}
	outcome, err := db.Bootstrap(ctx, dbConn, tok.Prefix, tok.Hash())
	if err != nil {
		return fmt.Errorf("install the bootstrap admin key: %w", err)
	}
	if outcome == db.BootstrapInstalled {
		log.Warn("installed the bootstrap admin key; remove it and its acknowledgement from the environment once another admin key is issued",
			"key_prefix", tok.Prefix)
		return nil
	}
	log.Warn("ignoring QUOTACORE_BOOTSTRAP_ADMIN_KEY, because API keys already exist; remove it from the environment",
		"key_prefix", tok.Prefix)
	return nil
}

func newLogger(cfg Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
