package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/snapshot"
)

// VersionSource is the slice of the datastore the loader stamps a load with:
// the tenant's entry in qc:cfg:tenants. internal/store's Store satisfies it.
type VersionSource interface {
	TenantVersion(ctx context.Context, tenantID string) (int64, error)
}

// Loader is the Postgres implementation of snapshot.Loader: the single bounded
// control-plane read a cache miss may perform (DR-039). It reads the tenant's
// version first and its rows second. In that order a concurrent change can
// only produce new rows under an old version, which the next prune evicts; the
// other order could cache old rows under the new version, and nothing would
// ever correct them.
type Loader struct {
	db       *sql.DB
	versions VersionSource
}

var _ snapshot.Loader = (*Loader)(nil)

func NewLoader(db *sql.DB, versions VersionSource) *Loader {
	return &Loader{db: db, versions: versions}
}

// loadTenant is one statement, so one round trip and one consistent read: the
// tenant, its plan, the plan's entitlements with each feature's archive
// state, the tenant's overrides, and its pins, the cadence and feature set it
// was anchored on (IP-10). A deleted tenant is not returned (INV-T1), so it
// reads as unknown. lock is empty for the data plane's read and FOR UPDATE OF
// t for an admin write that resolves the tenant it has locked.
const loadTenant = `
SELECT t.id, t.external_id, t.state, t.timezone, t.anchor_at, t.version, p.id, p.key, p.version,
       coalesce((SELECT jsonb_agg(jsonb_build_object(
                    'feature', f.key, 'limit', e.limit_value, 'interval', e.interval,
                    'archived', f.state = 'archived'))
                   FROM plan_entitlements e JOIN features f ON f.id = e.feature_id
                  WHERE e.plan_id = p.id), '[]'::jsonb),
       coalesce((SELECT jsonb_agg(jsonb_build_object(
                    'feature', f.key, 'limit', o.limit_value, 'interval', o.interval))
                   FROM tenant_entitlement_overrides o JOIN features f ON f.id = o.feature_id
                  WHERE o.tenant_id = t.id), '[]'::jsonb),
       coalesce((SELECT jsonb_agg(jsonb_build_object(
                    'feature', f.key, 'interval', n.interval, 'archived', f.state = 'archived',
                    'limit', n.retired_limit, 'until', n.retired_until))
                   FROM tenant_entitlement_pins n JOIN features f ON f.id = n.feature_id
                  WHERE n.tenant_id = t.id), '[]'::jsonb)
  FROM tenants t
  JOIN plans p ON p.id = t.plan_id
 WHERE t.id = $1 AND t.state <> 'deleted'`

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type loadedRow struct {
	Feature  string     `json:"feature"`
	Limit    *int64     `json:"limit"`
	Interval *string    `json:"interval"`
	Archived bool       `json:"archived"`
	Until    *time.Time `json:"until"`
}

// Load implements snapshot.Loader. A tenant that provably does not exist,
// including an identifier that cannot be one, is snapshot.ErrTenantUnknown;
// a failed read of the tenant's version, which lives in the data store, is
// snapshot.ErrStoreUnavailable; any other failure is returned for the
// snapshot to answer as control_plane_unavailable.
func (l *Loader) Load(ctx context.Context, tenantID string) (*snapshot.LoadedTenant, error) {
	if !uuidShape.MatchString(tenantID) {
		return nil, snapshot.ErrTenantUnknown
	}
	version, err := l.versions.TenantVersion(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", snapshot.ErrStoreUnavailable, err)
	}
	t, err := loadTenantRow(ctx, l.db, tenantID, "")
	if err != nil {
		return nil, err
	}
	t.Version = version
	return t, nil
}

// loadTenantRow runs loadTenant, with a lock clause appended, and decodes it.
// Version is the tenant row's own version; the data plane's Load replaces it
// with the propagation version it compares against.
func loadTenantRow(ctx context.Context, q querier, tenantID, lock string) (*snapshot.LoadedTenant, error) {
	var (
		t                            snapshot.LoadedTenant
		plan                         snapshot.LoadedPlan
		anchor                       time.Time
		entsJSON, overJSON, pinsJSON []byte
	)
	err := q.QueryRowContext(ctx, loadTenant+" "+lock, tenantID).Scan(
		&t.ID, &t.ExternalID, &t.State, &t.Timezone, &anchor, &t.Version, &plan.ID, &plan.Key, &plan.Version,
		&entsJSON, &overJSON, &pinsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, snapshot.ErrTenantUnknown
	}
	if err != nil {
		return nil, fmt.Errorf("load tenant: %w", err)
	}
	t.Anchor = anchor.UTC()

	var ents, overs, pins []loadedRow
	if err := json.Unmarshal(entsJSON, &ents); err != nil {
		return nil, fmt.Errorf("decode entitlements: %w", err)
	}
	if err := json.Unmarshal(overJSON, &overs); err != nil {
		return nil, fmt.Errorf("decode overrides: %w", err)
	}
	if err := json.Unmarshal(pinsJSON, &pins); err != nil {
		return nil, fmt.Errorf("decode pins: %w", err)
	}
	plan.Entitlements = make(map[string]snapshot.Entitlement, len(ents))
	for _, e := range ents {
		if e.Limit == nil || e.Interval == nil {
			return nil, fmt.Errorf("entitlement for %s has no limit or interval", e.Feature)
		}
		iv, err := parseInterval(*e.Interval)
		if err != nil {
			return nil, err
		}
		plan.Entitlements[e.Feature] = snapshot.Entitlement{Limit: *e.Limit, Interval: iv, Archived: e.Archived}
	}
	if len(overs) > 0 {
		t.Overrides = make(map[string]snapshot.LoadedOverride, len(overs))
	}
	for _, o := range overs {
		ov := snapshot.LoadedOverride{Limit: o.Limit}
		if o.Interval != nil {
			iv, err := parseInterval(*o.Interval)
			if err != nil {
				return nil, err
			}
			ov.Interval = &iv
		}
		t.Overrides[o.Feature] = ov
	}
	t.Pins = make(map[string]snapshot.Pin, len(pins))
	for _, p := range pins {
		if p.Interval == nil {
			return nil, fmt.Errorf("pin for %s has no interval", p.Feature)
		}
		iv, err := parseInterval(*p.Interval)
		if err != nil {
			return nil, err
		}
		pin := snapshot.Pin{Interval: iv, Archived: p.Archived, RetiredLimit: p.Limit}
		if p.Until != nil {
			until := p.Until.UTC()
			pin.RetiredUntil = &until
		}
		t.Pins[p.Feature] = pin
	}
	t.Plan = &plan
	return &t, nil
}

// parseInterval maps a reset_interval label to the engine's interval. An
// unknown label is refused rather than guessed: enums grow (data-model.md
// section 6), and a binary older than its schema must not enforce a cadence
// it does not know.
func parseInterval(label string) (cycle.Interval, error) {
	for iv := cycle.Hourly; iv <= cycle.Never; iv++ {
		if iv.String() == label {
			return iv, nil
		}
	}
	return 0, fmt.Errorf("reset interval %q is not one this binary knows", label)
}
