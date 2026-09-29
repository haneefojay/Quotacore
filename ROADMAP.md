# Roadmap

Quotacore · 2026-09-27

Direction over dates. The sequencing is dependency-driven and the gates are explicit: the point of
this document is to make visible what each phase unlocks, and what it must not contain. Release
boundaries are decided in [ADR-0015](docs/decisions/0015-release-slicing.md) and specified per
release in [mvp-scope.md](docs/product/mvp-scope.md).

## Current position

**Specification complete; implementation started.** 63 documents, 17 accepted decisions, 15
named correctness tests, 34 error codes, 49 domain rules — and the first code, in
[`IP-01`](docs/roadmaps/v0-1-enforcement-path.md#ip-01--repository-toolchain-and-ci-foundation),
which owns no product behaviour. See
[the register](docs/product/assumptions-and-open-questions.md#2-open-questions).

## Execution plan

This document is release slicing: **what** ships, in **which** release, and what it must not contain.
The order of the work inside a release, and what proves each piece of it finished, is in
[docs/roadmaps/](docs/roadmaps/roadmap-index.md) — 28 phases, `IP-00` to `IP-27`, with `IP-00` and
`IP-01` `COMPLETE` as of 2026-09-27 and `IP-02` and `IP-03` unblocked. Read the index before
starting anything; a phase is not the same thing as a release, and this file does not change when a
phase does.

| Release | Phases | Document |
| --- | --- | --- |
| v0.1 | `IP-00`-`IP-16` | [the enforcement path](docs/roadmaps/v0-1-enforcement-path.md) |
| v0.2 | `IP-17`-`IP-20` | [entitlements and rolling windows](docs/roadmaps/v0-2-entitlements-and-rolling-windows.md) |
| v0.3 | `IP-21`-`IP-23` | [outbound notification](docs/roadmaps/v0-3-webhooks.md) |
| v0.4 | `IP-24`-`IP-25` | [the embedded operator interface](docs/roadmaps/v0-4-admin-ui.md) |
| v0.5 | `IP-26`-`IP-27` | [client libraries](docs/roadmaps/v0-5-sdks.md) |

## Gate 0 — unblock the specification

Closed on 2026-09-27. Every item below was a question about the contract rather than about the code,
and each now points at the document that answers it.

- [x] **Q-01** — an admin grant does not survive a plan change. It is cycle-scoped, and a plan change
      starts a new cycle ([DR-013](docs/product/domain-rules.md), [UC-06](docs/product/use-cases.md)).
      Already decided when the question was raised; the question recorded that nobody had looked.
- [x] **Q-02** — the balance after a plan change is the new limit only. The old balance is discarded
      and proration is deferred ([DR-013](docs/product/domain-rules.md),
      [UC-06](docs/product/use-cases.md)). Also already decided.
- [x] **Q-04** — yes, above a configurable threshold. `apply-now` returns
      `409 confirmation_required` and applies nothing
      ([DR-046](docs/product/domain-rules.md), [T-12](docs/architecture/testing-strategy.md)).
- [x] **Q-14** — yes: `GET /v1/admin/plans/{id}/impact` reports the affected count and the earliest
      affected `cycle_end` ([UC-19](docs/product/use-cases.md)).
- [x] **The data-store eviction question ([Q-21](docs/product/assumptions-and-open-questions.md#2-open-questions))**
      — `noeviction`, sized for the 24-hour window, with capacity exhaustion surfacing as a `503` and
      an automatic reversal for a duplicate detected after total store loss
      ([ADR-0017](docs/decisions/0017-noeviction-and-duplicate-reversal.md)).
- [x] **A-01** moved off this gate. It is not answerable by writing and not answerable by coding, so
      holding the repository hostage to five conversations would have guaranteed it happened later.
      It is carried as a dated, owner-accepted release risk
      ([accepted risks](docs/product/assumptions-and-open-questions.md#accepted-risks)) and it is a
      **v0.1 release** blocker, not a code-start one: the first release is the test of the bet, and
      the re-check is due 2026-12-31.

## v0.1 — the MVP

One job, done correctly: **metered enforcement on the request path, with a calendar reset.** Nothing
else ships.

- [ ] `api/openapi.yaml`, spec-first, with validation generated from it
- [ ] Cycle engine and its boundary test matrix — the component most likely to hide a silent,
      permanent defect
- [ ] Atomic enforcement scripts: `consume`, `refund` and `check` in the binary, with the balance
      read served from the fast store in one round trip
- [ ] Snapshot cache with generation-versioned invalidation
- [ ] Control plane: tenants, features, plans, entitlements, API keys, audit
- [ ] Plan `apply` with the `409 confirmation_required` handshake above the configured threshold, and
      the read-only `GET /v1/admin/plans/{id}/impact` preview ([Q-04](docs/product/assumptions-and-open-questions.md#2-open-questions),
      [Q-14](docs/product/assumptions-and-open-questions.md#2-open-questions))
- [ ] Embeddable CLI for tenant, plan, feature, key and status management
- [ ] Signed-delta usage ledger, writer, optional replica, archiver
- [ ] Metrics, structured logs, the two money counters, a counter for each of the 34 error codes
- [ ] Docker image, `docker compose up` reference deployment, migration and restore rehearsal
- [ ] All 15 correctness tests and the 7 further suites, in CI, with the three headline tests — [T-01](docs/architecture/testing-strategy.md#t-01--atomic-decrement-under-contention),
      [T-02](docs/architecture/testing-strategy.md#t-02--idempotent-replay) and
      [T-09](docs/architecture/testing-strategy.md#t-09--atomicity-under-failure) — written first

**Exit criteria.** `docker compose up` produces a working enforcement path; the three headline
tests pass under `-race`; 200 concurrent requests against a balance of 100 allow exactly 100; a
server idle across three boundaries grants one allowance and emits one event; no plaintext secret
exists in any persistent store.

**Explicitly excluded from v0.1.** Boolean entitlements, rolling windows, webhooks, UI, SDKs,
fractional units, reservations, accumulation.

## v0.2 — entitlements and rolling windows

The two features most often asked for, and the two that change the data model rather than extend
it. Both ship together because the keyspace and the script interfaces are designed for them now and
used properly only then.

- [ ] Boolean features: assign or not, with the limit field irrelevant
- [ ] `set` and `grant` as first-class operations
- [ ] Rolling windows on a fixed window or a sliding one, on one key per tenant-feature
- [ ] Metrics that distinguish "not in plan" from "limit of zero"
- [ ] Per-tenant runtime API keys, closing gap 1 in [SECURITY.md](SECURITY.md#6-known-gaps)

**Exit criteria.** A boolean check costs the same as a metered one; a sliding window is correct
under concurrency; a per-tenant runtime key cannot address another tenant.

## v0.3 — outbound webhooks

The moment the service stops being purely internal, and the only egress it will ever have.

- [ ] Operator-configured signed HTTPS endpoints: HMAC, timestamp, JTI, documented replay window
- [ ] At-least-once delivery, bounded retries with backoff, dead-letter visibility
- [ ] SSRF protections: `https` only, private and link-local ranges refused, no redirects, DNS
      re-checked on connect
- [ ] Delivery metrics and a queue-depth alert

**Exit criteria.** A receiver can verify a delivery, replay it, and prove what it processed.

## v0.4 — embedded admin UI

A single embedded bundle, no build pipeline, no framework, and no server-rendered user content
without a strict CSP. Small on purpose: operators can already do everything with the CLI.

- [ ] Tenant, plan, feature, entitlement and key management
- [ ] Usage view backed by the ledger, including visible sink gaps
- [ ] The security requirements recorded in [security-model.md](docs/architecture/security-model.md#6-application-level-threats):
      no `innerHTML` of server data, strict CSP, no user-controlled redirects

## v0.5 — SDKs

Generated from the OpenAPI document, so they cannot drift from it.

- [ ] TypeScript: `check` and `consume`, typed errors, idempotency handled for the caller
- [ ] Python: the same surface, with `Decimal` handled without float drift
- [ ] Both with a `QuotaExceeded` distinct from a transport error, because conflating them is how
      a customer ends up disabling enforcement

## Beyond — candidates, not commitments

Ordered by how often they are asked for, not by value. Each is deliberately absent from the
releases above.

| Candidate | Why it is not sooner |
| --- | --- |
| Usage accumulation and rollover | A materially more complex cycle model, and a worse default |
| Fractional and decimal units | `int64` in the feature's unit is simpler to reason about; a `Decimal` type is a fine minor release |
| Quota reservations and holds | Real, and a genuinely hard concurrency problem |
| Per-second sliding-window rate limiting | Requires hot-path sliding counters; the truthful calendar model was chosen instead |
| Outbound webhooks with delivery guarantees beyond at-least-once | Requires an outbox and a scheduler, which is a different class of system |
| Cross-region replication | The data lives on the customer's host; there is nothing to replicate across regions |
| Sharded or clustered data plane | A sharding scheme would be worse than not having one. A managed data store is the answer past 10,000 requests per second |
| A billing provider integration | Out of scope permanently. Enforcement and billing are different products |

## Dependencies and parallel work

```
Gate 0 ──▶ v0.1 ──┬─▶ v0.2 ──▶ v0.3
                 ├─▶ v0.4        (independent of v0.2 and v0.3)
                 └─▶ v0.5        (needs a stable contract; parallel with v0.4)
```

Three pieces of work can run in parallel from day one of v0.1: the cycle engine, the script
interfaces, and the test harness. The CLI, the UI and the SDKs cannot — all three are downstream of
a stable contract, which is the whole reason the contract is written first.

## Status legend

| Marker | Meaning |
| --- | --- |
| Not started | No work begun |
| In progress | Actively being built |
| Blocked | Waiting on something, with the thing named |
| Done | Shipped and verified against its exit criteria |
