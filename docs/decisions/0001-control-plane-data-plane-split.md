# ADR-0001 — Separate the control plane from the data plane

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [ARCHITECTURE.md](../../ARCHITECTURE.md), [request lifecycle](../architecture/request-lifecycle.md), [overview](../architecture/overview.md)

## Context

Quotacore serves two very different kinds of request. The overwhelming majority are
**enforcement** requests: `POST /v1/consume`, arriving on the critical path of the
customer's application, budgeted at single-digit milliseconds and required to be
correct under concurrency. A tiny number are **configuration** requests: create a plan,
assign a tenant, grant a bonus. Configuration happens minutes or hours apart and has no
latency budget.

The two have opposite requirements. Enforcement needs very high write throughput with no
disk I/O in the request path. Configuration needs durability, relational integrity,
arbitrary query and audit. Forcing both onto one datastore means either an over-provisioned
write-optimised store with weak relational guarantees, or a relational store whose p99 under
row-lock contention cannot meet the enforcement budget.

## Options considered

**A — One store for everything.** Postgres holds plans, tenants and balances; enforcement is
`UPDATE tenant_balances SET balance = balance - $1 WHERE tenant_id = $2 AND balance >= $1`.
- Pros: one dependency, one backup, one transaction model, trivially consistent, balance history
  queryable with SQL.
- Cons: a hot tenant serialises on one row. p99 under contention grows without bound and is
  dominated by disk I/O and lock waits, which is precisely the number this product sells on.
  The enforcement path also inherits the availability characteristics of the configuration store.

**B — Two stores, split by access pattern (chosen).** Postgres is the system of record for
configuration and audit. A Redis-compatible store holds the enforcement state, mutated only by
server-side Lua scripts. Postgres is never consulted on the enforcement path.
- Pros: enforcement is a single in-memory round trip with no disk I/O and no cross-request
  locking. A slow or unavailable Postgres does not stop enforcement of already-configured
  tenants. Configuration gains full relational tooling and transactional integrity.
- Cons: two dependencies to operate, two backup stories, and a real risk of divergence between
  the two stores that must be actively managed (see ADR-0003 and ADR-0016).

**C — Two stores plus a local cache of configuration in every process.** As B, plus an
in-process snapshot of the configuration that the enforcement path reads instead of Redis.
- Pros: faster, removes the remaining Redis round trip for configuration lookups.
- Cons: a third place configuration lives, requiring explicit invalidation. Not justified at
  the scale a Redis round trip of roughly 100 microseconds already satisfies; rejected in its
  own right, but the *idea* of a configuration snapshot survives in a much smaller form.

## Decision

**Option B, with the specific refinement that a bounded, in-process configuration snapshot sits
in front of the data plane rather than replacing it.**

The enforcement path reads tenant configuration from an in-process, bounded, LRU-evicted cache
populated by a background refresh, invalidated by Redis pub/sub on control-plane writes, and
written through synchronously on local writes. It then computes the current cycle window and
issues exactly one Lua round trip.

The data plane is not a separate deployable. It is a set of keys in the same Redis-compatible
instance and a set of packages in the same binary. "Two planes" is a statement about which
datastore owns which class of data and which code path may touch it, not about process topology.

## Rationale

Splitting on access pattern is the only one of the three options that satisfies both halves of
the requirement: sub-millisecond enforcement with provable atomicity, and durable relational
configuration with real integrity. The snapshot refinement exists because the previous project
history contained a contradiction: it asserted that the data plane never touches Postgres while
also requiring the lazy reset path to read a plan limit from Postgres on a cache miss. Caching
the configuration in-process resolves the contradiction — a cache miss for a tenant that is
genuinely absent from the snapshot returns `404 tenant_not_found` and is never served from
Postgres on the hot path.

The snapshot is bounded and read-mostly, so it does not introduce a distributed-consistency
problem of its own. It is a cache of a system of record that already exists, invalidated by
event, with a periodic full refresh as a correctness backstop.

## Consequences

**Positive**
- Enforcement latency is one in-memory round trip. No disk, no lock waits, no cross-tenant
  contention.
- A Postgres outage degrades configuration and new-tenant provisioning but does not stop
  enforcement for tenants already in the snapshot.
- Configuration retains foreign keys, transactions, and `SELECT`-based reporting.
- The snapshot gives a natural home for the cycle-window computation, which is pure logic.

**Negative**
- Two datastores to deploy, secure, back up, and monitor. The quick-start is `docker compose up`
  with two backing services, not one.
- Divergence between the stores is a real failure mode. It is managed explicitly: a
  monotonically advancing, idempotent rollover script (ADR-0003) and an append-only delta ledger
  in Postgres (ADR-0016) that makes the authoritative balance reconstructable.
- A tenant that is missing from the snapshot is not enforceable, so provisioning has a
  propagation cost that must be specified and tested. It is specified as a ≤ 1 s p99 with a
  hard bound of the refresh interval, and the parent application is required to tolerate it.
- Two stores mean two partial failure modes to document, in [SECURITY.md](../../SECURITY.md) and
  the failure matrix in [ARCHITECTURE.md](../../ARCHITECTURE.md).

## Revisit when

- Enforcement traffic on a single instance exceeds roughly 50,000 requests per second, at which
  point the snapshot refresh cost and the single Redis node become the binding constraints.
- The customer base shifts to multi-region, where a single Redis primary cannot serve
  sub-millisecond reads from every region. That would force either a per-region store with
  partitioned ownership or a change of consistency model.
- A customer with a single-tenant, low-traffic deployment profile proves dominant, which would
  make the embedded-store option in ADR-0009's alternatives worth revisiting.
