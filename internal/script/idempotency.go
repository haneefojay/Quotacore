package script

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"time"
)

// IdempotencyWindow is how long a recorded outcome lives, and it is the window
// DR-029 makes a customer-facing promise about: a key first seen more than
// twenty-four hours ago is a new operation.
//
// It is a constant here, and QUOTACORE_IDEMPOTENCY_TTL is a constant in
// cmd/quotacore that refuses any other value at start-up. Neither reads the
// other, so a test in cmd/quotacore asserts they are the same number, and a
// source test here asserts the scripts spell it the same way. A value that could
// be configured would be a value a deployment could shorten without noticing,
// and the window is the guarantee rather than a setting (AGENTS.md, "Known
// traps").
const IdempotencyWindow = 24 * time.Hour

// fingerprintLength is how much of the digest the record carries: the first
// sixteen bytes, hex-encoded. Truncation is a sizing decision rather than a
// cryptographic one, because the stored value is never an input to anything -
// it is compared for equality against another value computed the same way, and
// the record population that deployment.md §5 sizes is roughly 170 million
// entries at NFR-T1. Sixteen bytes of digest is far beyond what keeps a
// coincidence out of a per-tenant key space.
const fingerprintLength = 32

// Fingerprint returns the value that decides whether a repeat of an
// Idempotency-Key is a replay of the same logical operation (DR-027) or a reuse
// of one key for two (DR-028).
//
// It is a hash of operation, tenant, feature and amount, and of nothing else.
// In particular it is not a hash of the request id, because a retry carries a
// new one by definition and including it would make every retry a mismatch and
// the mechanism useless (idempotency-and-retries.md, "The fingerprint").
//
// The four fields are length-prefixed rather than joined by a separator. A
// separator is enough only while no field can contain the separator, and a
// feature key is customer-supplied text, so the assumption is one this package
// would rather not make: with lengths there is no combination of field contents
// that reads as a different set of fields.
func Fingerprint(op Operation, tenantID, featureKey string, amount int64) string {
	digest := sha256.New()
	var length [8]byte
	for _, field := range []string{string(op), tenantID, featureKey, strconv.FormatInt(amount, 10)} {
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write([]byte(field))
	}
	return hex.EncodeToString(digest.Sum(nil))[:fingerprintLength]
}
