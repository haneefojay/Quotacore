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
	"github.com/quotacore/quotacore/internal/cli"
	"github.com/quotacore/quotacore/internal/db"
	"github.com/quotacore/quotacore/internal/snapshot"
	"github.com/quotacore/quotacore/internal/store"
)

// The readiness terms of observability.md section 4 (IP-13 D7). Postgres is
// not among them: cached tenants enforce through a control-plane outage.
const (
	termDataStore  = "data store answered within the timeout"
	termNoEviction = "data store refuses to evict"
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
	// One dispatch for the whole binary (IP-14 D11): no argument, or serve,
	// runs the service; the local operations run here; every other command
	// is internal/cli's, against a running service.
	stdin, _ := os.Stdin.Stat()
	os.Exit(cli.Run(cli.Options{
		Args:     os.Args[1:],
		Getenv:   os.Getenv,
		Stdin:    os.Stdin,
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
		Terminal: stdin != nil && stdin.Mode()&os.ModeCharDevice != 0,
		Version:  buildVersion,
		Locals:   locals(os.Stdout, os.Stderr),
	}))
}

// locals are the commands the binary carries out itself.
func locals(stdout, stderr io.Writer) map[string]func([]string) int {
	m := map[string]func([]string) int{"serve": serve}
	for _, name := range []string{"verify-tenant", "verify-all", "reconcile-report", "rebuild"} {
		m[name] = func(args []string) int { return runCommand(append([]string{name}, args...), stdout, stderr) }
	}
	return m
}

// serve runs the service until a signal, and exits 1 when it refuses to
// start (IP-01 DoD 4).
func serve(args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "error: serve takes no argument; the configuration is the environment (deployment.md section 4)\n")
		return cli.ExitUsage
	}
	// Until the configuration is read, the log is JSON at info, so a refusal
	// to start is a line a collector can parse like any other (IP-13 D6).
	log := newLogger(os.Stdout, "json", "info")
	if err := run(&log); err != nil {
		log.Error("refusing to start", "job", "startup", "error", err)
		return cli.ExitFinding
	}
	return cli.ExitOK
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

	// Migrations and the bootstrap run on a connection of their own with no
	// statement timeout: a migration may wait on another instance's lock, or
	// build an index, for longer than an admin query may take.
	setup, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	if err := db.Migrate(ctx, setup, db.MigrateOptions{Disabled: !cfg.Migrate}); err != nil {
		setup.Close()
		return err
	}
	if err := bootstrap(ctx, cfg, log, setup); err != nil {
		setup.Close()
		return err
	}
	setup.Close()

	dbConn, err := openServicePool(ctx, cfg)
	if err != nil {
		return err
	}
	defer dbConn.Close()

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
	if err := shutdown(srv, metricsSrv, app.Drain, cfg.ShutdownGrace, log); err != nil {
		return err
	}
	log.Info("stopped")
	return nil
}

// openServicePool opens the pool the admin API, the snapshot and the key cache
// read through, with deployment.md section 7's per-connection settings: an
// explicit search_path, and a statement timeout of QUOTACORE_CONTROLPLANE_TIMEOUT
// so a hung admin query fails rather than holding a slot (IP-15 D19). The
// ledger, fence and reconciliation pools are their own, and keep the server's
// timeout, because a reconciliation read is bounded by its own job.
func openServicePool(ctx context.Context, cfg Config) (*sql.DB, error) {
	return db.OpenWith(ctx, cfg.DatabaseURL, db.OpenOptions{StatementTimeout: cfg.ControlPlaneTimeout})
}

// ledgerDrainAfterOverrun is the ledger's own budget when the HTTP drain used
// the whole grace. It is short and separate, because the events of requests
// that did answer are history a customer will dispute, and a drain that
// overran is exactly when they are most likely to be queued (IP-15 D9).
const ledgerDrainAfterOverrun = 5 * time.Second

// shutdown drains the API listener within grace (NFR-A5), then the ledger,
// then the metrics listener. When requests are still open at the end of the
// grace they are closed and the ledger still drains, on its own short budget,
// before the overrun is reported: returning first would drop every queued
// event of a request that had already answered.
func shutdown(srv, metricsSrv *http.Server, drain func(context.Context), grace time.Duration, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	log.Info("draining", "grace", grace.String())
	overrun := srv.Shutdown(ctx)
	if overrun != nil {
		log.Warn("the grace ended with requests still open; closing them, then writing the ledger", "error", overrun)
		_ = srv.Close()
		var cancelLedger context.CancelFunc
		ctx, cancelLedger = context.WithTimeout(context.Background(), ledgerDrainAfterOverrun)
		defer cancelLedger()
	}
	// Every request has answered or been closed, so nothing more is
	// enqueued; the ledger writes what is queued (IP-12 D8).
	drain(ctx)
	_ = metricsSrv.Shutdown(ctx)
	return overrun
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
