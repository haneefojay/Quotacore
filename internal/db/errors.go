package db

import (
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/quotacore/quotacore/api"
)

// Error is a control-plane refusal named by its catalogue code. It is the
// layer where a constraint the database enforces becomes the code
// error-catalog.md assigns it, before any route exists to send it (IP-09 owns
// the routes and applies the status). Field names the request field for a
// validation_failed. Reason distinguishes the refusals that are never shown to
// a customer, which all carry internal_error.
//
// An Error never carries the driver's message, detail or row: PostgreSQL's
// detail for a failed CHECK is the whole failing row, which would put an
// external_id or a tenant's metadata into a log line (NFR-O6, DR-043).
type Error struct {
	Code   api.ErrorCode
	Field  string
	Reason string
	// Current is the stored version a resource_version_conflict lost to.
	Current *int64
}

// CatalogueCode, CatalogueField and CurrentVersion are what the admin routes
// read a refusal by (admin.Coded), since they cannot import this package.
func (e *Error) CatalogueCode() api.ErrorCode { return e.Code }
func (e *Error) CatalogueField() string       { return e.Field }
func (e *Error) CurrentVersion() (int64, bool) {
	if e.Current == nil {
		return 0, false
	}
	return *e.Current, true
}

func (e *Error) Error() string {
	msg := "control plane: " + string(e.Code)
	if e.Field != "" {
		msg += " (" + e.Field + ")"
	}
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg
}

// Is matches a sentinel by code, and by field and reason when the sentinel
// names them, so errors.Is(err, ErrValidation) matches any field.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok || t.Code != e.Code {
		return false
	}
	return (t.Field == "" || t.Field == e.Field) && (t.Reason == "" || t.Reason == e.Reason)
}

var (
	ErrPlanInUse            = &Error{Code: api.ErrorCodePlanInUse}
	ErrFeatureInUse         = &Error{Code: api.ErrorCodeFeatureInUse}
	ErrFeatureUnitImmutable = &Error{Code: api.ErrorCodeFeatureUnitImmutable}
	ErrFeatureKeyExists     = &Error{Code: api.ErrorCodeFeatureKeyExists}
	ErrPlanKeyExists        = &Error{Code: api.ErrorCodePlanKeyExists}
	ErrTenantExists         = &Error{Code: api.ErrorCodeTenantExists}
	ErrPlanNotFound         = &Error{Code: api.ErrorCodePlanNotFound}
	ErrFeatureNotFound      = &Error{Code: api.ErrorCodeFeatureNotFound}
	ErrTenantNotFound       = &Error{Code: api.ErrorCodeTenantNotFound}
	ErrValidation           = &Error{Code: api.ErrorCodeValidationFailed}
	ErrTenantDeleted        = &Error{Code: api.ErrorCodeTenantDeleted}
	ErrKeyNotFound          = &Error{Code: api.ErrorCodeAPIKeyNotFound}
	ErrVersionConflict      = &Error{Code: api.ErrorCodeResourceVersionConflict}

	// The refusals below are never customer-visible. A duplicate event is the
	// asynchronous detector of DR-030 firing after the data plane already
	// prevented the charge; IP-12 counts it. A zero delta and a broken audit
	// subject are writer bugs. A prefix collision is retried by the issuer.
	ErrDuplicateEvent  = &Error{Code: api.ErrorCodeInternalError, Reason: "duplicate usage event"}
	ErrZeroDelta       = &Error{Code: api.ErrorCodeInternalError, Reason: "usage event with a zero delta"}
	ErrAuditSubject    = &Error{Code: api.ErrorCodeInternalError, Reason: "audit row with an unknown subject type"}
	ErrPrefixCollision = &Error{Code: api.ErrorCodeInternalError, Reason: "api key prefix collision"}
)

// Op is the operation a statement was part of. The same foreign key means
// different things in its two directions — tenants_plan_id_fkey on a plan
// delete is plan_in_use, on a tenant insert it is plan_not_found — and
// PostgreSQL reports the referencing table both ways, so only the caller can
// tell them apart.
type Op int

const (
	OpWrite Op = iota
	OpDeletePlan
	OpDeleteFeature
)

