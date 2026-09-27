# Security Model

What is being defended, from whom, with what controls, and what is explicitly out of scope. The
purpose of this document is to be answerable by a sceptical evaluator, so it states the sharp
edges rather than the reassuring summary (J-10).

## 1. Threat model in one paragraph

Quotacore holds no personal data, no credentials that can be recovered, and no money. What it holds
is **authoritative state about what a customer is owed**, and a customer who can write to that
state can take service for free. The realistic attacker is therefore not a state-sponsored
intrusion but a **stolen or misused API key**, a **malicious or compromised customer
application**, and an **operator with more access than they should have**. The design responds to
exactly those three, and to nothing exotic.

**Assets, in order of value**

1. The balance state in the fast store — writing to it directly is service theft.
2. The runtime API keys — a leaked runtime key is the main revenue risk.
3. The configuration in Postgres — a changed limit is a contractual breach.
4. The audit log — its value is that it is *admissible*; a tampered audit log is worse than none.
5. The customer's metadata — explicitly not valuable, because it must not contain anything
   sensitive.

**Non-goals.** No attack against the host, the container runtime, Postgres itself or the network is
in scope. The service assumes a correctly configured, patched host with a firewall, because
defending the host is the customer's job and pretending otherwise would be false assurance.

## 2. Trust boundaries

```
  ┌─ untrusted ────────────────────────────────────────────────────────────┐
  │  internet / customer application                                        │
  └──────────────────────────────┬──────────────────────────────────────────┘
                                 │ TLS, bearer token
  ┌─ semi-trusted ───────────────▼──────────────────────────────────────────┐
  │  Quotacore process:  HTTP layer · auth · snapshot · cycle engine ·      │
  │                      script runner · ledger writer                       │
  │  ┌ trusted ──────────────────────────────────────────────────────────┐  │
  │  │  data store (atomic scripts)          control store (SQL)         │  │
  │  └────────────────────────────────────────────────────────────────────┘  │
  └──────────────────────────────────────────────────────────────────────────┘
```

Three rules follow from these boundaries:

1. **A semi-trusted component is not trusted with authority it does not need.** The reset worker
   holds a runtime-scoped credential and has no database access (INV-WK2). The ledger writer
   appends and cannot mutate balances.
2. **The data store is trusted, and that trust is total.** It is why a script execution is
   authoritative and why the ceiling invariant is enforced there rather than in Go. Anything that
   can write to Redis can mint free service, so the host's access to Redis is the security
   boundary, and the deployment guide says so.
3. **No input ever reaches the script as code.** Keys are constructed by one tested function from
   validated components. Values are `ARGV`, never string-concatenated into the script body
   (NFR-S8). A scripting-injection defect is P0 and is treated as a design failure, not a bug.

## 3. Authentication

| Control | Decision | Rationale |
| --- | --- | --- |
| Credential | Opaque random 256-bit secret in a prefixed token | No structure to attack, no derivation to get wrong |
| Storage | Argon2id hash with a per-key salt, plus a public prefix | Unrecoverable by construction, not by policy (ADR-0012) |
| Comparison | Constant time, every request | The cache holds the hash, not a boolean |
| Cache | Bounded, per-instance, with a configured TTL; eviction is a metric | An unbounded credential cache is a memory-exhaustion vector |
| Expiry | Optional at issue, enforced always | A long-lived leaked key is worse than a rotation chore |
| Revocation | Immediate, version-bumped into the cache | No "up to N minutes" window in which a revoked key works |
| Recovery | **None, by design** | A recovery path is a path for an attacker with database read access (INV-K3) |
| Logging | Prefix only | A key in a log is a key in a log aggregator, a backup and a support system (DR-043) |
| Transport | TLS required in the deployment | A bearer token in clear text is a leaked token |
| Brute force | Irrelevant. 256 bits of entropy | — |
| Timing | Auth failures take the same time whether the prefix is unknown or the hash mismatches | A distinguishable path is an oracle |
| Bootstrap key | One admin key on first start, printed once at `warn`; the service **refuses to start** while an unacknowledged default exists | A service that starts with a known key is not secure, and NFR-S9 exists to make that impossible (INV-K2) |
| Comparison surface | Probes and `/docs` are unauthenticated; `/metrics` is bound to localhost or authenticated; `/openapi.json` is public because it describes the service, not the data | A published specification is not a disclosure |

