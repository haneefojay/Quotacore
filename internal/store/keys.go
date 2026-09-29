// Package store is the data plane's slice of the fast datastore: the key
// layout that data-model.md section 3.1 specifies, the client and its pool,
// and the invalidation channel. IP-04 owns the keyspace and the connection
// pools; the Lua scripts that decide a balance are IP-05's, and this package
// deliberately contains neither them nor their vocabulary.
//
// Every runtime key carries a per-tenant hash tag so all of one tenant's keys
// occupy one slot, which is what makes a multi-key script single-slot (ADR-0002,
// A-11). The tag is the literal `{t:<tenant_id>}` segment and the tenant
// identifier is a UUID, so the builder below is the single tested function
// that assembles keys (data-model.md section 3.1): a key built by any other
// path is a key whose tag can silently split the tenant's state across slots.
package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// namespace is the product-wide prefix required by data-model.md section 3.1.
// The `qc` is deliberate and global; no other component of the key is a
// product name.
const namespace = "qc"

// KeyKind is the `:kind:` segment of a tenant key. It is a closed set; the
// four runtime kinds and nothing else.
type KeyKind string

// The four runtime key kinds. The `f` and `w` kinds exist in v0.1 with no
// writers because the rolling-window design is fixed before the keyspace is
// frozen (data-model.md, ADR-0008); the balance and idempotency kinds are used
// by the scripts from IP-05 and IP-06.
const (
	KindBalance     KeyKind = "bal"
	KindIdempotency KeyKind = "idem"
	KindBuckets     KeyKind = "f"
	KindWindow      KeyKind = "w"
)

// ErrBadKeyPart is returned when a key component would break the hash tag or
// the key grammar. A feature key, an idempotency key or a tenant identifier
// containing a brace or a colon is refused here so that a key injection cannot
// reach the datastore, and so that a brace in a name cannot open a second tag
// that splits the tenant's slot (request-lifecycle.md section 6).
var ErrBadKeyPart = errors.New("key component would break the hash tag")

const tagOpen, tagClose, colon = "{", "}", ":"

// TenantKey builds `qc:{t:<tenant_id>}:<kind>:<name>`. It is the one function
// that assembles a tenant key, and it is the only thing that may. The tag is
// the literal `{t:<tenant_id>}` segment, so two tenants that would otherwise
// share a slot are kept apart by the segment that names them.
func TenantKey(tenantID string, kind KeyKind, name string) (string, error) {
	if _, err := uuid.Parse(tenantID); err != nil {
		return "", fmt.Errorf("%w: tenant id is not a UUID", ErrBadKeyPart)
	}
	if kind == "" || strings.IndexByte(string(kind), ':') >= 0 {
		return "", fmt.Errorf("%w: kind %q", ErrBadKeyPart, kind)
	}
	if err := checkName(name); err != nil {
		return "", err
	}
	return namespace + ":{t:" + tenantID + "}:" + string(kind) + ":" + name, nil
}

// BalanceKey returns the balance hash key for a tenant and feature, which is
// the runtime key the whole product exists around (data-model.md section 3.2).
func BalanceKey(tenantID, featureKey string) (string, error) {
	return TenantKey(tenantID, KindBalance, featureKey)
}

// IdempotencyKey returns the idempotency record key for a tenant and client
// key. It sits inside the tenant's hash slot so that the script that writes
// the balance and the record is single-slot (ADR-0002).
func IdempotencyKey(tenantID, clientKey string) (string, error) {
	return TenantKey(tenantID, KindIdempotency, clientKey)
}

// BucketCounterKey returns the rolling-window bucket key (`:f:`), a v0.2 key
// that IP-19 will write. It is in the layout now because the keyspace is being
// frozen before it is used (ADR-0008), and a window kind added later would be
// a keyspace change.
func BucketCounterKey(tenantID, featureKey string) (string, error) {
	return TenantKey(tenantID, KindBuckets, featureKey)
}

// WindowMarkerKey returns the rolling-window boundary marker key (`:w:), the
// companion to BucketCounterKey. Both are read-only in v0.1.
func WindowMarkerKey(tenantID, featureKey string) (string, error) {
	return TenantKey(tenantID, KindWindow, featureKey)
}

// ConfigTenantsKey is `qc:cfg:tenants`, the hash of tenant to snapshot version
// that the invalidation subscriber diffs against.
func ConfigTenantsKey() string { return namespace + ":cfg:tenants" }

// ConfigVersionKey is `qc:cfg:version`, the monotonically increasing
// configuration version.
func ConfigVersionKey() string { return namespace + ":cfg:version" }

// ConfigInvalidateChannel is the pub/sub channel a committed control-plane
// write publishes to (data-model.md section 3.4).
func ConfigInvalidateChannel() string { return namespace + ":cfg:invalidate" }

// KeyExpiry returns the absolute expiry of a runtime key: `window_end + 24h`,
// which is strictly later than any rollover could need it (DR-048, INV-X6),
// and none for a `never` entitlement. It mirrors cycle.Window in shape: a nil
// end means the key outlives every cycle. The scripts assert the PTTL side of
// this in IP-05; this function is the Go side of the same rule.
func KeyExpiry(end *time.Time) (exp time.Time, ok bool) {
	if end == nil {
		return time.Time{}, false
	}
	return end.Add(24 * time.Hour), true
}

// HashTag extracts the literal `{t:...}` region of a key that TenantKey built.
// The single test that proves the tag boundaries compares this to the tenant
// id passed in, which is how a key whose tag is not exactly the tenant is
// caught instead of silently splitting the tenant across slots.
func HashTag(tenantID string) string { return tagOpen + "t:" + tenantID + tagClose }

func checkName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty name", ErrBadKeyPart)
	}
	if strings.IndexAny(name, "{}:") >= 0 {
		return fmt.Errorf("%w: name %q contains a brace or a colon", ErrBadKeyPart, name)
	}
	return nil
}
