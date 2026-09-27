# ADR-0011 — The control plane is PostgreSQL only, with embedded up-only migrations

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [data model](../architecture/data-model.md), [overview](../architecture/overview.md), [research: Go stack](../research/go-stack-and-dependency-selection.md), [deployment](../architecture/deployment.md)

## Context

The control plane holds features, plans, entitlements, tenants, overrides, API keys, the audit
log and the usage event ledger. It has no latency budget, modest write volume, and strong
relational requirements: a plan entitlement must reference a real feature, a tenant must
reference a real plan, and an override must reference both.

## Options considered

**A — PostgreSQL (chosen).**
- Pros: transactions across related writes, foreign keys that make invalid configuration
  unrepresentable, mature backup and replication story, universally available in every cloud and
  in `docker compose`, and the operational skill set almost every engineering team already has.
- Cons: another service to run.

**B — PostgreSQL with a dual SQLite driver for local development and single-node installs.**
- Pros: a zero-server install mode, which is genuinely attractive for a self-hosted sidecar.
- Cons: **two dialects to write against and two dialects to test.** SQLite has no native enum,
  different type affinity, different `ON CONFLICT` behaviour, different date handling, no
  `SELECT ... FOR UPDATE SKIP LOCKED`, and a different concurrency model. The config store is
  low-complexity but this is a disproportionate amount of conditional SQL, and a
  dialect-specific bug would only appear in the mode fewer people test.

**C — SQLite only.** Rejected: forfeits transactions across related writes, which is exactly
  what a configuration store needs, and the plan-change fan-out in ADR-0006 is a transaction.

**D — Store configuration in the same Redis instance.** Rejected outright: it discards
  durability and relational integrity for data whose whole purpose is to be the durable
  definition of what customers bought.

**E — A document store.** Rejected: the model is genuinely relational, and modelling it on
  documents means enforcing the referential rules in application code.

## Decision

**PostgreSQL, one driver, one dialect, no abstraction layer over SQL.**

- Minimum supported version: **PostgreSQL 16**, which is the version supported by the current
  major managed offerings and by the container image used in development.
- Queries use `pgx/v5` directly. No ORM. The control plane is a small number of tables with
  unambiguous relationships, and an ORM would add a translation layer between the specification
  in [data-model](../architecture/data-model.md) and what actually executes, which is the
  opposite of what this document set is for. Where SQL is genuinely complex, it is written as
  SQL and marked as such.
- Every write that spans tables runs in a single transaction with the appropriate isolation
  level. The plan fan-out in ADR-0006 is chunked into bounded transactions rather than run as
  one long transaction, so it cannot hold locks for the length of a large job.

## Migrations

- Migration SQL lives in `migrations/*.sql` and is **embedded in the binary**. No migration
  container, no separate CLI, no volume-mounting of schema files. A single binary that can
  upgrade its own schema is the correct shape for a self-hosted product.
- The embedded runner is **`pressly/goose`** in library mode. Rejected alternatives: writing a
  runner (a schema_migrations table and a checksum check is not where bespoke code belongs when
  a maintained library does it correctly), and a sidecar migration container (breaks the
  single-binary promise and introduces a version-matching problem with the running binary).
- **Up-only in production.** `down` migrations exist in the file set for local development
  convenience, and the server refuses to apply them when `environment=production`. Rationale:
  this database holds enforcement state and a ledger. A down migration that drops a
  `usage_events` partition destroys billing history irreversibly, and the availability risk of
  a rollback-in-place is worse than the operational cost of rolling back the binary.
- Migrations run at startup under a **Postgres advisory lock**, so N replicas starting
  simultaneously produce exactly one migration run. `migrate=false` disables automatic
  migration for operators who run migrations as a separate deploy step.
- Checksums are recorded. A migration file edited after it has been applied is a startup error,
  not a silent divergence.

## Rationale

The strongest argument against PostgreSQL was that it adds a service to a product whose pitch
is simplicity. The argument against the dual-driver option is that it converts a small operational
cost into a permanent correctness surface. One dialect, tested in CI, on one supported version
is the cheaper total.

Restricting the production migration path to up-only is a deliberate reduction in capability. A
rollback is performed by rolling back the binary, which is what a customer running a container
image expects, and it is a rollback that cannot destroy the ledger.

## Consequences

**Positive**
- Invalid configuration is unrepresentable: foreign keys reject an entitlement for a deleted
  feature or a tenant pointing at a deleted plan.
- One dialect means the test matrix is one matrix.
- The binary upgrades its own schema, so the upgrade path is `docker compose pull && docker
  compose up`, with a documented manual path for operators who gate deploys.
- The audit log and the event ledger live in the same transactional store as the configuration
  they describe, so an audit record cannot be orphaned from the change it records.

**Negative**
- Two services to deploy. The quick start is two backing containers plus the app.
- PostgreSQL 16 minimum excludes some very old managed instances. Documented; a startup version
  check fails fast with the exact requirement.
- No SQLite "just run the binary" mode. This was a real product benefit given up for
  correctness, and it is the main thing a "lite" distribution would have to solve.
- Automatic migration at startup means the application needs DDL privileges on its own database,
  which some enterprise security policies disallow. The `migrate=false` path exists for that
  case, and it is documented as a supported deployment mode.

## Revisit when

- A majority of deployments are proven to be single-node and low-traffic, which would make a
  lite distribution with an embedded store worth building.
- The control-plane write path becomes a bottleneck, which would require read replicas for the
  admin and reporting queries.