## 4. Authorisation

Two scopes, and only two, because a scope model with more levels is a model nobody gets right.

| Scope | May | May not |
| --- | --- | --- |
| `runtime` | `consume`, `refund`, `check`, `balance` for any tenant on the instance | Any `/v1/admin/*` path. Not `grant`. Not `set`. Not key management |
| `admin` | Everything, including the data plane | — |

**The central risk, stated plainly: a runtime key is not bound to a tenant.** Possession of one
gives read and deduct access to every tenant on the instance. This is a direct consequence of
one-customer-per-instance (ADR-0013) and it is a real limitation, not a nuance.

Mitigations, and their limits:

| Mitigation | Effect | Limit |
| --- | --- | --- |
| Runtime keys are issued deliberately, named, and audited | A key exists for a reason and someone is accountable for it | Prevents accidents, not theft |
| Admin-scope keys are required for everything that changes a contract | A leaked runtime key can move balances but cannot raise a limit or issue a key | The damage is bounded to redistribution, not expansion |
| The refund ceiling `balance + amount <= limit + bonus` | A runtime key cannot inflate a balance, so it cannot resell service to itself (DR-019) | Prevents one class of abuse completely |
| Rate limiting per key | Bounds the blast radius of a leaked key | Does not prevent a slow, well-paced drain |
| Short expiry on runtime keys | Bounds exposure | Operational friction; a customer with a fleet of long-lived instances will object |
| Key rotation is a first-class, supported operation | Rotation is cheap, so exposure can be short | — |

**Not implemented, and therefore a residual risk: per-tenant runtime keys.** Binding a key to one
tenant is straightforward and is deliberately deferred, because a runtime key for a customer's own
application naturally needs to address many of that customer's tenants, and binding it to one would
force a key per tenant. The trade is documented here rather than buried in a changelog, and it is
the first candidate for a post-MVP release.

## 5. Data protection

| Control | Rule |
| --- | --- |
| PII | None required, none accepted, none stored. `external_id` excludes `@` **at the schema level**, so an email address cannot become a tenant identifier (DR-034, NFR-S2) |
| `metadata` | A bounded correlation object. Not indexed, excluded from logs, never returned by an endpoint, and documented as not a data store |
| Secrets at rest | API keys are Argon2id hashes. Nothing else in the schema is secret. Connection strings come from the environment, not the database |
| Secrets in transit | TLS, and `rediss://` for the data store. A plaintext `redis://` connection is a configuration error the service warns about at startup and honours only with an explicit override |
| Data in the ledger | Amounts, deltas, balances, request identifiers, and the customer's optional metadata. Billing-relevant, not personal |
| Data in logs | No `external_id` values, no tenant names, no `metadata` content, no secrets (NFR-O6, DR-043) |
| Retention | Events 90 days, audit 400 days, both configurable, both applied by a bounded background job and never by an API call (DR-044, NFR-C6) |
| Egress | None in the MVP. No telemetry, no update check, no crash reporting, no usage measurement. v0.3 webhooks are the only egress, operator-configured (NFR-S3) |
| Backups | Postgres only. The fast store is not backed up, because it is rebuildable and a backup of it would be a second copy of revenue-bearing state to protect (NFR-OPS4) |
| Deletion | Tenants are soft-deleted. Hard deletion of a tenant with ledger history is not exposed, because the history is the customer's contractual record (DR-035, INV-P3) |
| Multi-tenancy | One customer per instance. This is a **security boundary**, which is why it is an ADR rather than a scaling choice: a compromise of one instance is a compromise of one customer's data |

## 6. Application-level threats

