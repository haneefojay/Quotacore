package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/quotacore/quotacore/internal/auth"
	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/observability"
)

// Authenticator verifies a bearer token at the instant a request was
// accepted. internal/auth's Authenticator satisfies it.
type Authenticator interface {
	Authenticate(header string, at time.Time) (auth.Key, error)
}

type RouterOptions struct {
	Ready Readiness
	// DataPlane serves the four data-plane routes, and Auth authenticates every
	// /v1 route. Without both, the /v1 routes answer a bare 404, which is what
	// a build before IP-07 answered.
	DataPlane *dataplane.Service
	Auth      Authenticator
	// RateLimit is the per-key limit on the data plane. Nil disables it
	// (QUOTACORE_RATELIMIT_ENABLED=false).
	RateLimit *dataplane.RateLimiter
	Metrics   *observability.DataPlane
	Logger    *slog.Logger
	// MaxBodyBytes is QUOTACORE_MAX_BODY_BYTES; RequestTimeout is
	// QUOTACORE_REQUEST_TIMEOUT, measured from accept.
	MaxBodyBytes   int64
	RequestTimeout time.Duration
	Now            func() time.Time
}

type router struct {
	auth      Authenticator
	limiter   *dataplane.RateLimiter
	metrics   *observability.DataPlane
	log       *slog.Logger
	maxBody   int64
	timeout   time.Duration
	now       func() time.Time
	dataPlane map[string]string // endpoint → operation id, for the four routes
}

// NewRouter builds the HTTP surface: the operational routes, the four
// data-plane routes, and every /v1/admin route of the contract behind
// authentication and the admin scope gate.
//
// The admin routes are registered so the gate holds on every one of them
// before IP-09 writes a single handler (IP-07 DoD 8). An admin key that passes
// the gate gets a bare 404 until then: the behaviour does not exist yet, and a
// 501 would be the service raising a code for a surface it does have. A path
// that is in no contract route is a bare 404 and a wrong method a bare 405
// with Allow, both without authentication, because neither names anything to
// protect.
func NewRouter(opts RouterOptions) http.Handler {
	rt := &router{
		auth:      opts.Auth,
		limiter:   opts.RateLimit,
		metrics:   opts.Metrics,
		log:       opts.Logger,
		maxBody:   opts.MaxBodyBytes,
		timeout:   opts.RequestTimeout,
		now:       opts.Now,
		dataPlane: map[string]string{},
	}
	if rt.log == nil {
		rt.log = slog.New(slog.DiscardHandler)
	}
	if rt.maxBody <= 0 {
		rt.maxBody = 64 << 10
	}
	if rt.timeout <= 0 {
		rt.timeout = 2 * time.Second
	}
	if rt.now == nil {
		rt.now = time.Now
	}
	if opts.DataPlane == nil {
		rt.auth = nil
	}

	liveness, readiness := Health(opts.Ready)
	operational := map[string]http.Handler{
		"healthz":         liveness,
		"readyz":          readiness,
		"openapiDocument": http.HandlerFunc(serveOpenAPI),
		"docs":            http.HandlerFunc(serveDocs),
	}
	var dp map[string]http.HandlerFunc
	if opts.DataPlane != nil {
		dp = map[string]http.HandlerFunc{
			"consume": opts.DataPlane.Consume,
			"refund":  opts.DataPlane.Refund,
			"check":   opts.DataPlane.Check,
			"balance": opts.DataPlane.Balance,
		}
	}

	r := chi.NewRouter()
	r.Use(rt.accept)
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	for _, op := range envelope.MustOperations() {
		var h http.Handler
		switch {
		case op.RequiredScope == "":
			// /metrics is IP-13's route, and stays absent until then.
			if h = operational[op.ID]; h == nil {
				continue
			}
			h = rt.named(op, h)
		case dp[op.ID] != nil:
			rt.dataPlane[op.Endpoint()] = op.ID
			h = rt.operation(op, dp[op.ID])
		default:
			h = rt.operation(op, http.HandlerFunc(notYet))
		}
		r.Method(op.Method, op.Path, h)
	}
	return r
}

// named records the endpoint of an operational route, which takes no
// credential and no body.
func (rt *router) named(op envelope.Operation, h http.Handler) http.Handler {
	endpoint := op.Endpoint()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envelope.From(r.Context()).Endpoint = endpoint
		h.ServeHTTP(w, r)
	})
}

// notYet answers a contract route whose handler belongs to a later phase.
func notYet(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }
