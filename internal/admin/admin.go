// Package admin is the control-plane API: the /v1/admin routes IP-09 serves,
// their decoding, rendering and pagination, and the per-client-address rate
// limit in front of them. It reaches Postgres only through Store, which
// internal/db implements, so the source check that keeps every package but
// internal/db off the control-plane connection covers it unchanged. Every
// write is one transaction with its audit row (DR-041, NFR-D4), propagated
// after commit (request-lifecycle.md section 4).
package admin

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/auth"
	"github.com/quotacore/quotacore/internal/store"
)

// Actor is who an audit row names: the calling key's id and name, and the
// request that made the change. The bootstrap has a name and no key id.
type Actor struct {
	KeyID     string
	Name      string
	RequestID string
}

// Position is where a page ends, newest first: the row's creation instant and
// a tiebreaker (its id, or the ledger's sequence), so a cursor is never
// ambiguous (api-conventions.md section 9).
type Position struct {
	At  time.Time
	Tie string
}

// Page asks for at most Limit rows after a position. A Store returns up to
// Limit+1, and the extra row only says that another page exists.
type Page struct {
	Limit int
	After *Position
}

// NewKey is a key the route has already minted: only the prefix and the
// digest reach the store (ADR-0018).
type NewKey struct {
	Name      string
	Scope     auth.Scope
	ExpiresAt *time.Time
	Prefix    string
	Hash      []byte
}

type NewFeature struct {
	Key, Name, Unit, Description string
}

// FeatureChange is a feature PATCH: nil leaves a field as it is.
type FeatureChange struct {
	Version     int64
	Name        *string
	Description *string
	Unit        *string
}

type NewPlan struct {
	Key, Name, Description string
	Entitlements           []Entitlement
}

// Entitlement is one row of a plan: a feature, its limit and its cadence.
type Entitlement struct {
	Feature  string
	Limit    int64
	Interval string
}

type NewTenant struct {
	ExternalID, DisplayName, PlanKey, Timezone string
	Metadata                                   []byte // a JSON object
	Anchor                                     time.Time
}

// TenantChange is a tenant PATCH of the fields IP-09 serves. A timezone
// change does not re-anchor the cycle (T-6, DR-033).
type TenantChange struct {
	Version     int64
	DisplayName *string
	Timezone    *string
	Metadata    []byte // nil leaves it as it is
}

// Opening is what a new tenant's balance hashes are written from, inside the
// transaction that inserts it and before the commit (S3).
type Opening struct {
	TenantID     string
	Anchor       time.Time
	Timezone     string
	Entitlements []Entitlement // the plan's non-archived entitlements
}

type AuditFilter struct {
	TenantExternalID string
	From, To         *time.Time
}

type EventFilter struct {
	FeatureKey, EventType, Source, ClientKey string
	From, To                                 *time.Time
}

// Event is one ledger row as the events route renders it, before its cycle
// window is computed. Interval is the cadence the tenant's plan or override
// holds for the feature now, empty when it holds none.
type Event struct {
	contract.UsageEvent
	Seq      int64
	Index    int64
	Interval string
}

// EventPage is a page of one tenant's ledger, with the anchor and zone the
// windows are computed from (S10).
type EventPage struct {
	Anchor   time.Time
	Timezone string
	Events   []Event
}

// Store is the control plane as the admin routes need it. Every write records
// its audit row in the same transaction, and every refusal is an error whose
// catalogue code the route answers with (see Coded).
type Store interface {
	ListKeys(ctx context.Context, p Page) ([]contract.Key, error)
	CreateKey(ctx context.Context, a Actor, k NewKey) (contract.Key, error)
	// RevokeKey returns the key's prefix and whether this call revoked it;
	// false for a key already revoked, which writes no audit row.
	RevokeKey(ctx context.Context, a Actor, id string, at time.Time) (string, bool, error)

	ListFeatures(ctx context.Context, p Page) ([]contract.Feature, error)
	GetFeature(ctx context.Context, key string) (contract.Feature, error)
	CreateFeature(ctx context.Context, a Actor, f NewFeature) (contract.Feature, error)
	UpdateFeature(ctx context.Context, a Actor, key string, c FeatureChange) (contract.Feature, error)
	// ArchiveFeature returns the tenants whose snapshot the archive changes.
	ArchiveFeature(ctx context.Context, a Actor, key string, version int64, reason string) (contract.Feature, []string, error)
	DeleteFeature(ctx context.Context, a Actor, key, reason string) error

	ListPlans(ctx context.Context, p Page) ([]contract.Plan, error)
	GetPlan(ctx context.Context, id string) (contract.Plan, error)
	CreatePlan(ctx context.Context, a Actor, p NewPlan) (contract.Plan, error)
	ArchivePlan(ctx context.Context, a Actor, id string, version int64, reason string) (contract.Plan, error)
	DeletePlan(ctx context.Context, a Actor, id, reason string) error

	ListTenants(ctx context.Context, p Page, planKey string) ([]contract.Tenant, error)
	GetTenant(ctx context.Context, id string) (contract.Tenant, error)
	CreateTenant(ctx context.Context, a Actor, t NewTenant, open func(context.Context, Opening) error) (contract.Tenant, error)
	UpdateTenant(ctx context.Context, a Actor, id string, c TenantChange) (contract.Tenant, error)
	// SetTenantState is suspend (to "suspended") and resume (to "active").
	SetTenantState(ctx context.Context, a Actor, id string, version int64, reason, to string) (contract.Tenant, error)
	DeleteTenant(ctx context.Context, a Actor, id, reason string, at time.Time) (contract.Tenant, error)

	ListAudit(ctx context.Context, f AuditFilter, p Page) ([]contract.AuditEntry, error)
	ListEvents(ctx context.Context, tenantID string, f EventFilter, p Page) (EventPage, error)

	Ping(ctx context.Context) error
}

// Coded is a refusal that names its catalogue code; internal/db's Error
// satisfies it. Field names the request field of a validation_failed, and
// Current is the stored version of a resource_version_conflict.
type Coded interface {
	error
	CatalogueCode() contract.ErrorCode
	CatalogueField() string
	CurrentVersion() (int64, bool)
}

// Data is the fast store as the admin routes need it: the balance hashes a
// new tenant starts with, the propagation of a committed write, and the key
// that signs cursors. internal/store's Store satisfies it.
type Data interface {
	Provision(ctx context.Context, tenantID string, openings []store.Opening) error
	Publish(ctx context.Context, tenantIDs []string) (int64, error)
	CursorKey(ctx context.Context) ([]byte, error)
	Ping(ctx context.Context) error
}

// KeyCache is the serving instance's key cache. A key issued or revoked is
// dropped from it before the response, so this instance's next request with
// that key reads the committed row (K-2, S6).
type KeyCache interface {
	Forget(prefix string)
}

// Options wire a Service.
type Options struct {
	Store  Store
	Data   Data
	Keys   KeyCache
	Logger *slog.Logger
	// MetadataMax is QUOTACORE_METADATA_MAX_BYTES, the cap on a tenant's
	// metadata (413 above it).
	MetadataMax int
	// PublishTimeout bounds the propagation after a commit, which is
	// best-effort: the refresh tick converges a missed publish.
	PublishTimeout time.Duration
	Status         StatusInfo
}

// StatusInfo is what GET /v1/admin/status reports beyond its probes.
type StatusInfo struct {
	Version, Commit string
	Started         time.Time
	// Cache reads the snapshot's size; nil reports zeros.
	Cache func() contract.CacheStats
	// ProbeTimeout bounds each dependency probe (S9).
	ProbeTimeout time.Duration
}

// Service serves the admin routes.
type Service struct {
	store       Store
	data        Data
	keys        KeyCache
	log         *slog.Logger
	metadataMax int
	pubTimeout  time.Duration
	status      StatusInfo
	sign        signer
}

func New(opts Options) *Service {
	s := &Service{
		store:       opts.Store,
		data:        opts.Data,
		keys:        opts.Keys,
		log:         opts.Logger,
		metadataMax: opts.MetadataMax,
		pubTimeout:  opts.PublishTimeout,
		status:      opts.Status,
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.metadataMax <= 0 {
		s.metadataMax = 4096
	}
	if s.pubTimeout <= 0 {
		s.pubTimeout = 2 * time.Second
	}
	if s.status.ProbeTimeout <= 0 {
		s.status.ProbeTimeout = time.Second
	}
	return s
}

// Handlers maps each operation IP-09 serves to its handler, by operationId.
// An admin operation missing from this map is IP-10's, and the router keeps
// answering it with a bare 404 until IP-10 serves it.
func (s *Service) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"listKeys":  s.listKeys,
		"createKey": s.createKey,
		"revokeKey": s.revokeKey,

		"listFeatures":   s.listFeatures,
		"createFeature":  s.createFeature,
		"getFeature":     s.getFeature,
		"updateFeature":  s.updateFeature,
		"archiveFeature": s.archiveFeature,
		"deleteFeature":  s.deleteFeature,

		"listPlans":   s.listPlans,
		"createPlan":  s.createPlan,
		"getPlan":     s.getPlan,
		"archivePlan": s.archivePlan,
		"deletePlan":  s.deletePlan,

		"listTenants":   s.listTenants,
		"createTenant":  s.createTenant,
		"getTenant":     s.getTenant,
		"updateTenant":  s.updateTenant,
		"suspendTenant": s.suspendTenant,
		"resumeTenant":  s.resumeTenant,
		"deleteTenant":  s.deleteTenant,

		"listUsageEvents": s.listEvents,
		"listAudit":       s.listAudit,
		"status":          s.serveStatus,
	}
}

// publish propagates a committed write. It is best-effort and never fails the
// request: the write is committed, and the refresh tick converges a missed
// message within its interval (request-lifecycle.md section 4).
func (s *Service) publish(r *http.Request, tenants []string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.pubTimeout)
	defer cancel()
	if _, err := s.data.Publish(ctx, tenants); err != nil {
		s.log.Warn("could not publish a configuration change; the refresh tick will converge it",
			"error", err)
	}
}
