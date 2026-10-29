// Package auth authenticates a request's API key (request-lifecycle.md section
// 1, step 2) under ADR-0018: the token is `qc_<scope>_<prefix>.<secret>`, the
// prefix finds the key, and SHA-256 of the secret is compared in constant time
// with the stored verifier on every request. The bounded key cache spares the
// database read and nothing else, so a revoked or expired key is refused by the
// same comparison that refuses a wrong secret.
//
// Postgres is reached only through KeyLoader, which internal/db implements, so
// this package stays on the data plane's side of the line that
// TestNoControlPlaneConnectionOnTheDataPlane draws.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
)

// Scope is a key's scope. The stored row is the authority; the label in the
// token is cosmetic and must agree with it (security-model.md, ADR-0018).
type Scope string

const (
	Runtime Scope = "runtime"
	Admin   Scope = "admin"
)

// Allows reports whether a key of scope s may call a route that requires
// required. An admin key works on both planes; a runtime key only where runtime
// is enough (api-conventions.md section 5, NFR-S4).
func (s Scope) Allows(required Scope) bool {
	switch required {
	case "", Runtime:
		return s == Runtime || s == Admin
	case Admin:
		return s == Admin
	}
	return false
}

// SecretBytes is the length of the random secret. Its base64url form, without
// padding, is 43 characters.
const SecretBytes = 32

// PrefixBytes is the length of the random prefix IssueToken draws, written as
// 16 hexadecimal characters. Key.prefix admits 8 to 64.
const PrefixBytes = 8

var tokenShape = regexp.MustCompile(`^qc_(runtime|admin)_([0-9a-f]{8,64})\.([A-Za-z0-9_-]{43})$`)

// ErrMalformed is a token that is not in the contract's format. It is refused
// before any lookup.
var ErrMalformed = errors.New("auth: the token is not a Quotacore API key")

// Token is a parsed bearer token. The secret is held only as its digest, so a
// Token can be logged by accident without leaking anything but the prefix.
type Token struct {
	Label  Scope
	Prefix string
	digest [sha256.Size]byte
}

// Parse reads `qc_<scope>_<prefix>.<secret>`.
func Parse(raw string) (Token, error) {
	m := tokenShape.FindStringSubmatch(raw)
	if m == nil {
		return Token{}, ErrMalformed
	}
	secret, err := base64.RawURLEncoding.DecodeString(m[3])
	if err != nil || len(secret) != SecretBytes {
		return Token{}, ErrMalformed
	}
	return Token{Label: Scope(m[1]), Prefix: m[2], digest: Digest(secret)}, nil
}

// Hash is the SHA-256 digest of the token's secret, what api_keys.key_hash
// stores. The bootstrap uses it to install an operator-supplied key without
// the secret ever leaving this package in any other form (ADR-0018).
func (t Token) Hash() []byte { d := t.digest; return d[:] }

// Digest is the stored verifier, api_keys.key_hash: SHA-256 over the 32 secret
// bytes, not over their text form.
func Digest(secret []byte) [sha256.Size]byte { return sha256.Sum256(secret) }

// Issued is a new key: the token the caller is shown once, and what the
// control plane stores.
type Issued struct {
	Token  string
	Prefix string
	Hash   []byte
}

// IssueToken draws a new key of the given scope from crypto/rand. The token is
// returned once and never stored; only Hash is (ADR-0012, ADR-0018). IP-09's
// key route is the production caller; the tests use it to seed api_keys.
func IssueToken(scope Scope) (Issued, error) {
	if scope != Runtime && scope != Admin {
		return Issued{}, fmt.Errorf("auth: %q is not a scope", scope)
	}
	var prefix [PrefixBytes]byte
	var secret [SecretBytes]byte
	if _, err := rand.Read(prefix[:]); err != nil {
		return Issued{}, err
	}
	if _, err := rand.Read(secret[:]); err != nil {
		return Issued{}, err
	}
	p := hex.EncodeToString(prefix[:])
	d := Digest(secret[:])
	return Issued{
		Token:  "qc_" + string(scope) + "_" + p + "." + base64.RawURLEncoding.EncodeToString(secret[:]),
		Prefix: p,
		Hash:   d[:],
	}, nil
}
