package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/store"
)

// actor is the audit actor of a request: the key the router authenticated.
func actor(r *http.Request) Actor {
	ex := envelope.From(r.Context())
	return Actor{KeyID: ex.KeyID, Name: ex.KeyName, RequestID: ex.RequestID}
}

func accepted(r *http.Request) time.Time { return envelope.From(r.Context()).Accepted }

// respond writes a contract resource.
func respond(w http.ResponseWriter, r *http.Request, status int, v json.Marshaler) {
	envelope.JSON(w, r, status, v)
}

func noContent(w http.ResponseWriter, r *http.Request) {
	envelope.From(r.Context()).Status = http.StatusNoContent
	w.WriteHeader(http.StatusNoContent)
}

func problem(w http.ResponseWriter, r *http.Request, p *envelope.Problem) { envelope.Write(w, r, *p) }

// fail answers a store or decoding error with its catalogue code. A refusal
// the store named carries its code; an error the store could not classify is
// the control plane failing to answer, or a defect.
func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	var own *refusal
	if errors.As(err, &own) {
		envelope.Write(w, r, own.problem)
		return
	}
	if errors.Is(err, store.ErrReadOnly) {
		readOnly(w, r)
		return
	}
	var down errDataStore
	if errors.As(err, &down) {
		s.log.Warn("the data store did not answer an admin write; nothing was committed",
			"request_id", envelope.From(r.Context()).RequestID, "error", down.err)
		envelope.Write(w, r, envelope.Error(contract.ErrorCodeServiceUnavailable,
			"the data store did not answer, so nothing was changed").Retry(1000))
		return
	}
	var big errTooLarge
	if errors.As(err, &big) {
		envelope.Write(w, r, envelope.Error(contract.ErrorCodePayloadTooLarge, big.field+" is larger than the limit").
			With("field", big.field).With("limit_bytes", big.limit))
		return
	}
	var c Coded
	if errors.As(err, &c) {
		code := c.CatalogueCode()
		p := envelope.Error(code, message(code))
		switch code {
		case contract.ErrorCodeValidationFailed:
			field := c.CatalogueField()
			if field == "" {
				field = "body"
			}
			p = envelope.Error(code, field+" is not acceptable").
				With("errors", []issue{{Field: field, Issue: "is not acceptable"}})
		case contract.ErrorCodeResourceVersionConflict:
			if v, ok := c.CurrentVersion(); ok {
				p = p.With("current_version", v)
			}
		case contract.ErrorCodeInternalError:
			s.log.Error("an admin request failed", "request_id", envelope.From(r.Context()).RequestID, "error", err)
		}
		envelope.Write(w, r, p)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || err != nil {
		s.log.Warn("the control plane did not answer an admin request",
			"request_id", envelope.From(r.Context()).RequestID, "error", err)
		envelope.Write(w, r, envelope.Error(contract.ErrorCodeControlPlaneUnavailable,
			"the control plane did not answer").Retry(1000))
	}
}

// readOnly is the 503 for a write a rebuild's read-only mode refuses (IP-11
// D5): nothing was committed and nothing was changed.
func readOnly(w http.ResponseWriter, r *http.Request) {
	envelope.Write(w, r, envelope.Error(contract.ErrorCodeServiceUnavailable,
		"the service is in read-only recovery mode, so nothing was changed").Retry(1000))
}

// message is the human line of each refusal an admin route answers. It names
// no value the caller sent.
func message(code contract.ErrorCode) string {
	switch code {
	case contract.ErrorCodeFeatureNotFound:
		return "no feature has that key"
	case contract.ErrorCodePlanNotFound:
		return "no plan has that identifier"
	case contract.ErrorCodeTenantNotFound:
		return "no tenant has that identifier"
	case contract.ErrorCodeAPIKeyNotFound:
		return "no API key has that id"
	case contract.ErrorCodeFeatureKeyExists:
		return "a feature with that key exists"
	case contract.ErrorCodePlanKeyExists:
		return "a plan with that key exists"
	case contract.ErrorCodeTenantExists:
		return "a tenant with that external_id exists"
	case contract.ErrorCodeTenantDeleted:
		return "the tenant is deleted, which is terminal"
	case contract.ErrorCodePlanInUse:
		return "a tenant is assigned to the plan; archive it instead"
	case contract.ErrorCodeFeatureInUse:
		return "a plan, override or usage event references the feature; archive it instead"
	case contract.ErrorCodeFeatureUnitImmutable:
		return "the feature is referenced, so its unit cannot change"
	case contract.ErrorCodeResourceVersionConflict:
		return "the resource changed since it was read; re-read it and retry with the current version"
	case contract.ErrorCodeServiceUnavailable:
		return "the data store did not answer"
	}
	return "the request could not be served"
}

func notFound(code contract.ErrorCode) *envelope.Problem {
	p := envelope.Error(code, message(code))
	return &p
}

// refusal is a route's own refusal, decided in Go rather than by the store,
// with the details its catalogue entry carries. It rolls back the transaction
// it is returned from, so like every refusal it changes nothing.
type refusal struct{ problem envelope.Problem }

func (e *refusal) Error() string { return "refused: " + string(e.problem.Code) }

func refuse(p envelope.Problem) error { return &refusal{problem: p} }

// invalidField is validation_failed naming one field, the shape the store's
// refusals take.
func invalidField(field, problem string) error {
	return refuse(envelope.Error(contract.ErrorCodeValidationFailed, field+" "+problem).
		With("errors", []issue{{Field: field, Issue: problem}}))
}

// errDataStore is the fast store failing inside a write, which rolls the
// write back and is answered 503 service_unavailable.
type errDataStore struct{ err error }

func (e errDataStore) Error() string { return "data store: " + e.err.Error() }
func (e errDataStore) Unwrap() error { return e.err }
