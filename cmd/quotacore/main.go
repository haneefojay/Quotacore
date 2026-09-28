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

	"github.com/quotacore/quotacore/internal/api"
	"github.com/quotacore/quotacore/internal/db"
)

const (
	termDataStore   = "data store answered within the timeout"
	termSnapshot    = "snapshot is populated"
	termMigrations  = "migrations are current"
	termDraining    = "not draining"
	termTenantsSeen = "at least one tenant is known"
)

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
