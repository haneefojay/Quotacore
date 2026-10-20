package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/admin"
	"github.com/quotacore/quotacore/internal/api"
	"github.com/quotacore/quotacore/internal/auth"
	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/db"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/ledger"
	"github.com/quotacore/quotacore/internal/observability"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/snapshot"
	"github.com/quotacore/quotacore/internal/store"
)

// enforcement is the assembled request path: the snapshot, the key cache, the
// script runner, the data-plane service and the router in front of them. run
// builds one and serves it; the integration tests build one the same way and
// serve it from httptest, so what they prove is the wiring that ships.
type enforcement struct {
	Handler  http.Handler
	Snapshot *snapshot.Snapshot
	Keys     *auth.Authenticator
	Metrics  *observability.Metrics
	Plane    *observability.DataPlane
	// Ledger is the event queue and its writer (IP-12).
	Ledger *ledger.Sink
	// Drain stops the ledger taking events and writes what is queued, within
	// ctx; main calls it after the HTTP server has finished its requests.
	Drain func(ctx context.Context)
	Close func()
}

// tee hands every event to the ledger and to an observer, which is how the
// integration tests see what a route enqueued without reading the table.
type tee struct{ a, b dataplane.EventSink }

func (t tee) Enqueue(e dataplane.Event) { t.a.Enqueue(e); t.b.Enqueue(e) }

// assembleOptions are what the tests vary; the zero value is what ships.
type assembleOptions struct {
	// adminStore replaces the Postgres admin store, so a test can wrap it
	// with a commit hook (IP-09 DoD 4).
	adminStore admin.Store
	// appender replaces the ledger's Postgres appender, so a test can stall
	// it or lose an acknowledgement (IP-12 DoD 3, 4, 8).
	appender func(*db.Ledger) ledger.Appender
	// noWriter leaves the ledger's writer unstarted, so events queue and are
	// never written (IP-12 DoD 8).
	noWriter bool
	// retentionFirstRun shortens the delay before the first retention tick.
	retentionFirstRun time.Duration
}

