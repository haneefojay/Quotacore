package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/quotacore/quotacore/api"
)

// fixture is one plan with one entitlement to one feature, built with plain
// SQL: the constraints are what is under test, not a create path, which is
// IP-09's.
type fixture struct {
	db                       *sql.DB
	planID, featureID, entID string
}

func newFixture(t *testing.T, db *sql.DB, planKey, featureKey string) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{db: db}
	if err := db.QueryRowContext(ctx, "INSERT INTO plans (key, name) VALUES ($1, 'P') RETURNING id", planKey).Scan(&f.planID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "INSERT INTO features (key, name, unit) VALUES ($1, 'F', 'token') RETURNING id", featureKey).Scan(&f.featureID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO plan_entitlements (plan_id, feature_id, limit_value, interval)
		VALUES ($1, $2, 100, 'monthly') RETURNING id`, f.planID, f.featureID).Scan(&f.entID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fixture) tenant(t *testing.T, externalID string) string {
	t.Helper()
	var id string
	if err := f.db.QueryRowContext(context.Background(), `INSERT INTO tenants (external_id, display_name, plan_id, anchor_at)
		VALUES ($1, 'T', $2, now()) RETURNING id`, externalID, f.planID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func wantCode(t *testing.T, err error, sentinel *Error) {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want %s", err, sentinel.Code)
	}
}

// TestASecondPlanIsUnrepresentable is DoD 3 (DR-011). The schema is the
// proof: the only paths into plans are tenants.plan_id, one NOT NULL column,
// and the entitlement table, so there is nowhere to write a second plan for a
// tenant. The behavioural half refuses the representable attempts.
func TestASecondPlanIsUnrepresentable(t *testing.T) {
	db := migratedDatabase(t)
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, `SELECT c.relname, a.attname, a.attnotnull, array_length(k.conkey, 1)
		FROM pg_constraint k
		JOIN pg_class c ON c.oid = k.conrelid
		JOIN pg_attribute a ON a.attrelid = k.conrelid AND a.attnum = k.conkey[1]
		WHERE k.contype = 'f' AND k.confrelid = 'plans'::regclass ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for rows.Next() {
		var table, column string
		var notNull bool
		var width int
		if err := rows.Scan(&table, &column, &notNull, &width); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, table+"."+column)
		if table == "tenants" && (!notNull || width != 1) {
			t.Errorf("tenants.%s must be a single NOT NULL column (not null %v, width %d)", column, notNull, width)
		}
	}
	rows.Close()
	if got := strings.Join(paths, ","); got != "plan_entitlements.plan_id,tenants.plan_id" {
		t.Errorf("foreign keys into plans are %s; a second path from a tenant to a plan would make two plans representable", got)
	}

	f := newFixture(t, db, "pro", "llm.tokens")
	f.tenant(t, "acme-1")
	var other string
	if err := db.QueryRowContext(ctx, "INSERT INTO plans (key, name) VALUES ('team', 'Team') RETURNING id").Scan(&other); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO tenants (external_id, display_name, plan_id, anchor_at) VALUES ('acme-1', 'T', $1, now())`, other)
	wantCode(t, Classify(OpWrite, err), ErrTenantExists)
	_, err = db.ExecContext(ctx, `INSERT INTO tenants (external_id, display_name, plan_id, anchor_at) VALUES ('acme-2', 'T', NULL, now())`)
	if e := Classify(OpWrite, err); !errors.Is(e, &Error{Code: api.ErrorCodeValidationFailed, Field: "plan_id"}) {
		t.Errorf("a tenant with no plan: got %v, want validation_failed naming plan_id", e)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO tenants (external_id, display_name, plan_id, anchor_at) VALUES ('acme-3', 'T', gen_random_uuid(), now())`)
	wantCode(t, Classify(OpWrite, err), ErrPlanNotFound)
}

