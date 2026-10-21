package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
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

// The readiness terms of observability.md section 4 (IP-13 D7). Postgres is
// not among them: cached tenants enforce through a control-plane outage.
const (
	termDataStore  = "data store answered within the timeout"
	termSnapshot   = "snapshot has read the configuration"
	termMigrations = "migrations are current"
	termDraining   = "not draining"
	termNormal     = "serving in normal mode"
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
	// An argument names a local operation (IP-11 D2b); none serves.
	if len(os.Args) > 1 {
		if !isCommand(os.Args[1:]) {
			fmt.Fprintf(os.Stderr, "unknown command %q; the commands are verify-tenant, verify-all, reconcile-report and rebuild, and no argument serves\n", os.Args[1])
			os.Exit(exitError)
		}
		os.Exit(runCommand(os.Args[1:], os.Stdout, os.Stderr))
	}
	// Until the configuration is read, the log is JSON at info, so a refusal
	// to start is a line a collector can parse like any other (IP-13 D6).
	log := newLogger(os.Stdout, "json", "info")
	if err := run(&log); err != nil {
		log.Error("refusing to start", "job", "startup", "error", err)
		os.Exit(1)
	}
}

func run(logp **slog.Logger) error {
	cfg, err := Load(OSGetenv)
	if err != nil {
		return err
	}
	*logp = newLogger(os.Stdout, cfg.LogFormat, cfg.LogLevel)
	log := (*logp).With("job", "startup")
	if os.Geteuid() == 0 {
		log.Warn("running as root; the service needs no privilege, so run it as an unprivileged user (NFR-S6)")
	}

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

	if err := bootstrap(ctx, cfg, log, dbConn); err != nil {
		return err
	}

	data, err := store.Open(store.Options{URL: cfg.RedisURL, DataScriptTimeout: cfg.DataScriptTimeout})
	if err != nil {
		return err
	}
	defer data.Close()

	// The datastore is the fail-closed half of availability (NFR-A3): if it
	// does not answer, the process still starts, the prober keeps /readyz
	// unready, and every enforcement that reaches the pool fails closed with
	// 503 rather than the service pretending it can enforce.
	app, err := assemble(ctx, cfg, *logp, dbConn, data, nil)
	if err != nil {
		return err
	}
	defer app.Close()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app.Handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// The metrics listener is its own server, on loopback by default, so a
	// scrape never shares the API's port (IP-13 D8).
	if !loopback(cfg.MetricsAddr) {
		log.Warn("the metrics listener is not on a loopback address, and it takes no credential; bind it to 127.0.0.1 unless a network policy restricts it (security-model.md section 7)",
			"addr", cfg.MetricsAddr)
	}
	metricsSrv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           app.MetricsHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	errc := make(chan error, 2)
	serve := func(srv *http.Server, what string) {
		log.Info("listening", "addr", srv.Addr, "listener", what)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}
	go serve(srv, "api")
	go serve(metricsSrv, "metrics")

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		stop()
	}

	app.Ready.Set(termDraining, false)
	log = (*logp).With("job", "shutdown")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	log.Info("draining", "grace", cfg.ShutdownGrace.String())
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	// Every request has answered, so nothing more is enqueued; the ledger
	// writes what is queued within what remains of the grace (IP-12 D8).
	app.Drain(shutdownCtx)
	_ = metricsSrv.Shutdown(shutdownCtx)
	log.Info("stopped")
	return nil
}

// loopback reports whether a listen address binds only the loopback
// interface. An empty host binds every interface.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// newLogger is the one logger the process writes with, and the harness
// builds its own with it, so the tests read what ships (IP-13 D6). JSON lines
// carry ts, in UTC to the microsecond, and a lowercase level, as
// observability.md section 2 shows. text is for a person at a terminal.
func newLogger(w io.Writer, format, levelName string) *slog.Logger {
	level := slog.LevelInfo
	switch levelName {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: logShape}
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func logShape(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.TimeKey:
		return slog.String("ts", a.Value.Time().UTC().Format("2006-01-02T15:04:05.000000Z"))
	case slog.LevelKey:
		return slog.String("level", strings.ToLower(a.Value.String()))
	}
	return a
}
