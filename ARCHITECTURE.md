# Architecture

Quotacore · 2026-09-27

The system in one sentence: **one Go binary holding two deliberately non-interacting planes — a
PostgreSQL control plane that is allowed to be slow, and a Redis-compatible data plane that is not
allowed to be.**

Normative detail is in [docs/architecture/](docs/architecture/); this document is the map. Where
they differ, they are the specification and this is the map.

## 1. Overview

A customer's application makes one call per metered action. That call must answer yes or no in
about a millisecond, must not depend on anything that can be slow, and must not be able to half-
apply. Three architectural commitments follow, and everything else in this repository exists to
protect them.

**The split.** Configuration lives in PostgreSQL and is read into bounded in-process snapshots.
Enforcement is answered from those snapshots, and the only control-plane read it may perform is a
single bounded lookup on a cache miss. A configuration change reaches the data plane in
milliseconds, while a control-plane outage does not reach enforcement at all —
[ADR-0001](docs/decisions/0001-control-plane-data-plane-split.md).

**Atomicity is a datastore feature, not ours.** Check, deduct, record, advance the cycle and append
the ledger delta are one Lua script execution. There is no read-modify-write in the request path
anywhere, and therefore no interleaving to reason about —
[ADR-0002](docs/decisions/0002-lua-scripts-for-atomicity.md).

**Time is computed, not scheduled.** The current cycle is derived from a tenant's anchor by
calendar arithmetic in Go, and a rollover is one monotonic, guarded atomic transition. A server
that was down at midnight does not need a catch-up job to be correct —
[ADR-0003](docs/decisions/0003-lazy-monotonic-cycle-rollover.md).

## 2. Context diagram

```
                       ┌──────────────────────────────────────────┐
                       │  Quotacore instance (one per customer)   │
                       │                                          │
  customer app ───────▶│  ┌──────────────┐    ┌────────────────┐  │
  (the hot path)      │  │ data plane   │───▶│ Redis/Valkey   │  │──┐
                       │  │ runtime key  │    │ atomic scripts │  │  │
  operator ──────────▶│  │ admin key    │    └────────────────┘  │  │
  (CLI, curl)         │  └──────┬───────┘                         │  │
                       │         │ bounded snapshot              │  │
                       │  ┌──────▼───────┐    ┌────────────────┐  │  │
                       │  │ control      │───▶│ PostgreSQL     │──┼──┤
                       │  │ plane        │    │ up-only schema │  │  │
                       │  │ admin key    │    └────────────────┘  │  │
  analysts ──────────▶│  └──────┬───────┘                         │  │
                       │         │ async, at-least-once          │  │
                       │  ┌──────▼──────────────────────────────┐│  │
                       │  │ usage event buffer (deque + chan)   ││  │
                       │  │ writer │ replica │ archiver         ││  │
                       │  └──────────────────────────────────────┘│  │
                       └──────────────────────┬───────────────────┘  │
                                              │ SQL only             │
                       ┌──────────────────────▼───────────────────┐  │
                       │  customer's own database (read-only)    │──┘
                       │  billing, entitlements, analytics        │
                       └──────────────────────────────────────────┘

  No network egress, no telemetry, no control plane.
```

