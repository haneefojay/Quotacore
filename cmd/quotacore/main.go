package main

import (
	"context"
	"errors"
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

	"github.com/quotacore/quotacore/internal/api"
	"github.com/quotacore/quotacore/internal/db"
	"github.com/quotacore/quotacore/internal/observability"
	"github.com/quotacore/quotacore/internal/script"
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

	snap := snapshot.New(snapshot.Options{
		LimitEntries:        cfg.ConfigCacheEntries,
		ByteCap:             int64(cfg.ConfigCacheMB) << 20,
		MissRate:            cfg.ConfigRefreshMissRate,
		RefreshInterval:     cfg.ConfigRefreshInterval,
		ControlPlaneTimeout: cfg.ControlPlaneTimeout,
		Client:              snapshotClient{data},
	})
	metrics := observability.New(observability.Options{HitRatio: snap.HitRatio})
	snap.WithMetrics(metrics)
	snap.Start(ctx)
	defer snap.Close()

	// The three scripts are loaded once at startup so the first request does not
	// pay for it (ADR-0002, NFR-S8). A failure here is a warning rather than a
	// refusal to start: the runner reloads all three and retries a call exactly
	// once whenever the store answers NOSCRIPT, so a store that was unreachable
	// at startup heals itself on the first request that reaches it. Refusing to
	// start would turn a transient store outage into an outage of the process,
	// which is the opposite of what fail-closed means.
	runner, err := script.New(script.Options{Client: data.Client(), Timeout: cfg.DataScriptTimeout})
	if err != nil {
		return err
	}
	loadCtx, cancelLoad := context.WithTimeout(ctx, cfg.ControlPlaneTimeout)
	if err := script.Load(loadCtx, runner.Client()); err != nil {
		log.Warn("scripts were not in the store at startup; the first request will put them back", "error", err)
	}
	cancelLoad()
	// The pool wait is the mechanism by which a control-plane problem could
	// reach the data plane, so it is watched from the first second
	// (observability.md section 1.4).
	db.StartPoolObserver(ctx, dbConn, metrics, 10*time.Second)

	handler := api.NewRouter(api.RouterOptions{Ready: state.ReadyFuncLogging(func(msg string, terms []string) {
		if msg == "ready" {
			log.Info("readiness changed", "ready", true)
			return
		}
		log.Warn("readiness changed", "ready", false, "unsatisfied", terms)
	})})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
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
