# Architecture Overview

The shape of the system in one document: the components, the boundaries, and why each boundary
exists. Detailed specifications live in the other architecture documents and are linked from
here; this document contains no specification detail that belongs elsewhere.

## 1. Design centre of gravity

Three properties are non-negotiable, and every structural decision in this system is a
consequence of one of them.

1. **The enforcement path must be correct and fast.** It runs on someone else's request, on
   every metered action, for a revenue-bearing decision. It is the product.
2. **The enforcement path must not depend on the administrative path.** An outage of a database
   full of configuration must not be able to stop enforcement for tenants already configured.
3. **Nothing may write enforcement state non-monotonically.** Every write is either idempotent or
   a strictly ordered transition, because workers, retries, races and restarts are all normal and
   none of them may produce money.

## 2. Context

```
                    ┌───────────────────────────────────────┐
                    │        Customer application           │
                    │  (owns users, sessions, entities,      │
                    │   pricing, invoices)                   │
                    └───────────────┬───────────────────────┘
                                    │ runtime-scoped key
                                    │ HTTPS, JSON, idempotency key
                                    ▼
        ┌───────────────────────────────────────────────────────────┐
        │                     Quotacore                              │
        │                                                           │
        │   ┌──────────────────┐      ┌──────────────────────────┐  │
        │   │  Data plane      │      │  Control plane           │  │
        │   │  /v1/consume     │      │  /v1/admin/*             │  │
        │   │  /v1/refund      │◀────▶│  features, plans,        │  │
        │   │  /v1/check       │ cfg  │  tenants, keys, audit    │  │
        │   │  /v1/balance     │      │                          │  │
        │   └────────┬─────────┘      └────────────┬─────────────┘  │
        │            │ atomic                        │ SQL           │
        │            ▼                               ▼               │
        │   ┌──────────────────┐      ┌──────────────────────────┐  │
        │   │ Redis-compatible │      │ PostgreSQL 16+           │  │
        │   │ store (Lua)      │      │ config, audit, ledger   │  │
        │   └──────────────────┘      └──────────────────────────┘  │
        │                                                           │
        │   ┌──────────────────┐                                     │
        │   │ Reset worker     │ observe + repair, same transition  │
        │   └──────────────────┘                                     │
        └───────────────────────────────────────────────────────────┘
                                    │
                    ┌───────────────▼────────────────┐
                    │  Optional, v0.3: outbound      │
                    │  signed webhooks to customer  │
                    │  endpoints (egress only)      │
                    └────────────────────────────────┘
```

**Egress is a product decision, not an implementation detail.** The MVP makes no outbound
connection to anything. Webhooks in v0.3 are the single exception, they are configured by the
operator, and they are the only thing that can leave the host. A self-hosted billing-relevant
component that phones home is a different product with a different trust story, and the trust
story is part of the value proposition (J-10).

## 3. The two planes

| | Data plane | Control plane |
| --- | --- | --- |
| Purpose | Answer and apply enforcement decisions | Define the offer and administer tenants |
| Store | Redis-compatible, Lua-atomic | PostgreSQL |
| Latency class | Sub-millisecond p50, single-digit p99 | Tens of milliseconds, irrelevant |
| Availability requirement | High; it is the revenue path | Moderate; an outage must not stop enforcement |
| Reads the other plane | Configuration from a bounded in-process snapshot | Never during enforcement |
| Writes the other plane | Ledger appends, asynchronous and best-effort | Full configuration, synchronous |
| Auth scope | `runtime` | `admin` |
| Fails by | `503 service_unavailable`, fail-closed | `503 control_plane_unavailable` for uncached tenants |

The separation is justified in ADR-0001 and enforced by the rule that **the enforcement path is
answered from the snapshot, and the only control-plane read it may perform is a single bounded
lookup on a cache miss** (DR-039). That rule is what makes the two-plane design real rather than
aspirational, and it is the first thing a reviewer should check.

### Why the split is not over-engineering

The alternative — one process reading configuration from Postgres and writing balances to Redis —
is simpler and is wrong in a specific, common way: a Postgres connection-pool exhaustion caused by
a control-plane query would then stall the enforcement path behind it. Splitting them means the
worst case of a slow admin page is a stale configuration, which the cycle model and the
monotonic rollover make harmless.

