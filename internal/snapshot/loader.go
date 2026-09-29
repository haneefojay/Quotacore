// Package snapshot is the bounded in-process projection of control-plane
// configuration that the data plane resolves against (DR-039,
// request-lifecycle.md section 3). IP-04 owns the cache, its invalidation and
// its bounded-miss discipline; the Loader is an interface because the Postgres
// implementer is IP-08, which reads the schema in data-model.md section 2.
//
// The invariants that matter here are the ones the phase's Definition of Done
// asserts: a cached tenant is resolved without touching anything (the hit path
// does no I/O, so the pool wait it could incur is zero), a miss is bounded by a
// token bucket and performs at most one Loader call (DR-039), and a
// configuration change becomes visible well inside the NFR-D3 second.
package snapshot

import (
	"context"
	"errors"
	"time"

	"github.com/quotacore/quotacore/internal/cycle"
)

// Typed miss outcomes. The mapping to the HTTP catalogue is the data plane's
// (IP-07): ErrTenantUnknown → 404 tenant_not_found, ErrMissRefused →
// 503 service_unavailable, ErrRefreshFailed → 503 control_plane_unavailable.
// No other error escapes Resolve.
var (
	ErrTenantUnknown = errors.New("snapshot: tenant unknown")
	ErrMissRefused   = errors.New("snapshot: miss refused by the bounded-miss limiter")
	ErrRefreshFailed = errors.New("snapshot: control plane unreachable on a miss")
	ErrStaleSnapshot = errors.New("snapshot: the snapshot predates the configuration, retry")
)

// Loader is the single bounded control-plane read the data plane may perform,
// and all the data plane needs from it (DR-039, and ADR-0001's "no DB on the
// hot path" made structural). IP-08 provides a Postgres-backed implementation.
type Loader interface {
	// Load returns the tenant's effective configuration at the version it had
	// at load time. A tenant that provably does not exist returns ErrTenantUnknown.
	Load(ctx context.Context, tenantID string) (*LoadedTenant, error)
}

// LoadedTenant is one tenant's configuration as the data plane needs it,
// projected from features, plans, entitlements, tenants and overrides. Version
// is the value of qc:cfg:tenants[tenant] at read time, which is what staleness
// is compared against during invalidation.
type LoadedTenant struct {
	ID         string
	ExternalID string
	State      string
	Timezone   string
	Anchor     time.Time
	Plan       *LoadedPlan
	Overrides  map[string]LoadedOverride
	Version    int64
}

// LoadedPlan is one plan with its entitlements. Plans are interned by the
// snapshot: ten thousand tenants on three plans hold three plan objects.
type LoadedPlan struct {
	ID           string
	Key          string
	Entitlements map[string]Entitlement
}

// Entitlement is the enforceable content of one plan_entitlements row.
type Entitlement struct {
	Limit    int64
	Interval cycle.Interval
}

// LoadedOverride is one tenant_entitlement_overrides row, resolved to concrete
// values. A nil field means "inherit from the plan".
type LoadedOverride struct {
	Limit    *int64
	Interval *cycle.Interval
}

// Resolution is the outcome of the data plane's `config.resolve` step: what
// the request is allowed to do, decided with no I/O (request-lifecycle.md
// section 5). The handler that consumes it (IP-07) maps the flags to the
// closure errors, but the decision order is made here, once: archived over
// suspended over in-plan over not-in-plan (DR-015).
type Resolution struct {
	// Suspended is true when the tenant is suspended; the feature result is
	// then irrelevant and limit fields are zero.
	Suspended bool
	// Archived is true when the feature is archived (DR-015); the limit fields
	// remain zero even if the plan still lists it.
	Archived bool
	// InPlan is true when the feature resolves to an entitlement, from the
	// tenant's override or the plan's.
	InPlan bool
	// Limit and Interval are the resolved entitlement values, valid only when
	// InPlan is true. Interval Never carries a limit of its own meaning.
	Limit    int64
	Interval cycle.Interval
	// Anchor is the tenant's cycle anchor, the one per-tenant cycle state
	// (DR-002), valid when InPlan is true.
	Anchor time.Time
	// Timezone is the tenant's configured zone.
	Timezone string
	// TenantVersion is the configuration version this resolution was made
	// under, for diagnostics only.
	TenantVersion int64
}
