// Package dataplane is the four routes a customer's application calls on the
// request path (request-lifecycle.md section 1): consume, refund, check and
// balance. It validates, resolves the tenant and the feature against the
// snapshot, computes the window with internal/cycle, and hands the decision to
// the scripts in internal/script, which are the only place a balance changes.
//
// The package performs no control-plane I/O of its own. The snapshot's bounded
// miss is the only way configuration is read, and it is reached through an
// interface, so the source check that keeps a database handle off the data
// plane covers this package unchanged.
package dataplane

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/observability"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/snapshot"
	"github.com/quotacore/quotacore/internal/store"
)

// Tenants is the slice of the snapshot the handlers use.
type Tenants interface {
	Tenant(externalID string) (*snapshot.Tenant, error)
	Decide(t *snapshot.Tenant, featureKey string, at time.Time) snapshot.Resolution
}

// Scripts is the slice of the script runner the handlers use.
type Scripts interface {
	Consume(ctx context.Context, req script.Request) (script.Outcome, error)
	Refund(ctx context.Context, req script.Request) (script.Outcome, error)
	Check(ctx context.Context, req script.Request) (script.Outcome, error)
	ConsumeBoolean(ctx context.Context, req script.Request) (script.Decision, error)
	CheckBoolean(ctx context.Context, req script.Request) (script.Decision, error)
	ConsumeRolling(ctx context.Context, req script.RollingRequest) (script.RollingOutcome, error)
	CheckRolling(ctx context.Context, req script.RollingRequest) (script.RollingOutcome, error)
	RefundRolling(ctx context.Context, req script.RollingRequest) (script.RollingOutcome, error)
}

// Balances reads stored balance hashes for the balance route, as the key
// that asked: a key bound to another tenant reads nothing (DR-053).
type Balances interface {
	ReadBalancesAs(ctx context.Context, binding, tenantID string, features []string) ([]store.Stored, error)
}

// Options configure a Service.
type Options struct {
	Tenants  Tenants
	Scripts  Scripts
	Balances Balances
	Events   EventSink // nil is NopSink
	Metrics  *observability.DataPlane
	// Cycles counts a balance read above its ceiling (DR-019, IP-13 D2); the
	// scripts' own outcomes are counted by the runner.
	Cycles *observability.Cycles
	// MetadataMax is QUOTACORE_METADATA_MAX_BYTES, the cap on the metadata
	// object's encoded size (NFR-T6).
	MetadataMax int
	// StoreBudget is the data store's share of NFR-L4's 250 ms, measured from
	// accept. Time spent on a bounded control-plane miss is added to it, so a
	// tenant's first sight is not charged to the data store.
	StoreBudget time.Duration
}

// Service holds the handlers' dependencies.
type Service struct {
	tenants     Tenants
	scripts     Scripts
	balances    Balances
	events      EventSink
	metrics     *observability.DataPlane
	cycles      *observability.Cycles
	metadataMax int
	storeBudget time.Duration
	zones       sync.Map // zone name → *time.Location
}

// New builds a Service.
func New(opts Options) *Service {
	if opts.Events == nil {
		opts.Events = NopSink{}
	}
	if opts.MetadataMax <= 0 {
		opts.MetadataMax = 4096
	}
	if opts.StoreBudget <= 0 {
		opts.StoreBudget = 250 * time.Millisecond
	}
	return &Service{
		tenants:     opts.Tenants,
		scripts:     opts.Scripts,
		balances:    opts.Balances,
		events:      opts.Events,
		metrics:     opts.Metrics,
		cycles:      opts.Cycles,
		metadataMax: opts.MetadataMax,
		storeBudget: opts.StoreBudget,
	}
}

type kind int

const (
	opConsume kind = iota
	opRefund
	opCheck
)

func (k kind) String() string {
	switch k {
	case opConsume:
		return "consume"
	case opRefund:
		return "refund"
	default:
		return "check"
	}
}

// retryShortly is the retry_after_ms of a 503 (IP-07, S1): a failure of a
// dependency is worth retrying soon, with the same Idempotency-Key.
const retryShortly = 100