// observe, when not nil, is handed every event the ledger is; production
// passes nil.
func assemble(ctx context.Context, cfg Config, log *slog.Logger, dbConn *sql.DB, data *store.Store,
	ready api.Readiness, observe dataplane.EventSink, opts ...assembleOptions) (*enforcement, error) {
	var opt assembleOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	// The ledger writer has its own pool of two connections, so a slow ledger
	// cannot take the admin pool or the snapshot's bounded miss read
	// (performance.md section 5, IP-12 D4).
	ledgerConn, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	ledgerConn.SetMaxOpenConns(2)
	ledgerConn.SetMaxIdleConns(2)
	var appender ledger.Appender = db.NewLedger(ledgerConn)
	if opt.appender != nil {
		appender = opt.appender(db.NewLedger(ledgerConn))
	}
	sink := ledger.New(ledger.Options{
		QueueSize:     cfg.EventQueueSize,
		BatchSize:     cfg.EventBatchSize,
		FlushInterval: cfg.EventFlushInterval,
		Appender:      appender,
		Logger:        log,
	})
	var events dataplane.EventSink = sink
	if observe != nil {
		events = tee{sink, observe}
	}
	// The writer runs on its own context: Close cancels it last, after Drain
	// has had the shutdown grace to write what is queued.
	writerCtx, cancelWriter := context.WithCancel(context.WithoutCancel(ctx))
	if !opt.noWriter {
		go sink.Run(writerCtx)
	}
	ctx, cancel := context.WithCancel(ctx)
	// Retention is a background job, never an API call (DR-044, NFR-C6). It
	// runs in every instance and one prunes at a time.
	go db.RunRetention(ctx, ledgerConn, db.RetentionOptions{
		EventDays: cfg.EventRetentionDays,
		AuditDays: cfg.AuditRetentionDays,
		Interval:  cfg.RetentionInterval,
		FirstRun:  opt.retentionFirstRun,
		Logger:    log,
		Metrics:   sink.Metrics(),
	})
	closeLedger := func() {
		cancelWriter()
		ledgerConn.Close()
	}
	snap := snapshot.New(snapshot.Options{
		LimitEntries:        cfg.ConfigCacheEntries,
		ByteCap:             int64(cfg.ConfigCacheMB) << 20,
		MissRate:            cfg.ConfigRefreshMissRate,
		RefreshInterval:     cfg.ConfigRefreshInterval,
		ControlPlaneTimeout: cfg.ControlPlaneTimeout,
		Client:              snapshotClient{data},
		Loader:              db.NewLoader(dbConn, data),
	})
	metrics := observability.New(observability.Options{HitRatio: snap.HitRatio})
	snap.WithMetrics(metrics)

	// The key cache reconciles on every change the snapshot learns of and on
	// its own refresh tick, off the request path, so a revocation reaches a
	// cached key without a request paying for the read.
	keys := auth.New(auth.Options{
		Loader:       db.NewKeyLoader(dbConn),
		CacheEntries: cfg.KeyCacheEntries,
		MissRate:     cfg.ConfigRefreshMissRate,
		Timeout:      cfg.ControlPlaneTimeout,
	})
	snap.OnChange(keys.Invalidate)
	go keys.Run(ctx, cfg.ConfigRefreshInterval, func(what string, err error) {
		log.Warn("could not "+what, "error", err)
	})
	snap.Start(ctx)

	// The three scripts are loaded once at startup so the first request does not
	// pay for it (ADR-0002, NFR-S8). A failure here is a warning rather than a
	// refusal to start: the runner reloads all three and retries a call exactly
	// once whenever the store answers NOSCRIPT, so a store that was unreachable
	// at startup heals itself on the first request that reaches it. Refusing to
	// start would turn a transient store outage into an outage of the process,
	// which is the opposite of what fail-closed means.
	runner, err := script.New(script.Options{Client: data.Client(), Timeout: cfg.DataScriptTimeout})
	if err != nil {
		cancel()
		snap.Close()
		closeLedger()
		return nil, err
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

	// Every (code, endpoint) cell the contract allows is warmed at zero, so the
	// code label set of quotacore_errors_total is the catalogue (IP-07 DoD 7).
	var cells [][2]string
	for _, op := range envelope.MustOperations() {
		for _, code := range op.Codes {
			cells = append(cells, [2]string{string(code), op.Endpoint()})
		}
	}
	plane := observability.NewDataPlane(cells)
	service := dataplane.New(dataplane.Options{
		Tenants:     snap,
		Scripts:     runner,
		Balances:    data,
		Metrics:     plane,
		Events:      events,
		MetadataMax: cfg.MetadataMaxBytes,
		StoreBudget: cfg.DataScriptTimeout,
	})
	var limiter, adminLimit *dataplane.RateLimiter
	if cfg.RateLimitEnabled {
		limiter = dataplane.NewRateLimiter(cfg.RateLimitRuntimeRPS, cfg.RateLimitBurst, cfg.RateLimiterMaxKeys)
		// Per client address, refilling the per-minute rate with a burst of
		// the same size (D4).
		adminLimit = dataplane.NewRateLimiter(cfg.RateLimitAdminRPM/60, cfg.RateLimitAdminRPM, cfg.RateLimiterMaxKeys)
	}
	adminStore := opt.adminStore
	if adminStore == nil {
		adminStore = db.NewAdmin(dbConn)
	}
	control := admin.New(admin.Options{
		Store:          adminStore,
		Data:           data,
		Keys:           keys,
		Logger:         log,
		MetadataMax:    cfg.MetadataMaxBytes,
		PublishTimeout: cfg.ControlPlaneTimeout,
		Scripts:        runner,
		Events:         events,
		// The confirmation threshold and the token's lifetime (DR-046).
		ConfirmThreshold: cfg.PlanApplyConfirmThreshold,
		ConfirmationTTL:  cfg.ConfirmationTTL,
		Status: admin.StatusInfo{
			Version: buildVersion,
			Commit:  buildCommit(),
			Started: time.Now(),
			Cache: func() contract.CacheStats {
				st := snap.Stats()
				return contract.CacheStats{Entries: st.Entries, Tenants: st.Tenants, Features: st.Features, SizeBytes: st.Bytes}
			},
			Ledger: sink,
		},
	})
	// Pending re-anchors are finished at start and on every refresh tick, so
	// one a request could not finish inline, or an instance that stopped
	// mid-drain, is finished by whichever instance reaches it first (D2).
	go control.RunDrain(ctx, cfg.ConfigRefreshInterval)
	handler := api.NewRouter(api.RouterOptions{
		Ready:          ready,
		DataPlane:      service,
		Auth:           keys,
		RateLimit:      limiter,
		Admin:          control.Handlers(),
		AdminLimit:     adminLimit,
		Proxies:        cfg.TrustedProxies,
		Metrics:        plane,
		Logger:         log,
		MaxBodyBytes:   cfg.MaxBodyBytes,
		RequestTimeout: cfg.RequestTimeout,
	})
	return &enforcement{
		Handler:  handler,
		Snapshot: snap,
		Keys:     keys,
		Metrics:  metrics,
		Plane:    plane,
		Ledger:   sink,
		Drain:    sink.Shutdown,
		Close: func() {
			cancel()
			snap.Close()
			closeLedger()
		},
	}, nil
}
