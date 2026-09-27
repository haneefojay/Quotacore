# Product Requirements Document

Quotacore · v0.1.0 · 2026-09-27

Normative scope and release boundaries live in
[mvp-scope.md](docs/product/mvp-scope.md); normative behaviour lives in
[domain-rules.md](docs/product/domain-rules.md). This document states the requirements and points
at those. Where they differ, those are the specification and this is the summary.

## 1. Summary

Quotacore is a self-hosted service that enforces metered allowances for usage-priced software. The
customer defines features, plans and tenants; their application makes one call per metered action;
the call answers yes or no and, if yes, deducts the cost atomically. A monthly allowance resets on
a calendar boundary in the tenant's own time zone, without a cron job, and without a customer's
request ever being refused because a scheduled task did not run.

The customer's billing provider continues to do billing. Quotacore does enforcement, and is precise
about that boundary.

## 2. Problem

Three forces turned quota enforcement from a nicety into a day-one requirement.

**Unit economics inverted.** Before large language models, an over-using feature cost fractions of a
cent in compute. A runaway agent loop can now produce a five-figure downstream bill overnight.
There is no ceiling that is not a database migration and a deploy.

**Pricing shifted to usage.** The allowance became a product surface. Changing it now changes a
contract, and the customer cannot afford a migration and a coordinated release to deliver a price
increase.

**Billing and entitlement are different problems.** Money movement is rare, auditable and
high-value. Access enforcement is frequent, latency-critical and unforgiving. Using a billing API
to answer an access question adds hundreds of milliseconds and a rate limit to a path that runs on
every request. The industry has converged on separating them, and the evidence is that a major
general-purpose billing platform added a dedicated entitlements surface to serve the access case at
all.

## 3. Goals

