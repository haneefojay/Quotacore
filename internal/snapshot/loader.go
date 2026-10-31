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
	"github.com/quotacore/quotacore/internal/rolling"
)

// Typed miss outcomes. The mapping to the HTTP catalogue is the data plane's
// (IP-07): ErrTenantUnknown → 404 tenant_not_found, ErrMissRefused and
// ErrStoreUnavailable → 503 service_unavailable, ErrRefreshFailed → 503
// control_plane_unavailable. No other error escapes Resolve.
var (
	ErrTenantUnknown = errors.New("snapshot: tenant unknown")
	ErrMissRefused   = errors.New("snapshot: miss refused by the bounded-miss limiter")
	ErrRefreshFailed = errors.New("snapshot: control plane unreachable on a miss")
	ErrStaleSnapshot = errors.New("snapshot: the snapshot predates the configuration, retry")
	// ErrStoreUnavailable is a miss whose data-store read failed: the
	// tenant's version lives in the store, so a dead store is not a
	// control-plane outage, and the read is bounded by the data-store budget
	// (NFR-L4), not by the control plane's.
	ErrStoreUnavailable = errors.New("snapshot: the data store did not answer on a miss")
)

// Loader is the single bounded control-plane read the data plane may perform,
// and all the data plane needs from it (DR-039, and ADR-0001's "no DB on the
// hot path" made structural). IP-08 provides a Postgres-backed implementation.
type Loader interface {
	// Load returns the tenant's effective configuration at the version it had
	// at load time. A tenant that provably does not exist returns ErrTenantUnknown.
	Load(ctx context.Context, tenantID string) (*LoadedTenant, error)
}

// IDResolver maps a customer's external_id to the internal tenant id. It is
// the first of the two statements the first sight of a tenant costs (IP-07,
// decision D3); every later miss of that tenant costs only Load. A tenant that
// provably does not exist, or is deleted, returns ErrTenantUnknown.
type IDResolver interface {
	TenantIDByExternalID(ctx context.Context, externalID string) (string, error)
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
	// Pins are what the tenant was anchored on, by feature: its cadence and
	// its feature set, which change only at a re-anchor (IP-10, D1).
	Pins    map[string]Pin
	Version int64
}

// LoadedPlan is one plan with its entitlements. Plans are interned by the
// snapshot: ten thousand tenants on three plans hold three plan objects.
// Version is the plan's own version, which an entitlement edit bumps, so an
// edited plan is a new interned object rather than a change to a shared one.
type LoadedPlan struct {
	ID           string
	Key          string
	Version      int64
	Entitlements map[string]Entitlement
}

// Pin is one tenant_entitlement_pins row: a feature the tenant was anchored
// on, the cadence its cycle index is counted in, and whether its feature is
// archived. A feature removed from the plan since is retired: RetiredLimit is
// the limit it had and RetiredUntil the end of the window it was removed in,
// after which the tenant no longer has it. A retired `never` feature has no
// window end, and keeps the feature until the next re-anchor (E-4, INV-E1).
type Pin struct {
	Interval     cycle.Interval
	Archived     bool
	RetiredLimit *int64
	RetiredUntil *time.Time
}

// activeAt reports whether the pinned feature is still the tenant's at an
// instant.
func (p Pin) activeAt(at time.Time) bool {
	return p.RetiredLimit == nil || p.RetiredUntil == nil || at.Before(*p.RetiredUntil)
}

// Entitlement is the enforceable content of one plan_entitlements row.
// Archived is the referenced feature's state: an archived feature is refused
// with 403 feature_archived before the tenant's state is considered
// (state-machines.md section 5, step 1), and its balance stays readable.
//
// Boolean marks an inclusion with no counter (DR-050): Limit and Interval are
// then zero and mean nothing, and no balance exists to enforce against.
//
// Rolling, when it is not zero, marks a rolling-window entitlement (DR-052):
// Limit is the window's limit, Interval means nothing, and the units are
// counted in the window's bucket hash, never in a balance hash.
type Entitlement struct {
	Limit    int64
	Interval cycle.Interval
	Archived bool
	Boolean  bool
	Rolling  rolling.Window
}

// Anchored reports an entitlement with a balance hash on an anchored cycle: a
// metered one. A boolean has no counter and a rolling one counts in a window,
// so every path that rolls, rebuilds, reconciles or adjusts a balance skips
// both (DR-050, DR-052).
func (e Entitlement) Anchored() bool { return !e.Boolean && e.Rolling.IsZero() }

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
	// Boolean is true when the entitlement is an inclusion with no counter
	// (DR-050); Limit, Interval and Anchor then mean nothing.
	Boolean bool
	// Rolling is the window of a rolling entitlement (DR-052), zero for any
	// other kind; Limit is then the window's limit and Interval and Anchor
	// mean nothing.
	Rolling rolling.Window
	// Anchor is the tenant's cycle anchor, the one per-tenant cycle state
	// (DR-002), valid when InPlan is true.
	Anchor time.Time
	// Timezone is the tenant's configured zone.
	Timezone string
	// TenantVersion is the configuration version this resolution was made
	// under, for diagnostics only.
	TenantVersion int64
}
