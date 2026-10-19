package api

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/auth"
	"github.com/quotacore/quotacore/internal/envelope"
)

// requestIDShape is the contract's RequestId: a ULID or a UUID. An inbound
// X-Request-Id of that shape is adopted; anything else is replaced, so a
// header cannot inject text into a log line (error-catalog.md section 1).
var requestIDShape = regexp.MustCompile(`^(?:[0-9A-HJKMNP-TV-Z]{26}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

// statusWriter remembers what was written, for the access log and for the
// recovery that must not write a second status.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// accept is step 1 of request-lifecycle.md section 1 for every route: the one
// clock read, the request id on the response before anything else can fail,
// Cache-Control: no-store, recovery from a panic into a 500 envelope, and one
// log line when the response is done.
func (rt *router) accept(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		at := rt.now()
		id := r.Header.Get("X-Request-Id")
		if !requestIDShape.MatchString(id) {
			id = newRequestID()
		}
		ex := envelope.NewExchange(id, at, rt.countError)
		w.Header().Set("X-Request-Id", id)
		w.Header().Set("Cache-Control", "no-store")
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				rt.log.LogAttrs(r.Context(), slog.LevelError, "panic",
					slog.String("request_id", id), slog.String("endpoint", endpointOf(ex)), slog.Any("error", rec))
				if sw.status == 0 {
					envelope.Write(sw, r.WithContext(envelope.With(r.Context(), ex)),
						envelope.Error(contract.ErrorCodeInternalError, "the request could not be served"))
				}
			}
			rt.finish(r, ex, sw.status)
		}()
		next.ServeHTTP(sw, r.WithContext(envelope.With(r.Context(), ex)))
	})
}

func newRequestID() string {
	if u, err := uuid.NewV7(); err == nil {
		return u.String()
	}
	return uuid.NewString()
}

func endpointOf(ex *envelope.Exchange) string {
	if ex.Endpoint == "" {
		return "unmatched"
	}
	return ex.Endpoint
}

// finish writes the access log line and observes the latency. The line
// carries the request id and what a support conversation needs, and never a
// secret, an external_id or metadata (observability.md section 2.1). The
// probes log at debug, because an orchestrator polls them.
func (rt *router) finish(r *http.Request, ex *envelope.Exchange, status int) {
	if status == 0 {
		status = http.StatusOK
	}
	elapsed := rt.now().Sub(ex.Accepted)
	attrs := []slog.Attr{
		slog.String("request_id", ex.RequestID),
		slog.String("endpoint", endpointOf(ex)),
		slog.Int("status", status),
		slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
	}
	if ex.Outcome != "" {
		attrs = append(attrs, slog.String("outcome", ex.Outcome))
	}
	if ex.Code != "" {
		attrs = append(attrs, slog.String("code", string(ex.Code)))
	}
	if ex.KeyPrefix != "" {
		attrs = append(attrs, slog.String("key_prefix", ex.KeyPrefix))
	}
	attrs = append(attrs, ex.Attrs()...)
	level := slog.LevelInfo
	if ex.Endpoint == "GET /healthz" || ex.Endpoint == "GET /readyz" {
		level = slog.LevelDebug
	}
	msg := "request"
	if op, ok := rt.dataPlane[ex.Endpoint]; ok {
		msg = op
		if rt.metrics != nil {
			outcome := ex.Outcome
			if outcome == "" {
				// A 4xx the caller caused: not a business outcome and not a
				// failure of the service (observability.md section 1.1).
				outcome = "refused"
			}
			rt.metrics.Latency.WithLabelValues(ex.Endpoint, outcome).Observe(elapsed.Seconds())
		}
	}
	rt.log.LogAttrs(r.Context(), level, msg, attrs...)
}

// countError is quotacore_errors_total's only door.
func (rt *router) countError(code contract.ErrorCode, endpoint string) {
	if rt.metrics != nil && endpoint != "" {
		rt.metrics.Errors.WithLabelValues(string(code), endpoint).Inc()
	}
}

// operation wraps one contract route: the body cap, authentication, the
// per-key rate limit on the data plane, the scope gate, and the request
// timeout, in request-lifecycle.md's order. An operation whose contract lists
// no error codes (GET /v1/admin/status) is refused without an envelope.
func (rt *router) operation(op envelope.Operation, h http.Handler) http.Handler {
	endpoint := op.Endpoint()
	bare := len(op.Codes) == 0
	has413 := false
	for _, c := range op.Codes {
		has413 = has413 || c == contract.ErrorCodePayloadTooLarge
	}
	required := auth.Scope(op.RequiredScope)
	dataPlane := required == auth.Runtime
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ex := envelope.From(r.Context())
		ex.Endpoint = endpoint
		refuse := func(p envelope.Problem) {
			if bare {
				w.WriteHeader(envelope.Status(p.Code))
				return
			}
			envelope.Write(w, r, p)
		}

		if op.HasBody {
			if r.ContentLength > rt.maxBody {
				refuse(bodyTooLarge(has413, rt.maxBody))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, rt.maxBody)
		}

		if required != "" {
			if rt.auth == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			key, err := rt.auth.Authenticate(r.Header.Get("Authorization"), ex.Accepted)
			if err != nil {
				p := authProblem(err)
				if envelope.Status(p.Code) == http.StatusUnauthorized {
					w.Header().Set("WWW-Authenticate", `Bearer realm="quotacore"`)
				}
				refuse(p)
				return
			}
			ex.KeyPrefix = key.Prefix
			if dataPlane && rt.limiter != nil {
				if ok, wait := rt.limiter.Allow(key.Prefix, ex.Accepted); !ok {
					if rt.metrics != nil {
						rt.metrics.RateLimited.WithLabelValues(key.Prefix).Inc()
					}
					refuse(rateLimited(wait, rt.limiter.Rate()))
					return
				}
			}
			if !key.Scope.Allows(required) {
				refuse(envelope.Error(contract.ErrorCodeInsufficientScope, "this route needs a key of another scope").
					With("required", string(required)).With("presented", string(key.Scope)))
				return
			}
		}

		ctx, cancel := context.WithDeadline(r.Context(), ex.Accepted.Add(rt.timeout))
		defer cancel()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bodyTooLarge(has413 bool, limit int64) envelope.Problem {
	if has413 {
		return envelope.Error(contract.ErrorCodePayloadTooLarge, "the request body is larger than the limit").
			With("field", "body").With("limit_bytes", limit)
	}
	return envelope.Error(contract.ErrorCodeValidationFailed, "the request body is larger than the limit").
		With("errors", []map[string]string{{"field": "body", "issue": "is larger than the limit"}})
}

func authProblem(err error) envelope.Problem {
	switch err {
	case auth.ErrNoCredential:
		return envelope.Error(contract.ErrorCodeUnauthorized, "send Authorization: Bearer <key>")
	case auth.ErrMissRefused:
		return envelope.Error(contract.ErrorCodeServiceUnavailable, "the key could not be checked right now").Retry(100)
	case auth.ErrUnavailable:
		return envelope.Error(contract.ErrorCodeControlPlaneUnavailable, "the key could not be checked right now").Retry(100)
	default:
		return envelope.Error(contract.ErrorCodeAPIKeyInvalid, "the API key is unknown, revoked, expired or malformed")
	}
}

// rateLimited is DR-040's 429: Retry-After in whole seconds rounded up, the
// same wait in milliseconds, and the limit that was hit. It carries no
// cycle_end, which is what keeps it distinguishable from quota_exceeded.
func rateLimited(wait time.Duration, rate float64) envelope.Problem {
	ms := int64(math.Ceil(float64(wait) / float64(time.Millisecond)))
	if ms < 1 {
		ms = 1
	}
	secs := int(math.Ceil(float64(ms) / 1000))
	p := envelope.Error(contract.ErrorCodeRateLimited, "this key is sending faster than the service's limit").
		Retry(ms).With("limit_per_second", rate)
	p.RetryAfter = secs
	return p
}