| # | Goal | Measured by |
| --- | --- | --- |
| G-1 | A customer can enforce a metered limit correctly on a live request path within an afternoon of `docker compose up` | Time to first enforced request — [J-1](docs/product/user-journeys.md#j-1--an-afternoon-from-zero-to-enforcing-p-3-solo-developer) |
| G-2 | Never oversell and never double-charge | [T-01](docs/architecture/testing-strategy.md#t-01--atomic-decrement-under-contention), [T-02](docs/architecture/testing-strategy.md#t-02--idempotent-replay) |
| G-3 | A plan or price change is a configuration change, not a deploy | Plan names remaining in a customer's schema after migration — [J-3](docs/product/user-journeys.md#j-3--replacing-hard-coded-plan-branches-p-2-backend-engineer) |
| G-4 | A customer can explain a disputed charge without a database query | Time to resolve a dispute — [J-5](docs/product/user-journeys.md#j-5--a-customer-disputes-a-charge-p-4-support) |
| G-5 | Dependency failure produces a fast, typed, fail-closed answer — never a hang, never a silent allow | [NFR-A3](docs/architecture/non-functional-requirements.md#3-availability), [FS-07](docs/product/error-catalog.md) |
| G-6 | The service has no external dependency: no account, no signup, no telemetry, no egress | [NFR-S3](docs/architecture/non-functional-requirements.md#5-security) |

## 4. Non-goals

Permanent, with reasons, and not deferred:

- **No invoicing, payments, cards, dunning or revenue recognition.** A different business, and
  deliberately absent. See [foundation §3](docs/product/foundation-and-personas.md#3-what-the-product-is-not).
- **No feature-flag targeting.** A different question, already well served.
- **No infrastructure rate limiting.** A different problem, solved well by gateways.
- **No customer-facing billing portal.** Out of product scope.
- **No PII, no user records, no identity.** The customer's domain model stays theirs —
  [ADR-0013](docs/decisions/0013-single-tenant-flat-model-no-pii.md).
- **No server-side fail-open configuration.** The customer decides what their application does on a
  `503`; we do not make that decision for them — [DR-037](docs/product/domain-rules.md).

## 5. Users

| ID | Persona | Needs | Priority |
| --- | --- | --- | --- |
| P-1 | AI product engineer | A hard ceiling on downstream cost, enforced in the request path, with a real number when it blocks | Primary |
| P-2 | Backend / platform engineer, B2B SaaS | Limits defined once, changed without a deploy, with tenant overrides instead of plan branches | Primary |
| P-3 | Solo developer | One `docker compose up`, no accounts, no ops team | Primary |
| P-4 | Support / operations | Explain a dispute, grant a concession, audit who changed what | Secondary |

## 6. Functional requirements

Every row is expanded in [use-cases.md](docs/product/use-cases.md) and constrained by
[domain-rules.md](docs/product/domain-rules.md).

### 6.1 Enforcement

| ID | Requirement | Rule | Status |
| --- | --- | --- | --- |
| F-1 | `consume` checks and deducts in one indivisible operation; a partial deduction is impossible | [DR-017](docs/product/domain-rules.md) | Specified |
| F-2 | A balance may never be reduced below zero by consumption; the request is denied in full | [DR-018](docs/product/domain-rules.md) | Specified |
| F-3 | An idempotency key is required on `consume` and `refund`; a repeat with a matching fingerprint replays the stored response | [DR-026](docs/product/domain-rules.md) | Specified |
| F-4 | A reused key with a mismatched fingerprint is `409`, never silently accepted | [DR-028](docs/product/domain-rules.md) | Specified |
| F-5 | `refund` credits the current cycle and may never raise a balance above `limit + bonus` | [DR-019](docs/product/domain-rules.md), [DR-021](docs/product/domain-rules.md) | Specified |
| F-6 | `check` is advisory, mutates nothing, and reports a denial as `allowed: false` in a `200` | [DR-024](docs/product/domain-rules.md) | Specified |
| F-7 | A denial changes no state, records no event and consumes no idempotency key | [DR-025](docs/product/domain-rules.md) | Specified |
| F-8 | Allocation caps — "3 projects" — are `consume 1` on create and `refund 1` on delete | [UC-10](docs/product/use-cases.md#uc-10--enforce-an-allocation-cap) | Specified |
| F-9 | An exhausted allowance is `429 quota_exceeded` with the cycle end, and is distinguishable from service rate limiting | [DR-040](docs/product/domain-rules.md) | Specified |

### 6.2 Cycles

| ID | Requirement | Rule | Status |
| --- | --- | --- | --- |
| F-10 | Six reset intervals: `hourly`, `daily`, `weekly`, `monthly`, `yearly`, `never` | [DR-003](docs/product/domain-rules.md) | Specified |
| F-11 | Boundaries are wall-clock times in the tenant's IANA zone, computed from the tenant's anchor — never by adding a duration, never from the previous boundary | [DR-002](docs/product/domain-rules.md) | Specified |
| F-12 | Month ends clamp; 29 February degrades to 28 February; a daylight-saving gap resolves forward | [DR-004](docs/product/domain-rules.md), [DR-007](docs/product/domain-rules.md) | Specified |
| F-13 | Rollover is lazy, monotonic and atomic; the request path repairs a missed boundary, so a background worker is an optimisation only | [DR-009](docs/product/domain-rules.md) | Specified |
| F-14 | Unused allowance does not accumulate | [DR-010](docs/product/domain-rules.md) | Specified |
| F-15 | The server is the sole time authority; no client-supplied timestamp is accepted | [DR-001](docs/product/domain-rules.md) | Specified |

### 6.3 Plans and tenants

| ID | Requirement | Rule | Status |
| --- | --- | --- | --- |
| F-16 | One plan per tenant, plus per-tenant overrides that customise an included feature | [DR-011](docs/product/domain-rules.md) | Specified |
| F-17 | A feature absent from the plan is `403 feature_not_in_plan`, and is distinct from a limit of zero | [DR-012](docs/product/domain-rules.md) | Specified |
| F-18 | A plan edit takes effect at each tenant's next boundary and changes no current balance; `apply-now` overrides it and is audited with a mandatory reason | [DR-014](docs/product/domain-rules.md) | Specified |
| F-19 | A tenant plan assignment change applies immediately and hard-resets the cycle | [DR-013](docs/product/domain-rules.md) | Specified |
| F-20 | Named, scoped, rotatable API keys, hashed at rest, with no recovery path | [ADR-0012](docs/decisions/0012-hashed-scoped-api-keys.md) | Specified |
| F-21 | Opaque tenant identifiers, with a schema constraint that makes an email address structurally impossible | [DR-034](docs/product/domain-rules.md) | Specified |
| F-22 | Every administrative mutation is audited in the same transaction as the mutation | [DR-041](docs/product/domain-rules.md) | Specified |

### 6.4 Operations

| ID | Requirement | Rule | Status |
| --- | --- | --- | --- |
| F-23 | `docker compose up` produces a working enforcement path with no manual step and no account | [NFR-OPS1](docs/architecture/non-functional-requirements.md#7-operability-and-portability) | Specified |
| F-24 | A control-plane outage does not affect enforcement for cached tenants | [NFR-A2](docs/architecture/non-functional-requirements.md#3-availability) | Specified |
| F-25 | A data-store outage is fail-closed, within 250 ms, with no partial state | [DR-037](docs/product/domain-rules.md) | Specified |
| F-26 | Every error code has a metric, an alert and a documented client action | [error catalogue](docs/product/error-catalog.md) | Specified |
| F-27 | A signed-delta event ledger records every applied mutation and is the recovery source, with visible and counted gaps when it cannot | [ADR-0016](docs/decisions/0016-usage-event-ledger.md) | Specified |
| F-28 | OpenAPI 3.1 is the contract; the binary serves the same document it validates against | [ADR-0010](docs/decisions/0010-go-chi-spec-first-openapi.md) | Specified |

### 6.5 Sequenced after the MVP

Boolean entitlements and rolling windows (v0.2) · outbound signed webhooks (v0.3) · embedded admin
UI (v0.4) · TypeScript and Python SDKs (v0.5). Per release, with a definition of done, in
[mvp-scope.md §2](docs/product/mvp-scope.md#2-release-slicing).

## 7. Non-functional requirements

Summarised here; the numbers, and how each is measured, are in
[non-functional-requirements.md](docs/architecture/non-functional-requirements.md).

| Area | Requirement |
| --- | --- |
| Latency | `consume` p50 ≤ 1 ms, p99 ≤ 5 ms warm; a dead data store answers within 250 ms |
| Throughput | 2,000 `consume`/s sustained, 10,000/s burst, 200 racing requests resolved exactly |
| Availability | A control-plane outage does not touch enforcement. A data-store outage fails closed at 0% — a deliberate, numbered sacrifice |
| Durability | No loss on restart. Full data-store loss is rebuildable from configuration plus ledger, and flagged approximate where it is not |
| Security | No recoverable secrets, no PII, no egress, no script injection, and a service that refuses to start on an unacknowledged default key |
| Observability | A metric for every error code, two counters that mean money, and a P0 for each |
| Portability | Static binary, no cgo, embedded time-zone data, Linux and containers |
| Compliance | Apache-2.0 with a patent grant, a BSD-default data-store image, and an SBOM per build |

## 8. Success metrics

| Metric | Baseline | Target | How measured |
| --- | --- | --- | --- |
| Time from `docker compose up` to a first enforced request | n/a | < 30 min | Pilot instrumentation |
| Integrations branching on `consume` rather than gating on `check` | n/a | > 80% | `check`/`consume` call ratio |
| Plan names remaining in a customer's schema | typical: 3–5 | 0 | Design-partner interviews |
| Time to resolve a quota dispute | hours, with an engineer | < 1 min, without one | Support ticket timings |
| `replayed: true` ratio | n/a | Rising | [Observability](docs/architecture/observability.md#11-enforcement) |
| Double charges | unknown, assumed non-zero | 0 | `event_duplicate_detected_total` at zero, permanently |
| Oversold allowances | unknown | 0 | Ceiling and contention tests in CI |
| Control-plane outages reaching enforcement | n/a | 0 | `dbpool_wait_seconds` at zero |
| OSS deployments in the first quarter | 0 | 25 | Public repository |
| Design partners validating A-01 | 0 | 5 | Scheduled conversations |

**The two metrics that matter most are the ones nobody can fake.** If
`event_duplicate_detected_total` and `quotacore_ceiling_violation_total` stay at zero while
`replayed` climbs, the product's central claims are holding under real traffic. Everything else on
this table is a business outcome.

## 9. Open questions

Twenty are logged with proposed defaults, verification methods and deadlines in
[the register](docs/product/assumptions-and-open-questions.md#2-open-questions). Four block
implementation of the affected endpoint:

| ID | Question | Proposed default | Blocks |
| --- | --- | --- | --- |
| Q-01 | Does an admin grant survive a plan change? | No — a grant is cycle-scoped | `grant` |
| Q-02 | After a plan change, is the balance the new limit, or new limit plus remainder? | New limit only | Plan change |
| Q-04 | Does `apply-now` need a confirmation token above N tenants? | Yes, above 50 | `apply-now` |
| Q-14 | Is there an endpoint to preview a plan edit's blast radius? | Yes — `GET /v1/admin/plans/{id}/impact` | `apply-now` |

## 10. Out of scope

| Excluded | Reason | Reconsider? |
| --- | --- | --- |
| Invoicing, payments, cards, dunning | A different business | Never |
| Multi-plan composition, add-ons, proration | Billing semantics | Post-v0.1 |
| Usage rollover and accumulation | A real feature, a materially more complex model | Minor release, if demanded |
| Fractional amounts and decimal units | `int64` in the feature's unit | Minor release |
| Per-second sliding-window rate limiting | Requires hot-path sliding counters; a truthful calendar model was chosen instead | Never, in this product |
| Per-tenant runtime API keys | A real security gap, documented rather than hidden | Post-v0.1, likely v0.2 |
| Quota reservations and holds | A genuine feature and a genuine complexity | Post-v0.1 |
| Multi-region, cross-region replication | Out of scope; the data is on the customer's host | Not planned |
| Horizontal scale-out of the data plane | Would require a sharding scheme that is worse than not having one | Managed data store instead |
| A community support SLA | A business decision, not a technical one | Commercial decision |
