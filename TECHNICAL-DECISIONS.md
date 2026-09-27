# Technical Decisions

Quotacore · 2026-09-27

Sixteen decisions, all accepted, none superseded. Full records live in
[docs/decisions/](docs/decisions/) as `NNNN-short-title.md`.

**Reading the log.** Every record has a *revisit trigger*: the specific event that would reopen
the decision. A decision without one is a preference, and a preference that is load-bearing is
indistinguishable from a bug.

**Two invariants of the process.** An accepted ADR is immutable — supersede it, never edit it,
because the record of what was believed and when is itself evidence. And an ADR that changes
behaviour changes [domain-rules.md](docs/product/domain-rules.md) in the same commit, or the
specification and the decisions disagree and one of them is wrong.

## Decision log

| ID | Decision | Status | Date | Record |
| --- | --- | --- | --- | --- |
| 0001 | Separate the control plane from the data plane | Accepted | 2026-09-27 | [record](docs/decisions/0001-control-plane-data-plane-split.md) |
| 0002 | Server-side Lua scripts are the atomicity primitive | Accepted | 2026-09-27 | [record](docs/decisions/0002-lua-scripts-for-atomicity.md) |
| 0003 | Cycle rollover is lazy, monotonic, and computed in the application | Accepted | 2026-09-27 | [record](docs/decisions/0003-lazy-monotonic-cycle-rollover.md) |
| 0004 | Idempotency: prevention in the data plane, detection in the control plane | Accepted | 2026-09-27 | [record](docs/decisions/0004-idempotency-prevention-and-detection.md) |
| 0005 | Refund is a first-class operation with a hard invariant | Accepted | 2026-09-27 | [record](docs/decisions/0005-refund-as-first-class-operation.md) |
| 0006 | Plan edits take effect at the next cycle boundary by default | Accepted | 2026-09-27 | [record](docs/decisions/0006-plan-edits-at-next-cycle-boundary.md) |
| 0007 | Per-tenant IANA time zone, with UTC as the default | Accepted | 2026-09-27 | [record](docs/decisions/0007-per-tenant-timezone.md) |
| 0008 | One cadence model: anchored calendar cycles, with rolling windows as a separate mechanism | Accepted | 2026-09-27 | [record](docs/decisions/0008-cadence-model-anchored-and-rolling.md) |
| 0009 | Speak the Redis protocol, ship Valkey by default | Accepted | 2026-09-27 | [record](docs/decisions/0009-redis-protocol-valkey-default.md) |
| 0010 | Go, `net/http` with chi, and a spec-first OpenAPI contract | Accepted | 2026-09-27 | [record](docs/decisions/0010-go-chi-spec-first-openapi.md) |
| 0011 | The control plane is PostgreSQL only, with embedded up-only migrations | Accepted | 2026-09-27 | [record](docs/decisions/0011-postgres-only-control-plane.md) |
| 0012 | Hashed, scoped, rotatable API keys in PostgreSQL, with an environment bootstrap key | Accepted | 2026-09-27 | [record](docs/decisions/0012-hashed-scoped-api-keys.md) |
| 0013 | Single-tenant instance, flat tenants, opaque identifiers, no PII | Accepted | 2026-09-27 | [record](docs/decisions/0013-single-tenant-flat-model-no-pii.md) |
| 0014 | Apache License 2.0 | Accepted | 2026-09-27 | [record](docs/decisions/0014-apache-2-0-license.md) |
| 0015 | A hard MVP boundary, with the remaining capability sequenced behind it | Accepted | 2026-09-27 | [record](docs/decisions/0015-release-slicing.md) |
| 0016 | The usage event log is a signed-delta ledger, and it is the recovery source | Accepted | 2026-09-27 | [record](docs/decisions/0016-usage-event-ledger.md) |
| 0017 | No eviction inside the window, and reversal for what eviction cannot prevent | Accepted | 2026-09-27 | [record](docs/decisions/0017-noeviction-and-duplicate-reversal.md) |

## The three load-bearing decisions

If only three records are read, read these. The rest are refinements of them.