// TestDeletingAnAssignedPlanIsPlanInUse is DoD 4 (DR-035, P-6).
func TestDeletingAnAssignedPlanIsPlanInUse(t *testing.T) {
	db := migratedDatabase(t)
	ctx := context.Background()

	active := newFixture(t, db, "assigned", "feature.a")
	active.tenant(t, "tenant-active")
	wantCode(t, DeletePlan(ctx, db, active.planID), ErrPlanInUse)
	if n := count(t, db, "SELECT count(*) FROM plan_entitlements WHERE plan_id = $1", active.planID); n != 1 {
		t.Errorf("a refused delete left %d entitlements, want the 1 it started with", n)
	}

	deleted := newFixture(t, db, "assigned-to-deleted", "feature.b")
	id := deleted.tenant(t, "tenant-deleted")
	if _, err := db.ExecContext(ctx, "UPDATE tenants SET state = 'deleted', deleted_at = now() WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	wantCode(t, DeletePlan(ctx, db, deleted.planID), ErrPlanInUse)

	unassigned := newFixture(t, db, "unassigned", "feature.c")
	if err := DeletePlan(ctx, db, unassigned.planID); err != nil {
		t.Fatalf("an unassigned plan with an entitlement must delete cleanly: %v", err)
	}
	if n := count(t, db, "SELECT count(*) FROM plans WHERE id = $1", unassigned.planID); n != 0 {
		t.Error("the unassigned plan is still there")
	}
	wantCode(t, DeletePlan(ctx, db, unassigned.planID), ErrPlanNotFound)
}

// TestDeletingAReferencedFeatureIsFeatureInUse is F-6 and DR-016.
func TestDeletingAReferencedFeatureIsFeatureInUse(t *testing.T) {
	db := migratedDatabase(t)
	ctx := context.Background()
	f := newFixture(t, db, "pro", "used.feature")
	wantCode(t, DeleteFeature(ctx, db, f.featureID), ErrFeatureInUse)

	var free string
	if err := db.QueryRowContext(ctx, "INSERT INTO features (key, name, unit) VALUES ('free.feature', 'F', 'token') RETURNING id").Scan(&free); err != nil {
		t.Fatal(err)
	}
	if err := DeleteFeature(ctx, db, free); err != nil {
		t.Fatalf("an unreferenced feature must delete: %v", err)
	}
	wantCode(t, DeleteFeature(ctx, db, free), ErrFeatureNotFound)
}

// TestUnitIsImmutableOnceAssigned is DoD 5 (INV-F2): each kind of reference
// on its own freezes the unit, and with none the unit changes.
func TestUnitIsImmutableOnceAssigned(t *testing.T) {
	db := migratedDatabase(t)
	ctx := context.Background()
	newFeature := func(key string) string {
		var id string
		if err := db.QueryRowContext(ctx, "INSERT INTO features (key, name, unit) VALUES ($1, 'F', 'token') RETURNING id", key).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	free := newFeature("free")
	if err := SetFeatureUnit(ctx, db, free, "request"); err != nil {
		t.Fatalf("an unreferenced feature's unit must change: %v", err)
	}
	var unit string
	var version int64
	if err := db.QueryRowContext(ctx, "SELECT unit, version FROM features WHERE id = $1", free).Scan(&unit, &version); err != nil {
		t.Fatal(err)
	}
	if unit != "request" || version != 2 {
		t.Errorf("after the change: unit %q version %d, want request and 2", unit, version)
	}

	f := newFixture(t, db, "pro", "in.plan")
	wantCode(t, SetFeatureUnit(ctx, db, f.featureID, "request"), ErrFeatureUnitImmutable)
	if err := SetFeatureUnit(ctx, db, f.featureID, "token"); err != nil {
		t.Errorf("setting the unit it already has is not a change: %v", err)
	}

	overridden := newFeature("overridden")
	tenant := f.tenant(t, "t-override")
	if _, err := db.ExecContext(ctx, `INSERT INTO tenant_entitlement_overrides (tenant_id, feature_id, limit_value) VALUES ($1, $2, 5)`, tenant, overridden); err != nil {
		t.Fatal(err)
	}
	wantCode(t, SetFeatureUnit(ctx, db, overridden, "request"), ErrFeatureUnitImmutable)

	historical := newFeature("historical")
	if _, err := db.ExecContext(ctx, `INSERT INTO usage_events (tenant_id, feature_id, cycle_index, delta, balance_after, source, event_type, request_id)
		VALUES ($1, $2, 0, -1, 9, 'api', 'consumed', '01J8Z6M2QK9V3X7B4C0N5TDEFA')`, tenant, historical); err != nil {
		t.Fatal(err)
	}
	wantCode(t, SetFeatureUnit(ctx, db, historical, "request"), ErrFeatureUnitImmutable)

	wantCode(t, SetFeatureUnit(ctx, db, "00000000-0000-4000-8000-000000000000", "request"), ErrFeatureNotFound)
	if e := SetFeatureUnit(ctx, db, free, "Not A Unit"); !errors.Is(e, &Error{Code: api.ErrorCodeValidationFailed, Field: "unit"}) {
		t.Errorf("a malformed unit: got %v, want validation_failed naming unit", e)
	}
}

// externalIDCorpus is DoD 7's adversarial set: what an identifier may be, and
// the shapes personal data arrives in.
var externalIDCorpus = []struct {
	id    string
	valid bool
}{
	{"acme-prod-01", true},
	{"org:42", true},
	{"a.b_c-d:e", true},
	{strings.Repeat("x", 128), true},
	{"jane@example.com", false},
	{"a@b", false},
	{"@", false},
	{"", false},
	{strings.Repeat("x", 129), false},
	{"jane doe", false},
	{"josé", false},
	{"tenant\n1", false},
	{"+44 20 7946 0000", false},
	{"name/surname", false},
}

// TestExternalIdCannotCarryPII is DoD 7 at the database: an email, an '@' on
// its own, a space, a non-ASCII letter or an oversized value is refused by
// the constraint, and the refusal does not repeat the value it refused.
func TestExternalIdCannotCarryPII(t *testing.T) {
	db := migratedDatabase(t)
	ctx := context.Background()
	f := newFixture(t, db, "pro", "llm.tokens")
	for _, c := range externalIDCorpus {
		_, err := db.ExecContext(ctx, `INSERT INTO tenants (external_id, display_name, plan_id, anchor_at) VALUES ($1, 'T', $2, now())`, c.id, f.planID)
		stored := err == nil
		if stored != c.valid {
			t.Errorf("external_id %q: stored %v, want %v (%v)", c.id, stored, c.valid, err)
			continue
		}
		if c.valid {
			continue
		}
		classified := Classify(OpWrite, err)
		if !errors.Is(classified, &Error{Code: api.ErrorCodeValidationFailed, Field: "external_id"}) {
			t.Errorf("external_id %q: classified %v, want validation_failed naming external_id", c.id, classified)
		}
		if c.id != "" && strings.Contains(classified.Error(), c.id) {
			t.Errorf("the refusal of %q repeats the value: %q", c.id, classified.Error())
		}
	}
}

// TestExternalIdValidatorAgreesWithTheConstraint holds the three statements
// of DR-034 to one answer: the Go validator the API boundary calls, the
// constraint, and openapi's pattern.
func TestExternalIdValidatorAgreesWithTheConstraint(t *testing.T) {
	for _, c := range externalIDCorpus {
		if got := ValidExternalID(c.id); got != c.valid {
			t.Errorf("ValidExternalID(%q) = %v, want %v", c.id, got, c.valid)
		}
	}
	contract := repoFile(t, "api/openapi.yaml")
	i := strings.Index(contract, "\n    ExternalId:\n")
	if i < 0 {
		t.Fatal("openapi.yaml has no ExternalId schema")
	}
	if !strings.Contains(contract[i:i+800], "pattern: '"+externalID.String()+"'") {
		t.Errorf("openapi's ExternalId pattern is not %s", externalID.String())
	}
	model := repoFile(t, "docs/architecture/data-model.md")
	if !strings.Contains(model, "CHECK (external_id ~ '"+externalID.String()+"')") {
		t.Errorf("data-model.md's tenants_external_id_format is not %s", externalID.String())
	}
}

// TestEveryConstraintIsClassified keeps Classify complete: every constraint
// and unique index the migrations create has a rule, and every rule names
// something the migrations create.
func TestEveryConstraintIsClassified(t *testing.T) {
	db := migratedDatabase(t)
	rows, err := db.QueryContext(context.Background(), `
		SELECT k.conname FROM pg_constraint k JOIN pg_class c ON c.oid = k.conrelid
		 WHERE c.relnamespace = 'public'::regnamespace
		   AND c.relname NOT IN ('goose_db_version', 'quotacore_migration_checksums')
		UNION
		SELECT i.relname FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid JOIN pg_class c ON c.oid = x.indrelid
		 WHERE x.indisunique AND c.relnamespace = 'public'::regnamespace
		   AND c.relname NOT IN ('goose_db_version', 'quotacore_migration_checksums')
		   AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = x.indexrelid)`)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		seen[name] = true
		if _, ok := constraints[name]; !ok {
			t.Errorf("constraint %s has no rule in Classify", name)
		}
	}
	rows.Close()
	for name := range constraints {
		if !seen[name] {
			t.Errorf("Classify has a rule for %s, which the schema does not create", name)
		}
	}
}

// TestEveryClassifiedCodeIsInTheCatalogue holds every code Classify can
// answer to the generated contract, so a misspelt or retired code fails here
// rather than reaching a client.
func TestEveryClassifiedCodeIsInTheCatalogue(t *testing.T) {
	valid := map[api.ErrorCode]bool{}
	for _, c := range api.ErrorCode("").AllValues() {
		valid[c] = true
	}
	for name, r := range constraints {
		for _, e := range []*Error{r.err, r.missing} {
			if e != nil && !valid[e.Code] {
				t.Errorf("%s answers %q, which is not a catalogue code", name, e.Code)
			}
		}
	}
	for _, e := range []*Error{ErrPlanInUse, ErrFeatureInUse, ErrFeatureUnitImmutable, ErrDuplicateEvent, ErrZeroDelta} {
		if !valid[e.Code] {
			t.Errorf("%v is not a catalogue code", e)
		}
	}
}

// TestNonDatabaseErrorsAreNotRefusals: a lost connection is not a refusal,
// so Classify returns it unchanged for the caller to answer as unavailable.
func TestNonDatabaseErrorsAreNotRefusals(t *testing.T) {
	lost := errors.New("connection reset")
	if got := Classify(OpWrite, lost); got != lost {
		t.Errorf("Classify changed a non-database error: %v", got)
	}
	if Classify(OpWrite, nil) != nil {
		t.Error("Classify(nil) is not nil")
	}
}

// TestTheLedgerAndAuditConstraintsRefuseWhatTheyShould covers the
// constraints IP-12 and IP-09 will lean on: a zero delta, a duplicate
// consumed key, and an unknown audit subject.
func TestTheLedgerAndAuditConstraintsRefuseWhatTheyShould(t *testing.T) {
	db := migratedDatabase(t)
	ctx := context.Background()
	f := newFixture(t, db, "pro", "llm.tokens")
	tenant := f.tenant(t, "acme")
	insert := func(delta int, typ, key string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO usage_events (tenant_id, feature_id, cycle_index, delta, balance_after, source, event_type, request_id, idempotency_key)
			VALUES ($1, $2, 0, $3, 1, 'api', $4, '01J8Z6M2QK9V3X7B4C0N5TDEFA', NULLIF($5, ''))`, tenant, f.featureID, delta, typ, key)
		return Classify(OpWrite, err)
	}
	wantCode(t, insert(0, "consumed", ""), ErrZeroDelta)
	if err := insert(-1, "consumed", "01JCQ9W3K2M9XQ3F8V6T0B5NDE"); err != nil {
		t.Fatal(err)
	}
	wantCode(t, insert(-1, "consumed", "01JCQ9W3K2M9XQ3F8V6T0B5NDE"), ErrDuplicateEvent)
	if err := insert(5, "granted", "01JCQ9W3K2M9XQ3F8V6T0B5NDE"); err != nil {
		t.Errorf("the duplicate detector covers only consumed and refunded: %v", err)
	}
	var first, second int64
	if err := db.QueryRowContext(ctx, "SELECT min(seq), max(seq) FROM usage_events").Scan(&first, &second); err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Errorf("seq does not follow insert order: %d then %d", first, second)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO audit_log (actor, action, subject_type, request_id) VALUES ('bootstrap', 'x.y', 'override', 'r')`)
	wantCode(t, Classify(OpWrite, err), ErrAuditSubject)
	_, err = db.ExecContext(ctx, `INSERT INTO tenant_entitlement_overrides (tenant_id, feature_id) VALUES ($1, $2)`, tenant, f.featureID)
	if e := Classify(OpWrite, err); !errors.Is(e, &Error{Code: api.ErrorCodeValidationFailed, Field: "limit_value"}) {
		t.Errorf("an override that sets nothing: got %v, want validation_failed (api-conventions.md 7.4)", e)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO plan_entitlements (plan_id, feature_id, limit_value, interval) VALUES ($1, $2, 1, 'daily')`, f.planID, f.featureID)
	if e := Classify(OpWrite, err); !errors.Is(e, &Error{Code: api.ErrorCodeValidationFailed, Field: "feature_key"}) {
		t.Errorf("a duplicate entitlement: got %v, want validation_failed naming the feature (INV-E2)", e)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO plan_entitlements (plan_id, feature_id, limit_value, interval) VALUES ($1, $2, 9007199254740992, 'daily')`, f.planID, f.featureID)
	if e := Classify(OpWrite, err); !errors.Is(e, ErrValidation) {
		t.Errorf("a limit above 2^53-1: got %v, want validation_failed", e)
	}
}