// tenant resolves an external_id, retrying once when the snapshot was
// refreshed under the request, and maps the snapshot's typed errors to the
// catalogue. It also reports how long it spent, which is control-plane time
// when the tenant was a miss.
func (s *Service) tenant(externalID string) (*snapshot.Tenant, time.Duration, *envelope.Problem) {
	start := time.Now()
	t, err := s.tenants.Tenant(externalID)
	if errors.Is(err, snapshot.ErrStaleSnapshot) {
		t, err = s.tenants.Tenant(externalID)
	}
	spent := time.Since(start)
	if err == nil {
		return t, spent, nil
	}
	var p envelope.Problem
	switch {
	case errors.Is(err, snapshot.ErrTenantUnknown):
		p = envelope.Error(contract.ErrorCodeTenantNotFound, "no tenant has that tenant_external_id").
			With("tenant_external_id", externalID)
	case errors.Is(err, snapshot.ErrStoreUnavailable):
		p = envelope.Error(contract.ErrorCodeServiceUnavailable,
			"the tenant is not cached and the data store did not answer").Retry(retryShortly)
	case errors.Is(err, snapshot.ErrRefreshFailed):
		p = envelope.Error(contract.ErrorCodeControlPlaneUnavailable,
			"the tenant is not cached and the control plane did not answer").Retry(retryShortly)
	default: // ErrMissRefused, a second ErrStaleSnapshot
		p = envelope.Error(contract.ErrorCodeServiceUnavailable,
			"the tenant could not be resolved right now").Retry(retryShortly)
	}
	return nil, spent, &p
}

// bound refuses a bound key's request for any tenant but its own, as an
// unknown tenant is refused, so the key learns nothing about another
// tenant's existence, state or plan (DR-053, Q-38). It runs before the gate.
// The scripts and the balance read check the same binding again, inside the
// store, so this check is the courteous answer and not the guarantee.
func bound(ex *envelope.Exchange, t *snapshot.Tenant, externalID string) *envelope.Problem {
	if ex.KeyTenant == "" || ex.KeyTenant == t.ID {
		return nil
	}
	p := foreign(externalID)
	return &p
}

// foreign is the answer to a bound key's request for another tenant: exactly
// the unknown tenant's.
func foreign(externalID string) envelope.Problem {
	return envelope.Error(contract.ErrorCodeTenantNotFound, "no tenant has that tenant_external_id").
		With("tenant_external_id", externalID)
}

// location returns the tenant's zone, loading each name once. The zone was
// validated when the tenant was written, so a failure is a defect.
// Location is a tenant's zone, loaded once, for the reset worker.
func (s *Service) Location(name string) (*time.Location, error) { return s.location(name) }

func (s *Service) location(name string) (*time.Location, error) {
	if loc, ok := s.zones.Load(name); ok {
		return loc.(*time.Location), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	s.zones.Store(name, loc)
	return loc, nil
}

// unavailable is the 503 for a data-store failure, with the counter DR-045
// asks for when the cause is a missing balance.
func (s *Service) unavailable(ex *envelope.Exchange, err error) envelope.Problem {
	if errors.Is(err, script.ErrStateMissing) {
		if s.metrics != nil {
			s.metrics.BalanceKeyMissing.Inc()
		}
		ex.Add(slog.String("error", "balance hash missing"))
		return envelope.Error(contract.ErrorCodeServiceUnavailable,
			"the balance is not available to enforce against").Retry(retryShortly)
	}
	if errors.Is(err, store.ErrReadOnly) {
		// A rebuild is in progress and this instance writes no balance until
		// it is restarted in normal mode (IP-11 D5). The client's remedy is
		// the same as for a store that did not answer.
		ex.Add(slog.String("error", "read-only mode"))
		return envelope.Error(contract.ErrorCodeServiceUnavailable,
			"the service is in read-only recovery mode; retry with the same Idempotency-Key").Retry(retryShortly)
	}
	ex.Add(slog.String("error", short(err)))
	return envelope.Error(contract.ErrorCodeServiceUnavailable,
		"the data store did not answer; retry with the same Idempotency-Key").Retry(retryShortly)
}

func short(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