The only outbound network connections in the system are to the customer's own database, and they
are read-only. The sole planned egress of any kind is the signed webhook in v0.3 —
[NFR-S3](docs/architecture/non-functional-requirements.md#5-security).

## 3. Components

| Component | Responsibility | Tech | Location | Plane |
| --- | --- | --- | --- | --- |
| HTTP API | Routing, auth, validation, error mapping, request IDs | `net/http` + `chi` | `internal/api` | both |
| Enforcement handler | `consume`, `refund`, `check`, `balance` | Go | `internal/api` | data |
| Atomic scripts | All state transitions, in one execution each | Lua, `EVALSHA` | `internal/store/scripts` | data |
| Snapshot cache | Bounded, TTL'd, LRU, generation-versioned | in-process | `internal/snapshot` | data |
| Cycle engine | Pure anchor-to-boundary calendar arithmetic | Go, no I/O | `internal/cycle` | shared |
| Rollover worker | Opportunistic pre-advance; optional | Go | `internal/worker` | data |
| Event buffer | In-memory deque plus channel; drops rather than blocks | Go | `internal/events` | data |
| Event writer, replica, archiver | Append to the ledger, fan out, prune | Go + `pgx` | `internal/ledger` | control |
| Admin handler | Tenants, plans, features, keys, audit, status | Go | `internal/api/admin` | control |
| Configuration store | Authoritative configuration, up-only migrations | `pgx`, embedded `goose` | `internal/db` | control |
| Migrations | Embedded, up-only, applied at start-up | `goose` | `internal/db/migrations` | control |
| Metrics | Prometheus registry and `/metrics` | Go | `internal/observability` | both |
| Embedded UI | v0.4 only, behind a build tag | Go `embed` | `internal/ui` | control |

**One binary, one process, three background concerns.** There is no message broker, no job queue
service, no cache tier and no sidecar. Background work runs in goroutines inside the same process,
and every one of them is degradable: the enforcement path is correct with all three switched off.

## 4. Data model

Full DDL, keyspace and rationale: [data-model.md](docs/architecture/data-model.md). Ownership
summary:

| Store | Authoritative for | Never |
| --- | --- | --- |
| PostgreSQL | Configuration, tenants, plans, features, entitlements, API keys, audit log, the signed-delta ledger | Request-path reads |
| Redis / Valkey | The current balance, the idempotency record, the current cycle window | Configuration, keys, audit |
| In-process | A bounded, disposable copy of configuration | Anything that must survive a restart |

The only durable state that both planes need to agree on is the ledger, and they agree on it by
PostgreSQL being the single writer. The data plane's balance is a projection that is authoritative
while the instance is running and rebuildable when it is not — and that is stated plainly rather
than dressed up, because the distinction is the difference between an SLO and an incident.

## 5. Interfaces

**Inbound.** `POST /v1/consume`, `POST /v1/refund`, `POST /v1/check`, `GET /v1/balance` on the
runtime key. `GET` and `POST` under `/v1/admin/...` for tenants, plans, features, entitlements,
keys, audit, impact and status on an admin key. `/healthz` and `/metrics` unauthenticated and
loopback-or-network-restricted by default. Conventions: [api-conventions.md](docs/architecture/api-conventions.md).

**Outbound.** SQL to the customer's database, read-only, for the event replica. Signed HTTPS
webhooks in v0.3, opt-in. Nothing else, ever.

## 6. Runtime flow

The `consume` path, in order, with the failure behaviour that matters:

1. **Authenticate and authorise.** Named key, `runtime` scope, constant-time Argon2id verify.
2. **Validate.** Body schema, then `external_id` and `feature_key` against the snapshot. Rejection
   is `422` or `404`, before any state is touched.
3. **Resolve configuration.** Snapshot lookup, O(1), in memory. A miss triggers one synchronous
   refresh; a second miss is `503`, not a database read on the hot path.
4. **Compute the cycle.** Pure function of `now`, the anchor and the interval. No I/O, no
   dependence on the previous boundary, and the same answer for the same input on every machine.
5. **Execute one script.** Check, deduct, record the idempotency entry, advance the cycle if stale,
   append the signed-delta event, all in a single execution with no `await` between them.
6. **Reply.** Typed success, or one of 34 typed failures with a code, a message, a request ID
   and a documented client action — [error catalogue](docs/product/error-catalog.md).

Steps 1, 2 and 6 are boring, and that is the design. Everything that can be slow, lossy or
interruptible has been pushed out of steps 3 to 5.

## 7. Infrastructure

| Environment | Purpose | Hosting |
| --- | --- | --- |
| local | Development and tests, `docker compose` | Docker, or a native Go build plus a containerised data store |
| CI | `go test -race`, integration suites, container smoke test | Linux container |
| pilot | Design-partner instance | Customer's host or a small VM |
| production | One instance per customer | The customer's own infrastructure |

The product's own footprint: one static binary, one PostgreSQL, one Valkey, one volume, one
port. No managed service is required at any point, and none is recommended.

## 8. Scaling and reliability

**Where the bottlenecks are.** The datastore's single-threaded script execution, then the network
hop, then argument validation. Nothing else is close — which is why
[performance.md](docs/architecture/performance.md#2-what-the-script-costs) fixes the script at O(1)
with no key iteration.

**Degradation, honestly.**

| Failure | Behaviour | Cost |
| --- | --- | --- |
| PostgreSQL down | Enforcement continues from snapshots; admin and writes fail | Zero impact on traffic |
| Redis or Valkey down | Fail closed, `503` in under 250 ms, no partial state | 100% of enforcement — deliberate |
| Whole data store lost | Rebuild from configuration plus ledger; a number derived that way is flagged approximate | Flagged, never silent |
| Memory pressure | Bounded caches, LRU eviction, OOM-avoiding ring buffers | Degradation, not failure |
| Clock skew | The server is the authority; a monotonic guard rejects a backwards cycle index | Rejected, typed |

**Scaling, and the limit that matters.** The data plane scales up to 10,000 requests per second on
one instance. Past that, the answer is a managed Redis-compatible service with replication, not
sharding this service. A customer with several instances partitions tenants across them and accepts
one balance per tenant — a trade that is stated rather than hidden.

## 9. Observability

Structured JSON logs with a request ID on every line, Prometheus metrics, and no tracing, because
there is no cross-service call to trace. Two metrics are treated as money counters and are alerted
on at zero: `quotacore_event_duplicate_detected_total` and `quotacore_ceiling_violation_total`.
Every one of the 34 error codes has a counter, and the alert table holds 16 alerts covering the
failures worth waking someone for. Full list, cardinality
discipline and the alert table: [observability.md](docs/architecture/observability.md).

## 10. Detailed docs

Read in this order to build it, or in this order to review it:

| # | Document | What it settles |
| --- | --- | --- |
| 1 | [overview.md](docs/architecture/overview.md) | Component map, request paths, dependency rules |
| 2 | [non-functional-requirements.md](docs/architecture/non-functional-requirements.md) | Every number, and how it is measured |
| 3 | [data-model.md](docs/architecture/data-model.md) | DDL, keyspace, TTLs, ownership |
| 4 | [cycle-engine.md](docs/architecture/cycle-engine.md) | The boundary algorithm and its test matrix |
| 5 | [request-lifecycle.md](docs/architecture/request-lifecycle.md) | The hot path, step by step, with failure modes |
| 6 | [api-conventions.md](docs/architecture/api-conventions.md) | Wire format, idempotency, errors, versioning |
| 7 | [consistency-and-recovery.md](docs/architecture/consistency-and-recovery.md) | Failure modes and recovery, item by item |
| 8 | [security-model.md](docs/architecture/security-model.md) | Trust boundaries and controls |
| 9 | [observability.md](docs/architecture/observability.md) | Metrics, logs, alerts |
| 10 | [deployment.md](docs/architecture/deployment.md) | Images, configuration, upgrades, backup |
| 11 | [performance.md](docs/architecture/performance.md) | Budgets and the measurement plan |
| 12 | [testing-strategy.md](docs/architecture/testing-strategy.md) | The 15 named correctness tests and the 7 further suites that prove the claims |

Decisions and their reasoning: [TECHNICAL-DECISIONS.md](TECHNICAL-DECISIONS.md). Risk:
[SECURITY.md](SECURITY.md).
