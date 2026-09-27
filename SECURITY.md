# Security

Quotacore · 2026-09-27

The full model — trust boundaries, control tables, the operator's actual powers, and the nine
security tests — is [docs/architecture/security-model.md](docs/architecture/security-model.md).
This document is the summary a sceptical evaluator reads first (J-10), and it is written to include
the sharp edges rather than to reassure.

## 1. Posture

Quotacore holds no personal data, no recoverable credentials, and no money. What it holds is
**authoritative state about what a customer is owed**, and anyone who can write to that state can
take service for free. The realistic threats are a stolen or misused API key, a compromised
customer application, and an operator with more access than they should have. The design answers
those three and makes no pretence about the rest: the host, the container runtime, PostgreSQL and
the network are assumed to be correctly configured and patched, because defending a host is the
customer's job.

The consequence worth stating early: **the fast store is fully trusted.** Anything that can write to
Redis can mint free service, so access to it is the security boundary, and the deployment guide
says so rather than burying it.

## 2. Scope

In scope: authentication, authorisation, the data store's trust boundary, script injection, integer
precision, PII exclusion, secret handling, transport, the container posture, and supply chain.

Out of scope: the host, the network perimeter, PostgreSQL's own authentication beyond the
configuration we prescribe, and a penetration test — see [§6](#6-known-gaps).

## 3. Threat model

| Threat | Likelihood | Impact | Mitigation | Residual risk |
| --- | --- | --- | --- | --- |
| Leaked runtime key | Medium | High — deduct and read every tenant | Named, scoped, rotatable, revocable immediately, prefix-only logging, per-key rate limiting, optional expiry | A runtime key is **not** bound to a tenant. Possession gives access to every tenant on the instance |
| Leaked admin key | Low | Severe — full control | Issued deliberately, short-lived by preference, every action audited with the key prefix and before/after values | Full control by design; detection depends on the audit log |
| Direct write to the fast store | Low | Severe — free service | Bind to localhost or a private network; authenticated, TLS-protected connections; the ceiling invariant enforced in the datastore, not in Go | Total trust. A store write is undetectable from inside the service |
| Script injection via a tenant or feature identifier | Low | Severe | Keys built by one tested function; all values passed as `ARGV`; the script body is a constant (NFR-S8) | A bypass is P0 and treated as a design failure |
| Integer overflow in an amount | Medium | High — a silently wrong balance | `2^53 − 1` bound on amounts, balances, limits and bonuses, enforced in validation, in the script and in the DDL | Values above the bound are rejected, not rounded |
| Customer application tampering with configuration | Low | High — a changed limit is a breach | Configuration is reachable only with an admin key, on the internal network, and every change is audited in-transaction | An operator can change a limit. That is a business action, and it is recorded |
| Email-shaped tenant identifiers | Medium | Low, but structural | `external_id` excludes `@` in the schema itself, so a PII-shaped identifier cannot be stored (DR-034) | None, by construction |
| Denial of service via key churn or `metadata` flooding | Low | Low | Admin-only and rate-limited key creation, bounded caches, 4 KiB metadata cap, per-key budgets | Sustained flooding within the rate limit costs the customer's own instance |
| Ledger tampering by an operator | Low | High — auditability is the asset | Four-way reconciliation, P1 alert on mismatch, append-only by convention and by API | An operator with database write access can alter both. Detected, not prevented |
| Service compromise at the host level | Low | Severe | Non-root, read-only root filesystem, no new privileges, dropped capabilities, egress closed by default (NFR-S6) | Out of this model's scope. Rebuild the host |

## 4. Controls

### 4.1 Authentication and authorisation

Opaque 256-bit secrets, Argon2id with a per-key salt, constant-time comparison on every request,
and a bounded credential cache that holds the hash rather than a boolean. No recovery path, ever —
a recovery path is a path for an attacker with database read access (INV-K3). Two scopes only:
`runtime` for the four data-plane endpoints, `admin` for everything including configuration. The
bootstrap key is printed once, and the service **refuses to start** while an unacknowledged default
exists (NFR-S9, INV-K2).

### 4.2 Secrets management

Connection strings, the bootstrap acknowledgement and the Argon2 parameters come from the
environment and are never baked into an image. Nothing else in the schema is secret. Plaintext
`redis://` is a startup warning honoured only with an explicit override.

### 4.3 Data protection

No personal data is required, accepted or stored. `metadata` is a bounded correlation object, not a
data store: not indexed, not logged, never returned. Retention is 90 days for events and 400 for
audit, applied by a bounded background job and never by an API call (DR-044). Backups cover
PostgreSQL only, because the fast store is rebuildable and a second copy of revenue-bearing state
is a liability. Encryption at rest is the host's control and is not claimed here.

### 4.4 Input validation and injection

Bounded strings, bounded collections, strict integer parsing, and a schema-level `CHECK` mirroring
the `2^53` bound. Every parameterised query is parameterised. No request value is ever concatenated
into a script, a log line or a response body. Error messages carry a code, a message, a request ID
and documented `details` keys — never a stack trace or an internal identifier.

### 4.5 Network and transport

TLS in the deployment, `rediss://` for the data store, admin endpoints on an internal network,
`/metrics` bound to localhost, and egress blocked by default in the reference compose file except
for the v0.3 webhooks. Rate limiting is per key on the data plane and per IP on the admin API, and is
deliberately distinct from a business quota (DR-040).

## 5. Dependencies and supply chain

Pinned base images by digest, pinned Go modules with a committed `go.sum`, an SBOM per build, and a
documented licence check. The default data-store image is BSD-3-Clause; the Redis licence change
is recorded in [ADR-0009](docs/decisions/0009-redis-protocol-valkey-default.md) and in
[the licensing research](docs/research/licensing-redis-vs-valkey.md) rather than discovered at
audit time. No runtime dependency is fetched at start-up.

## 6. Known gaps

Each is a decision, not an oversight. Severity is impact if exploited by a party who already holds a
valid key or host access.

| # | Gap | Severity | Position | Revisit |
| --- | --- | --- | --- | --- |
| 1 | A runtime key is not bound to a tenant | High | Documented; mitigated by admin-scope being required for anything that changes a contract, and by the refund ceiling | First candidate for a post-MVP release |
| 2 | No encryption at rest | Low | The host's responsibility; the service holds no data whose compromise it cannot describe | With a managed-deployment story, not before |
| 3 | The fast store is fully trusted | High, if the boundary is ignored | Bind it privately and say so in the deployment guide | Revisit if a managed data store is ever offered |
| 4 | An operator with database write access can alter both the ledger and the audit log | Medium | Detected by reconciliation and alerted as P1, not prevented | Only a cryptographic append-only store would change this, at a cost the MVP cannot bear |
| 5 | No independent penetration test | Medium | No code exists to test | Before the first public release, not before the first pilot |
| 6 | No per-tenant key issuance without a runtime key per tenant | Low | Deferred deliberately | Post-MVP |
| 7 | The v0.4 UI's XSS and redirect surface | Unknown | Not built; requirements recorded for when it is | At the start of v0.4 |

## 7. Verification

Nine security tests run in CI, listed in full in
[security-model.md §11](docs/architecture/security-model.md#11-verification). The four that would
fail first, and therefore the four to write first, are: no plaintext secret in any persistent store;
scope enforcement against the real router; immediate revocation; and the ceiling holding across
every error code in the catalogue.

## 8. Incident response

| Incident | First action | Containment |
| --- | --- | --- |
| Runtime key leaked | Revoke it | Immediate; rotate and redeploy the customer application; review events for the exposure window |
| Admin key leaked | Revoke, then issue a replacement | Audit every action by that key's prefix |
| Balance inflated | `set` the correct value with a reason | Audited and event-emitting, so the correction is visible; a runtime ceiling failure is P0 |
| Data tampered with in PostgreSQL | Restore from backup, then reconcile | The fast store was unaffected, so enforcement continued correctly |
| Unauthorised access suspected | Stop the instance; preserve PostgreSQL; **do not flush the fast store** | The fast store is evidence and it is rebuildable |
| Host compromise | Rebuild the host | Nothing in this design survives a compromised host |
