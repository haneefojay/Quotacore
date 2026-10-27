package cli

import contract "github.com/quotacore/quotacore/api"

// The exit codes of every command (IP-14 D5, docs/architecture/cli.md
// section 3). None equals an HTTP status, so a script can never mistake one
// for the other. An error answer is mapped by its catalogue code, not by its
// status, so the 403s that are not about the credential are not reported as
// one.
const (
	ExitOK          = 0
	ExitFinding     = 1 // a local operation's finding, or the server refusing to start
	ExitUsage       = 2 // an unknown command, a bad flag or a missing variable
	ExitClient      = 3
	ExitCredential  = 4
	ExitNotFound    = 5
	ExitConflict    = 6
	ExitQuota       = 7 // reserved: no v0.1 command can be answered quota_exceeded
	ExitRateLimited = 8
	ExitUnavailable = 9 // also a connection refused or timed out
	ExitInternal    = 10
)

// ExitFor is the exit code of an error answer carrying code. A code the
// catalogue does not have is a response the contract does not define.
func ExitFor(code contract.ErrorCode) int {
	switch code {
	case contract.ErrorCodeValidationFailed, contract.ErrorCodeInvalidAmount, contract.ErrorCodeInvalidWindow,
		contract.ErrorCodeInvalidTimezone, contract.ErrorCodeMissingIdempotencyKey, contract.ErrorCodePayloadTooLarge,
		contract.ErrorCodeFeatureNotInPlan, contract.ErrorCodeFeatureArchived, contract.ErrorCodeTenantSuspended,
		contract.ErrorCodeNotImplemented:
		return ExitClient
	case contract.ErrorCodeUnauthorized, contract.ErrorCodeAPIKeyInvalid, contract.ErrorCodeInsufficientScope:
		return ExitCredential
	case contract.ErrorCodeTenantNotFound, contract.ErrorCodeFeatureNotFound, contract.ErrorCodePlanNotFound,
		contract.ErrorCodeAPIKeyNotFound:
		return ExitNotFound
	case contract.ErrorCodeIdempotencyKeyReuse, contract.ErrorCodePlanInUse, contract.ErrorCodeFeatureInUse,
		contract.ErrorCodeFeatureKeyExists, contract.ErrorCodeTenantExists, contract.ErrorCodeTenantDeleted,
		contract.ErrorCodePlanKeyExists, contract.ErrorCodeRefundExceedsGrant, contract.ErrorCodeGrantExceedsCeiling,
		contract.ErrorCodeResetNotSupported, contract.ErrorCodeFeatureUnitImmutable,
		contract.ErrorCodeResourceVersionConflict, contract.ErrorCodeConfirmationRequired:
		return ExitConflict
	case contract.ErrorCodeQuotaExceeded:
		return ExitQuota
	case contract.ErrorCodeRateLimited:
		return ExitRateLimited
	case contract.ErrorCodeServiceUnavailable, contract.ErrorCodeControlPlaneUnavailable:
		return ExitUnavailable
	}
	return ExitInternal
}