| Threat | Control | Residual risk |
| --- | --- | --- |
| Key enumeration | 256-bit entropy; no endpoint enumerates keys; the prefix is public and reveals nothing about the secret | None |
| Credential stuffing against the admin API | Argon2id makes each guess expensive; rate limiting per key and per IP; no default credentials | An attacker with a valid admin key has full control, by design |
| Lua injection through a feature key or tenant id | Keys built by one tested function; values passed as `ARGV`; the script body is a constant (NFR-S8) | A bypass is P0 |
| Integer overflow in an amount | `int64` parsed strictly; Lua arithmetic in doubles is a real hazard, so amounts are validated and bounded before the script and the script range-checks (see below) | **Lua uses doubles.** `2^53` is where integer arithmetic stops being exact. Amounts above that are rejected with `400 validation_failed` and the limit is documented |
| Replay of a webhook (v0.3) | HMAC signature with a timestamp and a JTI, and a documented receiver-side replay window | Receiver-side idempotency remains the receiver's responsibility |
| Webhook SSRF (v0.3) | Endpoints must be `https`, private and link-local ranges are refused by default with an explicit override, redirects are not followed, and DNS is re-checked on connect | A customer who deliberately points a webhook at their own infrastructure is not an attack |
| Ledger tampering by an operator | Audit log is append-only by convention, the ledger has a four-way reconciliation check, and a mismatch is a P1 alert | An operator with database write access can alter both. Detected by reconciliation, not prevented. Stated plainly |
| Denial of service via key churn | Key creation is admin-only and rate-limited | An admin key holder can exhaust memory with keys. Bounded cache, and the metric makes it visible |
| Denial of service via `metadata` flooding | 4 KiB cap per request, rate limiting, and a per-key budget | Sustained flooding within the rate limit costs the customer's own instance |
| Information disclosure through error messages | A single envelope; no stack traces; no internal identifiers; errors carry only documented `details` keys | — |
| Existence disclosure through the data plane | Unknown features and features not in the plan both return `403 feature_not_in_plan` (FS-17) | Tenant existence *is* disclosed to a valid runtime key, which is inherent to a key that addresses tenants by `external_id` |
| Privilege escalation via a scope confusion | The scope is parsed from the stored record, never from the token text. The token's visible prefix is cosmetic | None |
| Open redirect or XSS in a future UI (v0.4) | Out of scope for the MVP. Recorded as a requirement for v0.4: no `innerHTML` of server data, a strict CSP, and no user-controlled redirect targets | — |

**The double-precision hazard deserves emphasis.** Lua 5.1 numbers are doubles. A `balance` above
`2^53` cannot be represented exactly, and a system that silently rounds a customer's balance is
worse than one that refuses the value. The service therefore bounds all amounts, balances, limits
and bonuses to `±2^53 − 1`, validates on input with `400 validation_failed`, and the DDL
`CHECK` mirrors the bound rather than using the full `bigint` range.

## 7. Transport, network and host

| Layer | Requirement |
| --- | --- |
| TLS | Terminated by the customer's proxy or by the service. A self-signed certificate is supported for internal deployments, with a startup warning |
| Bind address | The service binds `0.0.0.0:8080` inside the container; the published port should be restricted by the customer's firewall. The metrics port 9090 binds localhost inside the container |
| Privileges | Non-root user, non-root port, read-only root filesystem, `no-new-privileges`, all capabilities dropped (NFR-S6) |
| Secrets in the environment | Connection strings, the bootstrap acknowledgement, and the Argon2 parameters. Never baked into the image |
| Admin API exposure | Recommended: on an internal network only. A key on a public endpoint is a key that will eventually leak |
| Data store exposure | Bind to localhost or a private network. Anyone with write access to it can mint free service, and that is the strongest single reason to keep it private (section 2) |
| Postgres exposure | Same. Plus: no `trust` authentication, a `scram-sha-256` role, and a dedicated non-superuser application role |
| Egress | Blocked by default in the reference compose file, except v0.3 webhooks. A metering component with unconstrained egress is a component that can be used as a proxy |
| Supply chain | Pinned base images by digest, pinned Go modules with a committed `go.sum`, an SBOM per build, and a documented licence check (NFR-S7, NFR-C3) |
| Rate limiting | Per key on the data plane, per IP on the admin API, with `Retry-After`. Distinct from any customer-configured limit, and from a business quota (DR-040) |