## 4. Components

| Component | Responsibility | Deployment |
| --- | --- | --- |
| HTTP server | Routing, auth, validation, request IDs, the error envelope | Same binary |
| Snapshot cache | Bounded in-process tenant and plan configuration, refreshed on an interval and invalidated by pub/sub | Same process |
| Script runner | `EVALSHA` invocation, SHA management, failover to `EVAL` on `NOSCRIPT` | Same process |
| Cycle engine | Pure functions from anchor, interval and zone to a window. No I/O, no clock reads | Same process |
| Admin store | Postgres access, transactions, migrations, audit writes | Same binary, separate pool |
| Ledger writer | Asynchronous append to `usage_events`, with a bounded queue and a drop counter | Same process |
| Reset worker | Periodic, optional, idempotent cycle advance for accuracy of reported windows | Same process, separately disableable |
| Webhook dispatcher | v0.3 only. Queue, backoff, dead-letter, HMAC signing | Same process |

One binary, one process, three background concerns that can each be disabled by configuration.
The alternative — separate worker and writer services — was rejected because each one would need
the cycle engine and the script runner, and duplicating those is a correctness risk, not a
decoupling.

## 5. Request path, end to end

The full sequence of a `POST /v1/consume`. Every step is specified in
[request-lifecycle.md](request-lifecycle.md); this is the shape.

```
 1. accept        read body, cap size, assign or adopt X-Request-Id
 2. authenticate  constant-time key hash lookup against the bounded key cache
 3. authorise     scope must include "runtime"
 4. validate      amount, feature key, window, and the presence of Idempotency-Key
 5. resolve       snapshot: tenant, plan entitlement, override   ← no I/O
 6. check gates   active, feature included, feature not archived
 7. script        one EVALSHA: idempotency, rollover, ceiling, deduct
 8. respond       200 with balance and window, or a typed error from step 7
 9. enqueue       ledger append, asynchronous, droppable          ← after the response
10. observe       metrics, structured log, optional trace
```

Two properties of this sequence carry the design:

- **Step 5 is the only configuration read and it does no I/O.** Everything needed to decide is
  already in memory. This is what bounds the tail latency independent of Postgres.
- **Steps 8, 9, 10 are in that order.** The response is written before the ledger append is
  attempted. A metered action that has been applied atomically cannot be un-done by failing the
  response, because the client would then retry and be charged twice (FS-10).

## 6. Storage model

Full schema in [data-model.md](data-model.md). In outline:

**PostgreSQL** holds configuration, audit and the ledger. It is the source of truth for *what
should be true*. Configuration tables are small — features, plans, entitlements, tenants, keys —
and are read only by the control plane. The ledger is append-only and grows without bound within
its retention window.

**The Redis-compatible store** holds what *is* true at runtime: balances, cycle state, and
idempotency records. It is small, hot, and entirely reconstructable from configuration plus the
ledger, which is the property that makes the restore drill in
[consistency-and-recovery.md](consistency-and-recovery.md) possible.

That asymmetry is deliberate: the small hot store holds the state whose loss is urgent but
recoverable, and the large durable store holds the state whose loss is unacceptable.

## 7. Key design decisions and where they are argued