// constraintRule is how one constraint or unique index is answered.
type constraintRule struct {
	err   *Error // the refusal, for a unique or check violation
	field string // the request field, for validation_failed
	// For a foreign key, the refusal when a write references a missing row.
	// The delete direction is decided by the Op.
	missing *Error
}

func validation(field string) constraintRule {
	return constraintRule{err: ErrValidation, field: field}
}

// constraints names every constraint and unique index the migrations create.
// TestEveryConstraintIsClassified fails when one is added without an entry, so
// a new constraint cannot reach a caller as an unnamed driver error.
var constraints = map[string]constraintRule{
	// Primary keys are generated, so a violation is a bug.
	"features_pkey":                     {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},
	"plans_pkey":                        {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},
	"plan_entitlements_pkey":            {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},
	"tenants_pkey":                      {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},
	"tenant_entitlement_overrides_pkey": {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},
	"api_keys_pkey":                     {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},
	"usage_events_pkey":                 {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},
	"audit_log_pkey":                    {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},
	"usage_events_seq_unique":           {err: &Error{Code: api.ErrorCodeInternalError, Reason: "ledger sequence"}},
	// A pin and a pending re-anchor are written by the store from rows it
	// has just read, never from a caller's values, so any refusal is a bug.
	"tenant_entitlement_pins_pkey":          {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},
	"pending_reanchors_pkey":                {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},
	"tenant_pins_retired_limit_range":       {err: &Error{Code: api.ErrorCodeInternalError, Reason: "retired limit"}},
	"tenant_pins_retired_until_needs_limit": {err: &Error{Code: api.ErrorCodeInternalError, Reason: "retired pin"}},
	"pending_reanchors_kind":                {err: &Error{Code: api.ErrorCodeInternalError, Reason: "re-anchor kind"}},
	// A dead letter is written by the ledger writer under the event's own id,
	// and only with one of the two reasons, so any refusal is a bug.
	"usage_event_dead_letters_pkey":   {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},
	"usage_event_dead_letters_reason": {err: &Error{Code: api.ErrorCodeInternalError, Reason: "dead-letter reason"}},
	// Reconciliation's tables and the ledger's loss record (IP-11) are
	// written by the service alone, from values it computed, so any refusal
	// is a bug.
	"usage_event_dead_letters_resolution": {err: &Error{Code: api.ErrorCodeInternalError, Reason: "dead-letter resolution"}},
	"reconcile_findings_pkey":             {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},
	"reconcile_findings_fingerprint":      {err: &Error{Code: api.ErrorCodeInternalError, Reason: "finding fingerprint"}},
	"reconcile_findings_kind":             {err: &Error{Code: api.ErrorCodeInternalError, Reason: "finding kind"}},
	"reconcile_findings_found_by":         {err: &Error{Code: api.ErrorCodeInternalError, Reason: "finding source"}},
	"reconcile_checkpoint_pkey":           {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},
	"reconcile_checkpoint_one_row":        {err: &Error{Code: api.ErrorCodeInternalError, Reason: "checkpoint row"}},
	"usage_event_gaps_pkey":               {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},
	"usage_event_gaps_reason":             {err: &Error{Code: api.ErrorCodeInternalError, Reason: "gap reason"}},
	"usage_event_gaps_events":             {err: &Error{Code: api.ErrorCodeInternalError, Reason: "gap size"}},
	"ledger_sessions_pkey":                {err: &Error{Code: api.ErrorCodeInternalError, Reason: "primary key"}},

	// Unique keys a caller chooses.
	"features_key_key":        {err: ErrFeatureKeyExists},
	"plans_key_key":           {err: ErrPlanKeyExists},
	"tenants_external_id_key": {err: ErrTenantExists},
	// INV-E2: a duplicate feature in a plan is a validation error naming it.
	"plan_entitlements_plan_id_feature_id_key":              validation("feature_key"),
	"tenant_entitlement_overrides_tenant_id_feature_id_key": validation("feature_key"),
	// openapi Key.name: a duplicate name is validation_failed naming the field.
	"api_keys_name_unique":  validation("name"),
	"api_keys_prefix_key":   {err: ErrPrefixCollision},
	"usage_events_idem_idx": {err: ErrDuplicateEvent},

	// Checks.
	"features_key_format":            validation("feature_key"),
	"features_unit_format":           validation("unit"),
	"plans_key_format":               validation("plan_key"),
	"tenants_external_id_format":     validation("external_id"),
	"plan_entitlements_limit_nonneg": validation("limit"),
	"plan_entitlements_limit_range":  validation("limit"),
	"plan_entitlements_limit_pair":   validation("limit"),
	"tenant_overrides_limit_range":   validation("limit_value"),
	// Both aspects null: the one constraint the store, not the schema,
	// enforces (api-conventions.md section 7.4).
	"tenant_overrides_not_empty": validation("limit_value"),
	"usage_events_delta_nonzero": {err: ErrZeroDelta},
	"audit_log_subject_type":     {err: ErrAuditSubject},

	// Foreign keys.
	"plan_entitlements_plan_id_fkey":               {missing: ErrPlanNotFound},
	"plan_entitlements_feature_id_fkey":            {missing: ErrFeatureNotFound},
	"tenants_plan_id_fkey":                         {missing: ErrPlanNotFound},
	"tenant_entitlement_overrides_tenant_id_fkey":  {missing: ErrTenantNotFound},
	"tenant_entitlement_overrides_feature_id_fkey": {missing: ErrFeatureNotFound},
	"usage_events_tenant_id_fkey":                  {missing: ErrTenantNotFound},
	"tenant_entitlement_pins_tenant_id_fkey":       {missing: ErrTenantNotFound},
	"tenant_entitlement_pins_feature_id_fkey":      {missing: ErrFeatureNotFound},
	"pending_reanchors_tenant_id_fkey":             {missing: ErrTenantNotFound},
	"usage_events_feature_id_fkey":                 {missing: ErrFeatureNotFound},
	"audit_log_actor_key_id_fkey":                  {missing: &Error{Code: api.ErrorCodeInternalError, Reason: "audit actor key"}},
}