## 8. What the operator can and cannot do

Stated because "the operator can read your customers' usage" is a question every evaluator asks.

| Operator can | Operator cannot |
| --- | --- |
| Read balances, cycle windows and usage history | Recover an API key, ever |
| Change limits, plans, overrides, grants | Read the customer's application data; there is none |
| Suspend and resume a tenant | Recover a consumed amount; `refund` grants it back, and the original event remains |
| Force a rollover, with an audit record | Alter or delete an audit or event row through the API |
| See who did what, when, with before and after values | Bypass the ceiling invariant from outside the data store |
| Delete a tenant (softly) | Delete a tenant's history, or the customer's own data |

The asymmetry is deliberate: an operator can grant service, which is a business decision, and
cannot take it back. Making a grant irreversible is what makes the audit log meaningful.

## 9. Incident response

| Incident | First action | Containment |
| --- | --- | --- |
| A runtime key leaked | Revoke it | The revocation is immediate (K-2). Rotate, redeploy the customer's application, review events for the exposure window |
| An admin key leaked | Revoke, then issue a replacement | Audit the log for every action by that key's prefix. The prefix is in every audit row, which is why it exists |
| A tenant's balance was inflated | `set` the balance to the correct value with a reason | The change is audited and emits a `set` event, so the correction is visible rather than silent. The root cause is the runtime ceiling failing, which is a P0 |
| A customer's data was tampered with in Postgres | Restore from backup; reconcile | The fast store was not affected, so enforcement continued correctly throughout |
| Unauthorised access suspected | Stop the instance; preserve Postgres; do **not** flush the fast store | The fast store is evidence and it is rebuildable; flushing destroys evidence to fix nothing |
| The service was compromised at the host level | Rebuild the host | Nothing in the design survives a compromised host, and that is the boundary of this model |

## 10. Compliance posture

Stated as facts with their limits, not as claims.

| Area | Position |
| --- | --- |
| Personal data | None collected, required, or accepted (NFR-S2) |
| Data residency | The customer's host. Nothing leaves it in the MVP (NFR-C5) |
| Licence | Apache-2.0, with the patent grant. Compatible with proprietary redistribution (NFR-C1) |
| Dependencies | Default image BSD-3-Clause. The Redis licence change is documented, not discovered (NFR-C2, ADR-0009) |
| SBOM | Generated per build (NFR-C3) |
| Audit | Complete for administrative actions, in-transaction, append-only, retained 400 days by default |
| Access control | Named, scoped, rotatable, audited credentials. No per-tenant runtime keys in the MVP (section 4) |
| Encryption at rest | Not provided. The host's disk encryption is the customer's control, and the service does not hold data whose compromise it cannot describe |
| Certification | None claimed. A certification claim would not be true of a self-hosted single-binary service operated by its owner |
| Right to erasure | Soft deletion only, because the ledger is a contractual record. A hard-delete workflow is deliberately not exposed |

## 11. Verification

Security claims are tested, not asserted. In CI:

1. **No plaintext secret in any persistent store.** A test asserts that no column in any table
   contains a value submitted as a key, and that no log line contains one.
2. **Scope enforcement.** Every `/v1/admin/*` path returns `403` for a runtime key, exercised
   against the real router, not a mock.
3. **Revocation is immediate.** A test revokes a key and asserts the very next request fails.
4. **The ceiling holds.** Every error code in the catalogue is exercised, and the balance and event
   count are asserted unchanged afterwards.
5. **The `2^53` bound is enforced** on amounts, limits, balances and bonuses, from both the API
   and the script.
6. **No PII can be stored.** A test attempts to create a tenant with an email-like
   `external_id` and asserts the schema constraint rejects it.
7. **Injection attempts.** Feature keys, tenant identifiers and metadata containing Lua
   metacharacters, newlines, template syntax and oversized values are all fuzzed, asserting that
   no key is ever constructed with an unexpected shape.
8. **The service refuses to start** with an unacknowledged bootstrap key, and the test asserts the
   refusal.
9. **Egress is closed.** A network-policy test asserts the MVP makes no outbound connection other
   than to its two datastores.