| Decision | ADR | Consequence visible in the architecture |
| --- | --- | --- |
| Control/data split | [0001](../decisions/0001-control-plane-data-plane-split.md) | Two failure domains, two error codes, no DB on the hot path |
| Lua for atomicity | [0002](../decisions/0002-lua-scripts-for-atomicity.md) | One script per operation; the entire correctness argument is in those scripts |
| Lazy monotonic rollover | [0003](../decisions/0003-lazy-monotonic-cycle-rollover.md) | No cron dependency for correctness; the worker is optional; TTL is GC only |
| Idempotency by key | [0004](../decisions/0004-idempotency-prevention-and-detection.md) | A required request header, a Redis record, a 24 h bound, `409` on reuse |
| Refund first class | [0005](../decisions/0005-refund-as-first-class-operation.md) | Runtime scope can credit, bounded by the `limit + bonus` ceiling |
| Plan edits at boundary | [0006](../decisions/0006-plan-edits-at-next-cycle-boundary.md) | Two behaviours for one entity, deliberately: assignment hard-resets, edits wait |
| Per-tenant time zone | [0007](../decisions/0007-per-tenant-timezone.md) | Embedded tzdata, UTC-pinned host maths, calendar arithmetic in Go |
| Anchored and rolling | [0008](../decisions/0008-cadence-model-anchored-and-rolling.md) | One cadence in the MVP, six intervals, a second cadence in v0.2 |
| Redis protocol, Valkey default | [0009](../decisions/0009-redis-protocol-valkey-default.md) | Engine-agnostic code, BSD-default image, documented Redis licence change |
| Spec-first OpenAPI | [0010](../decisions/0010-go-chi-spec-first-openapi.md) | The spec is the contract, generated docs, `go-chi` + `net/http` |
| Postgres-only control plane | [0011](../decisions/0011-postgres-only-control-plane.md) | One database, embedded migrations, no second SQL store |
| Hashed scoped API keys | [0012](../decisions/0012-hashed-scoped-api-keys.md) | No recoverable secrets, two scopes, no recovery path by design |
| Single-tenant flat model | [0013](../decisions/0013-single-tenant-flat-model-no-pii.md) | No users, no orgs, no PII, no multi-tenancy in the product sense |
| Apache-2.0 | [0014](../decisions/0014-apache-2-0-license.md) | Permissive, patent grant, survives being vendored |
| Release slicing | [0015](../decisions/0015-release-slicing.md) | Five releases, each independently useful |
| Usage event ledger | [0016](../decisions/0016-usage-event-ledger.md) | Best-effort history, visible gaps, full-allowance recovery |

## 8. Deliberate exclusions from the architecture

Listed because an architecture document that only describes what exists invites the reader to
assume the omissions were oversights.

- **No multi-region, no cross-region replication.** Single region, single primary. Rationale and
  the resulting latency envelope in [non-functional-requirements.md](non-functional-requirements.md).
- **No horizontal scale-out of the data plane.** The data plane is a single logical instance with
  per-tenant keys, so adding a second instance is a Redis Cluster or sentinel arrangement, not an
  application-level sharding problem. Stated plainly so nobody designs a sharded API on top of it.
- **No read replicas in the enforcement path.** A replica read is a stale read, and a stale
  balance is a wrong answer.
- **No service mesh, no Kubernetes manifests, no sidecar pattern in the reference deployment.**
  The supported deployment is `docker compose up`. A Kubernetes chart is a post-MVP contribution
  with a named maintainer, not a first-class artefact.
- **No multi-tenancy inside one instance.** One customer per instance (ADR-0013). This is a
  security boundary and a licensing simplification, not a scalability limit.
- **No plugin system, no webhook-in mechanism, no expression language for limits.** Every
  extension point in the design costs support forever.

## 9. Physical view

```
        ┌──────────────── host ─────────────────┐
        │                                       │
        │  quotacore (single binary)            │
        │    :8080 http        :9090 metrics    │
        │    worker, ledger writer, dispatcher  │
        │                                       │
        │  ┌───────────────┐ ┌───────────────┐  │
        │  │ postgres:16   │ │ valkey        │  │
        │  │ volume: pg    │ │ volume: data  │  │
        │  └───────────────┘ └───────────────┘  │
        │                                       │
        └───────────────────────────────────────┘
```

Sizing, resource envelopes, persistence and backup responsibilities are in
[deployment.md](deployment.md). Health and readiness semantics are in
[observability.md](observability.md).

## 10. Reading order

A reader implementing this should read, in order:

1. This document, for the shape.
2. [domain-rules](../product/domain-rules.md), for what must be true. Everything else is
   subordinate to it.
3. [data-model.md](data-model.md) and [cycle-engine.md](cycle-engine.md), for the two pieces of
   state everything else manipulates.
4. [request-lifecycle.md](request-lifecycle.md) and [api-conventions.md](api-conventions.md), for
   the contract.
5. [consistency-and-recovery.md](consistency-and-recovery.md), for what happens when things break.
6. [testing-strategy.md](testing-strategy.md), for how the claims in this document are proven.
