package envelope

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	contract "github.com/quotacore/quotacore/api"
)

// Exchange is the request-scoped record of one request: what the access log
// line and the metrics need to know about it once it has been answered. The
// middleware creates it at accept, the handlers add to it, and nothing else
// reads it. It is not safe for concurrent use, and a request is served by one
// goroutine.
type Exchange struct {
	// RequestID is adopted or minted at accept (NFR-S10).
	RequestID string
	// Accepted is the one clock read of the request: key expiry, the cycle
	// window and the script's instant are all this value (request-lifecycle.md
	// section 6).
	Accepted time.Time
	// Endpoint is the contract operation, "POST /v1/consume", or empty for a
	// path the contract does not have.
	Endpoint string
	// Status and Code are what was written. Code is empty on a 2xx.
	Status int
	Code   contract.ErrorCode
	// Outcome is the data-plane outcome label: allowed, denied, replayed or
	// rejected. Empty off the data plane.
	Outcome string
	// KeyPrefix is the authenticated key's public prefix, the one key
	// identifier a log or a label may carry (DR-043).
	KeyPrefix string
	// KeyID and KeyName are the authenticated key's id and name, which an
	// admin route records as the actor of its audit row (DR-041).
	KeyID, KeyName string

	attrs   []slog.Attr
	onError func(code contract.ErrorCode, endpoint string)
}

// NewExchange starts the record for a request. onError is called once for
// every error envelope written, with the code and the endpoint; it is how
// quotacore_errors_total counts without every handler remembering to.
func NewExchange(requestID string, accepted time.Time, onError func(contract.ErrorCode, string)) *Exchange {
	return &Exchange{RequestID: requestID, Accepted: accepted, onError: onError}
}

// Add records fields for the access log line. Callers add the numbers a
// support conversation needs (observability.md section 2), never a secret,
// an external_id or metadata.
func (e *Exchange) Add(attrs ...slog.Attr) { e.attrs = append(e.attrs, attrs...) }

// Attrs returns what Add recorded.
func (e *Exchange) Attrs() []slog.Attr { return e.attrs }

type exchangeKey struct{}

// With returns a context carrying the exchange.
func With(ctx context.Context, e *Exchange) context.Context {
	return context.WithValue(ctx, exchangeKey{}, e)
}

// From returns the request's exchange. A request that did not pass the
// middleware gets a detached record, so a handler never has to check for nil;
// the record it writes to is simply not logged.
func From(ctx context.Context) *Exchange {
	if e, ok := ctx.Value(exchangeKey{}).(*Exchange); ok {
		return e
	}
	return &Exchange{Accepted: time.Now()}
}

// Problem is one error, before it becomes an envelope. The status is not a
// field: it is the contract's for the code.
type Problem struct {
	Code    contract.ErrorCode
	Message string
	// RetryAfterMs is set only when retrying is the correct action, and is
	// absent rather than null otherwise (error-catalog.md section 1).
	RetryAfterMs *int64
	// RetryAfter, when positive, is written as the Retry-After header in
	// seconds. Only rate_limited carries it (DR-040).
	RetryAfter int
	Details    map[string]any
}

// Error is a Problem with no details.
func Error(code contract.ErrorCode, message string) Problem {
	return Problem{Code: code, Message: message}
}

// Retry returns a copy of p that tells the caller to retry after ms.
func (p Problem) Retry(ms int64) Problem {
	p.RetryAfterMs = &ms
	return p
}

// With returns a copy of p with one more detail.
func (p Problem) With(key string, value any) Problem {
	details := make(map[string]any, len(p.Details)+1)
	for k, v := range p.Details {
		details[k] = v
	}
	details[key] = value
	p.Details = details
	return p
}

type wireError struct {
	Error wireBody `json:"error"`
}

type wireBody struct {
	Code         contract.ErrorCode `json:"code"`
	Message      string             `json:"message"`
	RequestID    string             `json:"request_id"`
	RetryAfterMs *int64             `json:"retry_after_ms,omitempty"`
	Details      map[string]any     `json:"details,omitempty"`
}

// Write answers the request with the envelope for p. It is the only function
// that writes an error body, which is what makes "no endpoint returns a bare
// 400 or a bare 500" a property of the code rather than of discipline.
func Write(w http.ResponseWriter, r *http.Request, p Problem) {
	e := From(r.Context())
	status := Status(p.Code)
	e.Status, e.Code = status, p.Code
	if e.onError != nil {
		e.onError(p.Code, e.Endpoint)
	}
	body, err := json.Marshal(wireError{Error: wireBody{
		Code:         p.Code,
		Message:      p.Message,
		RequestID:    e.RequestID,
		RetryAfterMs: p.RetryAfterMs,
		Details:      p.Details,
	}})
	if err != nil {
		// A detail that cannot be marshalled is a defect in this service, and
		// the envelope still has to go out: drop the details, keep the code.
		body, _ = json.Marshal(wireError{Error: wireBody{Code: p.Code, Message: p.Message, RequestID: e.RequestID}})
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if p.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(p.RetryAfter))
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// JSON answers with a 2xx body. body is one of the generated contract types,
// whose encoder is the contract's own.
func JSON(w http.ResponseWriter, r *http.Request, status int, body json.Marshaler) {
	e := From(r.Context())
	e.Status = status
	encoded, err := body.MarshalJSON()
	if err != nil {
		Write(w, r, Error(contract.ErrorCodeInternalError, "the response could not be encoded"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}
