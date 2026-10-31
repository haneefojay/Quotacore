package dataplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strconv"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/rolling"
)

// The contract's patterns, repeated here so the aggregating pass can name every
// offending field at once. The generated validator runs after this pass on the
// same body, so a pattern that drifted from the contract is caught there, not
// served.
var (
	featureKeyShape = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)
	externalIDShape = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	clientKeyShape  = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,255}$`)
)

const amountCeiling = 1<<53 - 1

// issue is one offending field or header, as it appears in details.errors.
type issue struct {
	Field string `json:"field"`
	Issue string `json:"issue"`
	// code is the field's own code when it has one (invalid_amount,
	// invalid_window, missing_idempotency_key); empty means validation_failed.
	code contract.ErrorCode
}

// metered is a decoded, validated consume, refund or check request.
type metered struct {
	FeatureKey string
	Amount     int64
	ExternalID string
	ClientKey  string
	Metadata   json.RawMessage
	// Window is the request's window, empty when it sent none. That it is
	// one of the seven was checked here; that it is the one the feature's
	// entitlement configures is checked once the feature is resolved
	// (DR-052).
	Window string
}

// decodeMetered reads and validates a metered request. Every problem is
// collected before any is reported, because the contract promises that three
// bad fields produce one response naming all three. The precedence (IP-07,
// S2) is: exactly one problem that has a code of its own is answered with that
// code; anything else is validation_failed naming every problem.
func decodeMetered(r *http.Request, op kind, metadataMax int) (metered, *envelope.Problem) {
	var issues []issue
	if ct := r.Header.Get("Content-Type"); !isJSON(ct) {
		issues = append(issues, issue{Field: "Content-Type", Issue: "must be application/json"})
	}
	if r.Header.Get("Content-Encoding") != "" {
		issues = append(issues, issue{Field: "Content-Encoding", Issue: "a request body is never compressed"})
	}
	var in metered
	if op != opCheck {
		in.ClientKey = r.Header.Get("Idempotency-Key")
		switch {
		case in.ClientKey == "":
			issues = append(issues, issue{Field: "Idempotency-Key", Issue: "is required on this route",
				code: contract.ErrorCodeMissingIdempotencyKey})
		case !clientKeyShape.MatchString(in.ClientKey):
			issues = append(issues, issue{Field: "Idempotency-Key", Issue: "must be 8 to 255 of A-Z, a-z, 0-9, '.', '_', ':' and '-'"})
		}
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return in, tooLarge(op, "body", tooBig.Limit)
		}
		p := envelope.Error(contract.ErrorCodeValidationFailed, "the request body could not be read")
		return in, &p
	}
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&fields); err != nil || fields == nil || dec.More() {
		issues = append(issues, issue{Field: "body", Issue: "must be one JSON object"})
		return in, reject(issues)
	}

	if m, ok := fields["metadata"]; ok {
		if len(m) > metadataMax {
			return in, tooLarge(op, "metadata", int64(metadataMax))
		}
		if !isObject(m) {
			issues = append(issues, issue{Field: "metadata", Issue: "must be an object"})
		}
		in.Metadata = m
	}
	in.FeatureKey, issues = stringField(fields, "feature_key", featureKeyShape, issues)
	in.ExternalID, issues = stringField(fields, "tenant_external_id", externalIDShape, issues)
	in.Amount, issues = amountField(fields, issues)
	if raw, ok := fields["window"]; ok {
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			issues = append(issues, issue{Field: "window", Issue: "must be one of the rolling windows, as a string",
				code: contract.ErrorCodeInvalidWindow})
		} else if _, known := rolling.Parse(name); !known {
			issues = append(issues, issue{Field: "window", Issue: fmt.Sprintf("%q is not a supported window; "+
				"use rolling_5m, rolling_15m, rolling_1h, rolling_6h, rolling_24h, rolling_7d or rolling_30d", name),
				code: contract.ErrorCodeInvalidWindow})
		} else {
			in.Window = name
		}
	}
	var unknown []string
	for name := range fields {
		switch name {
		case "feature_key", "amount", "tenant_external_id", "metadata", "window":
		default:
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		issues = append(issues, issue{Field: name, Issue: "is not a field of this request"})
	}
	if len(issues) > 0 {
		return in, reject(issues)
	}

	// The generated decoder and validator are the contract's own. Anything the
	// pass above let through and they refuse is a drift between the two, and
	// the contract wins.
	var generated contract.MeteredRequest
	if err := generated.UnmarshalJSON(raw); err != nil {
		return in, reject([]issue{{Field: "body", Issue: err.Error()}})
	}
	if err := generated.Validate(); err != nil {
		return in, reject([]issue{{Field: "body", Issue: err.Error()}})
	}
	return in, nil
}

// reject turns the collected issues into one Problem.
func reject(issues []issue) *envelope.Problem {
	if len(issues) == 1 && issues[0].code != "" {
		i := issues[0]
		p := envelope.Error(i.code, i.Field+" "+i.Issue).With("field", i.Field)
		return &p
	}
	msg := fmt.Sprintf("%d problems with the request", len(issues))
	if len(issues) == 1 {
		msg = issues[0].Field + " " + issues[0].Issue
	}
	p := envelope.Error(contract.ErrorCodeValidationFailed, msg).With("errors", issues)
	return &p
}

// tooLarge is 413 payload_too_large where the contract lists it. POST /v1/check
// does not list it (error-catalog.md section 3), so there the same refusal is
// validation_failed naming the field: the request is still refused before it
// is enforced, and the route still returns only codes its row allows.
func tooLarge(op kind, field string, limit int64) *envelope.Problem {
	if op == opCheck {
		return reject([]issue{{Field: field, Issue: fmt.Sprintf("is larger than %d bytes", limit)}})
	}
	p := envelope.Error(contract.ErrorCodePayloadTooLarge, field+" is larger than the limit").
		With("field", field).With("limit_bytes", limit)
	return &p
}

func stringField(fields map[string]json.RawMessage, name string, shape *regexp.Regexp, issues []issue) (string, []issue) {
	raw, ok := fields[name]
	if !ok {
		return "", append(issues, issue{Field: name, Issue: "is required"})
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", append(issues, issue{Field: name, Issue: "must be a string"})
	}
	if !shape.MatchString(s) {
		return "", append(issues, issue{Field: name, Issue: "does not match " + shape.String()})
	}
	return s, issues
}

// amountField accepts a JSON integer and nothing else: 4200.0 and 4.2e3 are
// floats and are refused, because amounts are integers (api-conventions.md).
// A negative integer is invalid_amount, because a sign is never a direction
// (DR-023).
func amountField(fields map[string]json.RawMessage, issues []issue) (int64, []issue) {
	raw, ok := fields["amount"]
	if !ok {
		return 0, append(issues, issue{Field: "amount", Issue: "is required"})
	}
	text := string(bytes.TrimSpace(raw))
	if len(text) == 0 || !(text[0] == '-' || (text[0] >= '0' && text[0] <= '9')) {
		return 0, append(issues, issue{Field: "amount", Issue: "must be an integer, got " + truncate(text)})
	}
	if bytes.ContainsAny(raw, ".eE") {
		return 0, append(issues, issue{Field: "amount", Issue: "must be an integer, got " + truncate(text)})
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		if text[0] == '-' {
			return 0, append(issues, issue{Field: "amount", Issue: "must not be negative", code: contract.ErrorCodeInvalidAmount})
		}
		return 0, append(issues, issue{Field: "amount", Issue: "must be at most 2^53 - 1"})
	}
	if n < 0 {
		return 0, append(issues, issue{Field: "amount", Issue: "must not be negative", code: contract.ErrorCodeInvalidAmount})
	}
	if n > amountCeiling {
		return 0, append(issues, issue{Field: "amount", Issue: "must be at most 2^53 - 1"})
	}
	return n, issues
}

func truncate(s string) string {
	if len(s) > 32 {
		return s[:32] + "…"
	}
	return s
}

func isObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '{'
}

func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/json"
}