const (
	sqlstateForeignKey   = "23503"
	sqlstateUnique       = "23505"
	sqlstateCheck        = "23514"
	sqlstateNotNull      = "23502"
	sqlstateInvalidText  = "22P02"
	sqlstateOutOfRange   = "22003"
	sqlstateInvalidValue = "22023"
)

// Classify turns a driver error from a statement that was part of op into the
// catalogue refusal it stands for. An error that is not from the database (a
// lost connection, a cancelled context) is returned unchanged, because it is
// not a refusal: the snapshot and the routes answer it as
// control_plane_unavailable. A database error that no rule names becomes an
// internal_error carrying only its SQLSTATE.
func Classify(op Op, err error) error {
	var pg *pgconn.PgError
	if err == nil || !errors.As(err, &pg) {
		return err
	}
	rule, known := constraints[pg.ConstraintName]
	switch pg.Code {
	case sqlstateForeignKey:
		if known {
			switch op {
			case OpDeletePlan:
				return ErrPlanInUse
			case OpDeleteFeature:
				return ErrFeatureInUse
			}
			if rule.missing != nil {
				return rule.missing
			}
		}
	case sqlstateUnique, sqlstateCheck:
		if known && rule.err != nil {
			if rule.field != "" {
				return &Error{Code: rule.err.Code, Field: rule.field}
			}
			return rule.err
		}
	case sqlstateNotNull:
		return &Error{Code: api.ErrorCodeValidationFailed, Field: pg.ColumnName}
	case sqlstateInvalidText, sqlstateOutOfRange, sqlstateInvalidValue:
		return &Error{Code: api.ErrorCodeValidationFailed}
	}
	return &Error{Code: api.ErrorCodeInternalError, Reason: "sqlstate " + pg.Code}
}

// externalID is DR-034's character class, the same expression as the
// tenants_external_id_format constraint and openapi's ExternalId pattern. The
// API boundary (IP-09) calls ValidExternalID; the constraint is the second
// line, and TestExternalIdValidatorAgreesWithTheConstraint holds the two to
// the same answer.
var externalID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// ValidExternalID reports whether s is an acceptable tenant identifier. The
// class excludes '@', so an email address is never one (NFR-S2, ADR-0013).
func ValidExternalID(s string) bool { return externalID.MatchString(s) }
