package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestTenantKeyLayoutMatchesTheDataModel pins the layout of data-model.md
// section 3.1 to the byte: five tenant keys and the config namespace, built by
// the single builder. A later key that is added by a second format string is
// the defect this table exists to make visible.
func TestTenantKeyLayoutMatchesTheDataModel(t *testing.T) {
	tenant := "df125e35-7a6d-4a63-9695-889af4db8da9"
	feature := "llm.tokens.output"
	idem := "01JCV3F6G7H8J9K0L1M2N"

	cases := []struct {
		name string
		got  func() (string, error)
		want string
	}{
		{"balance", func() (string, error) { return BalanceKey(tenant, feature) },
			"qc:{t:df125e35-7a6d-4a63-9695-889af4db8da9}:bal:llm.tokens.output"},
		{"idempotency", func() (string, error) { return IdempotencyKey(tenant, idem) },
			"qc:{t:df125e35-7a6d-4a63-9695-889af4db8da9}:idem:01JCV3F6G7H8J9K0L1M2N"},
		{"bucket counters", func() (string, error) { return BucketCounterKey(tenant, feature) },
			"qc:{t:df125e35-7a6d-4a63-9695-889af4db8da9}:f:llm.tokens.output"},
		{"window marker", func() (string, error) { return WindowMarkerKey(tenant, feature) },
			"qc:{t:df125e35-7a6d-4a63-9695-889af4db8da9}:w:llm.tokens.output"},
	}
	for _, tc := range cases {
		got, err := tc.got()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, got, tc.want)
		}
	}
	if ConfigTenantsKey() != "qc:cfg:tenants" || ConfigVersionKey() != "qc:cfg:version" ||
		ConfigInvalidateChannel() != "qc:cfg:invalidate" {
		t.Errorf("config namespace drifted: got %q, %q, %q",
			ConfigTenantsKey(), ConfigVersionKey(), ConfigInvalidateChannel())
	}
}

// TestHashTagBoundaries asserts the property data-model.md section 3.1 says
// getting wrong silently splits a tenant across slots: the tag is exactly the
// tenant, two tenants that share a leading prefix carry distinct tags, and
// every tenant key for one tenant carries the identical tag so a multi-key
// script is single-slot.
func TestHashTagBoundaries(t *testing.T) {
	aa := "df125e35-0000-4000-8000-000000000001"
	ab := "df125e35-0000-4000-8000-000000000002"
	for _, fixture := range []string{aa, ab} {
		if _, err := uuid.Parse(fixture); err != nil {
			t.Fatalf("fixture %q is not a UUID: %v", fixture, err)
		}
	}

	first, err := BalanceKey(aa, "a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := IdempotencyKey(aa, "b")
	if err != nil {
		t.Fatal(err)
	}
	other, err := BalanceKey(ab, "a")
	if err != nil {
		t.Fatal(err)
	}

	tag := first[strings.Index(first, "{") : strings.Index(first, "}")+1]
	if tag != HashTag(aa) {
		t.Errorf("tag = %q, want %q", tag, HashTag(aa))
	}
	if otherTag := other[strings.Index(other, "{") : strings.Index(other, "}")+1]; otherTag == tag {
		t.Errorf("tenants whose UUIDs share a prefix must not share a tag: %q vs %q", otherTag, tag)
	}
	if second[strings.Index(second, "{"):strings.Index(second, "}")+1] != tag {
		t.Errorf("two keys of one tenant must carry the same tag, got %q and %q", tag, second)
	}
}

// TestKeyBuilderRejectsUnsafeParts asserts the fail-closed half of the single
// builder: a component that could open a second hash tag or inject a path is a
// refused key, never a malformed one (request-lifecycle.md section 6).
func TestKeyBuilderRejectsUnsafeParts(t *testing.T) {
	tenant := "df125e35-7a6d-4a63-9695-889af4db8da9"
	for _, name := range []string{"", "a{b", "a}b", "a:b", "{"} {
		if _, err := BalanceKey(tenant, name); !errors.Is(err, ErrBadKeyPart) {
			t.Errorf("BalanceKey(%q) err = %v, want ErrBadKeyPart", name, err)
		}
		if _, err := IdempotencyKey(tenant, name); !errors.Is(err, ErrBadKeyPart) {
			t.Errorf("IdempotencyKey(%q) err = %v, want ErrBadKeyPart", name, err)
		}
	}
	for _, bad := range []string{"", "not-a-uuid", "01JCV3A8B9C0D1E2F3G4H5J:extra", "{x}"} {
		if _, err := BalanceKey(bad, "f"); !errors.Is(err, ErrBadKeyPart) {
			t.Errorf("BalanceKey(tenant=%q) err = %v, want ErrBadKeyPart", bad, err)
		}
	}
	if _, err := TenantKey(tenant, KeyKind("b:ad"), "f"); !errors.Is(err, ErrBadKeyPart) {
		t.Errorf("a kind containing a colon must be refused, got %v", err)
	}
}

// TestKeyExpiryMatchesTheWindowRule asserts the TTL column of the layout table
// (data-model.md section 3.1): window_end + 24h, and none for a `never`
// entitlement. The PTTL half of the same rule is asserted inside the scripts in
// IP-05; this is the Go side.
func TestKeyExpiryMatchesTheWindowRule(t *testing.T) {
	end := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	got, ok := KeyExpiry(&end)
	if !ok {
		t.Fatal("a bounded window must have an expiry")
	}
	want := end.Add(24 * time.Hour)
	if !got.Equal(want) {
		t.Errorf("KeyExpiry = %v, want %v", got, want)
	}
	if _, ok := KeyExpiry(nil); ok {
		t.Error("a `never` entitlement must have no expiry (DR-008)")
	}
}
