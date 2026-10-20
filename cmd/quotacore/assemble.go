package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"
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
	"github.com/quotacore/quotacore/internal/reconcile"
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
	// Pass is the in-service reconciliation, and Reconcile its two counters
	// (IP-11 D2).
	Pass      *reconcile.Pass
	Reconcile *observability.Reconcile
	// Fence is the instance's shared hold on the rebuild fence, nil in
	// read-only mode (IP-11 D6).
	Fence *db.Fence
	// Data is the fast store, whose ReadOnly says the instance's mode.
	Data *store.Store
	// Service is the data plane, whose Reverse a test's own pass calls.
	Service *dataplane.Service
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
	// reconcileFirstRun and settle shorten the reconciliation pass's first
	// run and its settle period (IP-11 D3).
	reconcileFirstRun time.Duration
	settle            time.Duration
	// fenceEvery is how often the fence is checked; zero means five seconds.
	fenceEvery time.Duration
}

// hostname names a ledger session; an unreadable one is still a session.
func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// observe, when not nil, is handed every event the ledger is; production
// passes nil.
func assemble(ctx context.Context, cfg Config, log *slog.Logger, dbConn *sql.DB, data *store.Store,
	ready api.Readiness, observe dataplane.EventSink, opts ...assembleOptions) (*enforcement, error) {
	var opt assembleOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	// Read-only mode, and the fence that makes it a property of the fleet
	// (IP-11 D5, D6). An instance started read-only writes no tenant key and
	// holds no fence, so a rebuild can take it; a normal instance holds the
	// fence shared for its life, refuses to start while a rebuild holds it,
	// and serves read-only whenever it has lost it.
	var fence *db.Fence
	var fenceConn *sql.DB
	if cfg.StartupMode == ModeReadOnly {
		data.SetReadOnly(true)
		log.Warn("starting in read-only mode: no balance or idempotency record is written until a restart in normal mode")
	} else {
		var err error
		fenceConn, err = db.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			return nil, err
		}
		fenceConn.SetMaxOpenConns(1)
		fence = db.NewFence(fenceConn, log)
		held, err := fence.Acquire(ctx)
		if err != nil || !held {
			fenceConn.Close()
			if err == nil {
				err = db.ErrFenced
			}
			return nil, err
		}
		data.SetReadOnly(false)
	}
	readOnly := data.ReadOnly
	settle := cfg.Settle()
	if opt.settle > 0 {
		settle = opt.settle
	}
	firstPass := opt.reconcileFirstRun
	if firstPass <= 0 {
		firstPass = time.Minute
	}
	fenceEvery := opt.fenceEvery
	if fenceEvery <= 0 {
		fenceEvery = 5 * time.Second
	}
	// The ledger writer has its own pool of two connections, so a slow ledger
	// cannot take the admin pool or the snapshot's bounded miss read
	// (performance.md section 5, IP-12 D4).
	ledgerConn, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		if fence != nil {
			fence.Release()
			fenceConn.Close()
		}
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
		// The loss record (IP-11 D8). A request can spend up to its timeout
		// between its accepted instant and its enqueue.
		Sessions: db.NewLedger(ledgerConn),
		Host:     hostname(),
		Lag:      cfg.RequestTimeout + time.Second,
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
		Paused:    readOnly,
	})
	if fence != nil {
		go fence.Keep(ctx, fenceEvery, func(held bool) { data.SetReadOnly(!held) })
	}
	closeLedger := func() {
		cancelWriter()
		ledgerConn.Close()
		if fence != nil {
			fence.Release()
			fenceConn.Close()
		}
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
		ReadOnly:         readOnly,
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
	// The reconciliation pass runs in every instance and one runs at a time;
	// it reverses detected duplicates and verifies what changed (IP-11 D2).
	// It has a pool of its own, so a pass holding its lock and reading
	// cannot take the ledger writer's two connections.
	reconcileConn, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		cancel()
		snap.Close()
		closeLedger()
		return nil, err
	}
	reconcileConn.SetMaxOpenConns(3)
	counters := observability.NewReconcile()
	pass := reconcile.NewPass(reconcile.PassOptions{
		Ledger:   db.NewReconciler(reconcileConn),
		Balances: balances{data},
		Refunder: refunder{plane: service, now: time.Now},
		Metrics:  counters,
		Logger:   log,
		Settle:   settle,
		Paused:   readOnly,
	})
	go pass.Run(ctx, firstPass, cfg.ReconcileInterval)
	handler := api.NewRouter(api.RouterOptions{
		// A service that cannot enforce is not ready, and a read-only one
		// cannot (IP-11 D5, IP-13 DoD 5).
		Ready:          func() bool { return !readOnly() && ready() },
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
		Handler:   handler,
		Snapshot:  snap,
		Keys:      keys,
		Metrics:   metrics,
		Plane:     plane,
		Ledger:    sink,
		Drain:     sink.Shutdown,
		Pass:      pass,
		Reconcile: counters,
		Fence:     fence,
		Data:      data,
		Service:   service,
		Close: func() {
			cancel()
			snap.Close()
			closeLedger()
			reconcileConn.Close()
		},
	}, nil
}