**0002 — Lua scripts.** The product's central promise is that a check and a deduction are one
indivisible act. Every alternative either cannot express that atomically, or moves the atomicity
problem to the customer. A multi-key `MULTI/EXEC` is atomic in time but not in logic: the script
must exist in the store, and a failover to a replica that has not loaded it changes behaviour
underneath a running instance. A stored procedure is atomic and portable, but PostgreSQL is the
wrong tool for a millisecond-latency path that must survive a database outage. The revisit trigger
is Redis removing or changing server-side scripting, which would make the calculus different.

**0003 — Lazy monotonic rollover.** The alternative designs all fail in a way that is invisible
until it is expensive. A cron job at midnight is wrong on a leap day, in a half-hour-offset zone,
after a restart, and for a tenant created at 09:00, and each of those failures is a support ticket
about a customer being denied unfairly. Adding a duration to a timestamp is worse: it drifts
across DST. This decision makes the cycle a pure function, so correctness is testable without a
clock, and makes a missed boundary a non-event — the next request repairs it atomically.

**0004 — Split idempotency.** Prevention in the data plane, detection in the control plane.
Doing it only in the database would put an insert on a path that must not touch the database.
Doing it only in the datastore would let a rolled-back ledger write and a surviving idempotency
record disagree. Splitting the responsibility costs one recorded fingerprint and one cross-check
against the ledger, and buys both fast prevention and authoritative detection. The revisit trigger
is a customer who needs exactly-once billing rather than exactly-once enforcement, which is a
different product and a different store.

## Decision map by concern

| Concern | Decisions |
| --- | --- |
| Correctness of enforcement | [0002](docs/decisions/0002-lua-scripts-for-atomicity.md), [0003](docs/decisions/0003-lazy-monotonic-cycle-rollover.md), [0005](docs/decisions/0005-refund-as-first-class-operation.md) |
| Correctness of configuration | [0001](docs/decisions/0001-control-plane-data-plane-split.md), [0006](docs/decisions/0006-plan-edits-at-next-cycle-boundary.md), [0016](docs/decisions/0016-usage-event-ledger.md) |
| Time and cadence | [0007](docs/decisions/0007-per-tenant-timezone.md), [0008](docs/decisions/0008-cadence-model-anchored-and-rolling.md) |
| Money safety | [0004](docs/decisions/0004-idempotency-prevention-and-detection.md), [0005](docs/decisions/0005-refund-as-first-class-operation.md), [0016](docs/decisions/0016-usage-event-ledger.md), [0017](docs/decisions/0017-noeviction-and-duplicate-reversal.md) |
| Capacity and durability of the fast store | [0009](docs/decisions/0009-redis-protocol-valkey-default.md), [0017](docs/decisions/0017-noeviction-and-duplicate-reversal.md) |
| Platform and supply chain | [0009](docs/decisions/0009-redis-protocol-valkey-default.md), [0010](docs/decisions/0010-go-chi-spec-first-openapi.md), [0011](docs/decisions/0011-postgres-only-control-plane.md), [0014](docs/decisions/0014-apache-2-0-license.md) |
| Security and tenancy | [0012](docs/decisions/0012-hashed-scoped-api-keys.md), [0013](docs/decisions/0013-single-tenant-flat-model-no-pii.md) |
| Scope | [0015](docs/decisions/0015-release-slicing.md) |

## Conventions

- `NNNN-short-title.md`, sequential, never reused — including for rejected decisions.
- Status is one of Proposed, Accepted, Rejected, Superseded by ADR-NNN, Deprecated.
- Sections: Context · Options considered · Decision · Rationale · Consequences · Revisit when.
- A *Consequences* section that lists only benefits is a sign the record is incomplete.
- A new decision that changes behaviour updates [domain-rules.md](docs/product/domain-rules.md),
  [api-conventions.md](docs/architecture/api-conventions.md) or the
  [error catalogue](docs/product/error-catalog.md) in the same commit.

---

## Template: ADR-NNN — Title

- **Status:** Proposed | Accepted | Rejected | Superseded by ADR-NNN
- **Date:** YYYY-MM-DD
- **Deciders:** _Names or roles_
- **Affects:** _Links to the documents this decision constrains_
- **Context:** _What forces are at play? What constraints exist?_
- **Options considered:**
  - Option A — pros / cons
  - Option B — pros / cons
- **Decision:** _What we chose._
- **Rationale:** _Why this option wins._
- **Consequences:** _What becomes easier, harder, or newly possible._
- **Revisit when:** _The trigger that would reopen this._
