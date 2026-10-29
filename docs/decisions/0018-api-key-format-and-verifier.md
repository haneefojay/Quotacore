# ADR-0018 — API keys: the contract's token format, and a SHA-256 verifier on every request

- **Status:** Accepted
- **Date:** 2026-10-19
- **Deciders:** project owner
- **Supersedes:** [ADR-0012](0012-hashed-scoped-api-keys.md), which is marked Superseded and names this record
- **Affects:** [api-conventions](../architecture/api-conventions.md), [request-lifecycle](../architecture/request-lifecycle.md), [security-model](../architecture/security-model.md), [data-model](../architecture/data-model.md), [performance](../architecture/performance.md), [deployment](../architecture/deployment.md), [SECURITY.md](../../SECURITY.md), `api/openapi.yaml`

## Context

ADR-0012 chose hashed, scoped, rotatable keys in PostgreSQL with an environment bootstrap key. That
choice stands. Two of its details had drifted from the rest of the specification by the time `IP-07`
had to authenticate a request, and both have to be one thing before a key can be parsed or issued:

1. **The token format was written three ways.** ADR-0012 gave `qc_<scope>_<id>_<secret>` with a
   UUIDv7 identifier and the scope spelled `rt`. [api-conventions.md](../architecture/api-conventions.md)
   §5 and [deployment.md](../architecture/deployment.md) gave `qc_live_<prefix>.<secret>` and
   `qc_admin_<prefix>.<secret>`. The contract frozen by `IP-02` — the `bearerAuth` scheme, the
   `KeyCreated` example and `Key.prefix`'s pattern `^[0-9a-f]{8,64}$` — gave
   `qc_runtime_<prefix>.<secret>` and `qc_admin_<prefix>.<secret>`, and CI's bootstrap refusal test
   already uses that form.
2. **The verifier was written two ways.** ADR-0012 stores `sha256(secret)` and argues that a fast
   hash is the right primitive for a high-entropy secret. Eight other documents and three contract
   descriptions said Argon2id. Argon2id at any recommended cost is tens of milliseconds and megabytes
   of memory per verification. Request-lifecycle step 2 verifies on **every** request, so at 2,000
   requests per second that cost cannot fit inside the 5 ms p99 of the first claim. The 80 µs that
   [performance.md](../architecture/performance.md) budgeted for it is reachable only with a token
   Argon2id setting, which buys nothing over SHA-256 for a random 256-bit secret.

## Options considered

**A — The contract's format, and SHA-256 as ADR-0012 decided (chosen).**

**B — Argon2id with a recommended cost, verified per request.** Rejected: it spends the latency
budget of the product's first claim on a guessing defence that a 256-bit secret does not need.

**C — Argon2id, with the verified result cached.** Rejected:
[request-lifecycle.md](../architecture/request-lifecycle.md) step 2 explains why a cached *verified*
key is the thing a revocation, an expiry or a copied key would defeat.

## Decision

Everything in ADR-0012 is carried over unchanged — two scopes, server-generated keys shown once,
immediate revocation through the cache's invalidation, `last_used_at` written off the path at most
once a minute, the bootstrap key, and bearer transport only — except these two points.

- **Token format.** `qc_<scope>_<prefix>.<secret>`, where `<scope>` is `runtime` or `admin`,
  `<prefix>` is the key's public hexadecimal prefix (`^[0-9a-f]{8,64}$`, the `api_keys.prefix`
  column), and `<secret>` is 32 bytes from `crypto/rand`, base64url without padding. The scope label
  is cosmetic, as [security-model.md](../architecture/security-model.md) says: authorisation reads the
  scope from the stored row, and a token whose label disagrees with its row is
  `401 api_key_invalid`.
- **Verifier.** `api_keys.key_hash` holds `sha256(secret)`, 32 bytes. Every request recomputes it and
  compares with `subtle.ConstantTimeCompare`. An unknown prefix is compared against a fixed dummy
  digest, so it takes the same time as a wrong secret. The bounded key cache spares the database
  read and nothing else.

## Rationale

A 256-bit secret drawn from `crypto/rand` cannot be guessed, so a slow hash defends against nothing
that the secret's entropy has not already defeated, and a stolen database still yields no usable
credential. A slow hash is the right tool for a human-chosen password, and this storage approach
must never be reused for one; that warning, from ADR-0012, stands.

The format is the contract's because the contract is frozen and published: it is what `/docs` shows a
customer, what `Key.prefix` validates, and what IP-09 will issue.

## Consequences

**Positive**
- Authentication costs a microsecond or so, so the first claim's budget is spent on enforcement.
- One format and one verifier, stated once.

**Negative**
- A deployment's key table is only as strong as the secret's entropy, which is why keys are never
  chosen by a person.
- Every document that said Argon2id carries a dated correction, and the research note's line on
  `argon2.IDKey` is withdrawn.

## Revisit when

- Keys ever become human-chosen, which would make this storage wrong rather than merely weaker.
- The triggers in ADR-0012's "Revisit when" fire: a hosted multi-tenant offering, per-tenant keys,
  or mutual TLS.
