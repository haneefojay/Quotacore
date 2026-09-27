# Product Foundation

## 1. Purpose

Quotacore is a self-hostable service that answers one question, on the critical path of someone
else's application, faster and more correctly than that application can answer it itself:

> May this quota holder perform this action, and if so, deduct the cost?

It exists because that question is currently answered badly almost everywhere. It is answered
with a counter column in the customer's users table, or a Redis `INCR` guarded by an `if`, or a
call to a billing provider's API on the request path, or a cron job that resets a number on the
first of the month. Each of those answers is fast until it is wrong, and being wrong is
expensive: a customer is either billed for something they did not use, or permitted to consume
something they did not pay for.

## 2. Problem statement

Three forces have turned a formerly optional engineering convenience into a day-one survival
requirement.

**Unit economics inverted.** Before large language models, an over-using feature cost a fraction
of a cent in server compute. A user who spams an AI endpoint can now generate a five-figure
downstream bill in minutes. Metering is no longer a feature; it is the thing standing between
one customer's automation and the operator's margin.

**Pricing shifted to usage.** Usage-based pricing makes the allowance a product surface rather
than an internal detail. When the allowance changes, the contract with the customer changes, and
changing it must not require a database migration and a redeploy.

**Billing and entitlement are different problems.** Money movement is a rare, high-value,
auditable operation. Access enforcement is a frequent, latency-critical, unforgiving operation.
They have opposite requirements in every dimension — frequency, latency, consistency tolerance,
failure cost — and using a billing API to answer an access question adds hundreds of
milliseconds and a rate limit to a path that runs on every request. The industry has converged on
separating them, and the evidence is that a general-purpose billing platform had to add a
dedicated entitlements surface to serve the access case at all.

## 3. What the product is not

This is as load-bearing as what it is.

- **Not a billing system.** It does not move money, does not know about payment methods, does not
  produce invoices, and does not compute prices. The customer's billing provider owns all of
  that.
- **Not a feature-flag service.** Feature flags answer "should this code path run for this
  cohort". Entitlements answer "is this customer entitled to this capability". They share
  vocabulary and nothing else. Boolean entitlements (v0.2) are a convenience, not a
  positioning claim.
- **Not an API gateway.** Gateways do infrastructure rate limiting — requests per second per IP
  — which is a different problem from a business allowance that resets on a calendar boundary.
  See ADR-0008 for why per-minute limiting is deliberately not attempted in the MVP.
- **Not an analytics product.** It emits a signed-delta usage ledger for recovery and support.
  It is not a reporting warehouse and does not claim to be.
- **Not a customer identity store.** It holds opaque identifiers. Deliberately. See ADR-0013.

## 4. Target users

| ID | Persona | Description | What they feel today |
| --- | --- | --- | --- |
| **P-1** | AI product engineer | Building an agent, copilot or LLM wrapper with strict unit economics. The downstream provider bills them per token. | "One user's runaway loop cost me four hundred dollars overnight and I had no ceiling that wasn't a database migration." |
| **P-2** | Backend / platform engineer, B2B SaaS | Owns a multi-tenant application with tiered plans. Currently enforces limits in application code. | "Every plan change is a migration. I have four `if plan === 'pro'` branches and I am afraid to touch any of them." |
| **P-3** | Solo developer / indie hacker | Usage-based pricing, no ops team, cannot afford a billing platform. | "I need a hard ceiling on tokens per month and I need it to be one `docker compose up`." |
| **P-4** | Support / operations engineer (secondary) | Investigates why a customer was blocked, grants concessions. Uses the CLI in the MVP, the UI in v0.4. | "When a customer says they were charged twice, I have no way to find out what happened." |

P-1 and P-3 are the beachhead. P-2 is the durable base. P-4 is the internal champion who
determines whether a team actually rolls the product out or quietly reverts it.

## 5. Jobs to be done

| ID | Job | Statement |
| --- | --- | --- |
| **JTBD-1** | Enforce a ceiling | When a request arrives that consumes a metered resource, I want an authoritative yes/no plus an atomic deduction, so that I never serve more than I sold and never charge twice. |
| **JTBD-2** | Model the offer | When I change my plans, I want limits defined in one place and applied on a schedule I control, so that a pricing change is a configuration change, not a deploy. |
| **JTBD-3** | Restrict the allowance | When a plan includes "up to 3 projects", I want that enforced accurately as entities are created and deleted, so that the limit reflects reality. |
| **JTBD-4** | Correct a mistake | When I deduct the wrong amount or a provider call fails, I want to credit it back, so that I am not permanently overcharging a customer. |
| **JTBD-5** | Survive failure | When the enforcement service is unreachable, I want a precise, fast, machine-readable failure, so that my application can make an explicit fail-open or fail-closed decision instead of crashing or hanging. |
| **JTBD-6** | Explain a decision | When a customer disputes a block, I want the tenant's balance, cycle window and event history, so that I can answer without a database query and an engineer. |
| **JTBD-7** | Move fast | When I evaluate this, I want it running in one command with no accounts, no cloud signup and no dashboard, so that I can try it in an afternoon. |

JTBD-5 and JTBD-7 are the two that competitors handle worst, and they are the two that decide
whether the product gets adopted at all.

## 6. Value proposition

> Define your allowances once. Every protected action becomes one call that is atomic by
> construction, answers in single-digit milliseconds, and cannot be bypassed by a retry, a
> double-click, a race, or a background job that was down at midnight.

Three claims, each of which is a testable property rather than a slogan:

1. **Atomic by construction.** The check and the deduction are a single indivisible operation in
   the datastore. No lock, no retry loop, no double-spend. Verified by a contention test in
   [testing-strategy](../architecture/testing-strategy.md).
2. **Correct under retry.** A client that retries after a timeout is charged once, because
   retries are identified and replayed rather than reapplied.
3. **Correct under failure.** A missed cycle boundary cannot deny a customer their allowance,
   because the request path repairs the cycle itself and the background worker is only an
   observer.

## 7. Differentiation

The honest competitive position, established in
[competitive-landscape](../research/competitive-landscape.md):

**The problem is real and validated, and the space is not empty.** Stigg markets itself as a
usage runtime with entitlement checks at p99 under 10 ms and credits; Lago is an
AGPL-licensed metering and billing platform with entitlements, credits and wallets; OpenMeter,
Orb, Metronome and Polar address overlapping ground. Stripe itself now ships an Entitlements
API. Any claim that nobody has built this would be false and would not survive contact with a
prospect.

The wedge that is defensible is narrower and specific:

| Dimension | Billing and usage platforms | API gateways | **Quotacore** |
| --- | --- | --- | --- |
| Answers access questions at request time | Secondary; via a separate surface | Not modelled | **The entire product** |
| Invoicing, payment, dunning, tax | The product | Not modelled | **Not built, ever** |
| Correct atomic decrement under concurrency | Requires the right configuration | Not modelled | **Guaranteed, test-enforced** |
| Retry safety for metered operations | Varies; the hard cases are known | Not applicable | **Idempotency required, not optional** |
| Time to first working install | Hours to days, plus a signup | Minutes | **`docker compose up`, one command** |
| Self-hosted footprint | Many services, some requiring a cluster | One or two | **One app, one Postgres, one Redis-compatible store** |
| Cloud dependency | Often required for the full product | Varies | **None. No account, no telemetry, no egress** |

Two claims in that table are unique rather than comparative, and they are the ones the product
should be built around: **retry safety is required rather than optional**, and **the deployment
has no external dependency at all**. Both are things a larger platform cannot easily adopt,
because both would be regressions against their own billing guarantees and their own
cloud-hosted business model.

## 8. Product principles

1. **The critical path stays fast, or the product is worthless.** Nothing that can block goes on
   the enforcement path. No per-request database query, no external call, no unbounded work. When
   something must be slow, it happens after the answer — or, in the case of a cold cache, fails
   fast and visibly rather than waiting.
2. **Correctness over convenience, in the enforcement path specifically.** A balance is never
   approximated on the hot path. Every shortcut has a named failure mode, and the named failure
   mode is always a documented revenue event, never a silent one.
3. **Refuse gracefully, never by hanging.** Every failure is a fast, typed, machine-readable
   answer. The customer must be able to write `if err == ErrQuotaExceeded` and nothing subtler.
4. **Configuration is data, not code.** A plan change must never require a deploy or a schema
   migration.
5. **One concept per entity.** Tenant, plan, feature. No users, no organisations, no hierarchies.
   The customer's domain model stays in the customer's application (ADR-0013).
6. **Store no identity.** Opaque identifiers only. A breach of Quotacore must not become a breach
   of the customer's customer list.
7. **Say what it does not do.** Where a customer needs something outside the product — a gateway
   for per-second limits, their own billing for invoicing — the documentation says so and points
   at the right tool.

## 9. Scope statement

- **In the MVP:** [mvp-scope.md](mvp-scope.md) defines the release boundary precisely, per
  release, with a definition of done for each capability.
- **Sequenced after the MVP:** boolean entitlements and rolling windows (v0.2), outbound
  webhooks (v0.3), embedded admin UI (v0.4), official SDKs (v0.5). All five releases are
  specified; the reasoning for the sequence is ADR-0015.
- **Never:** invoicing, payment processing, card handling, customer-facing billing portals.
  Recorded with reasons in [mvp-scope.md](mvp-scope.md).

## 10. What must be true for this to succeed

Stated as falsifiable assumptions, tracked in
[assumptions-and-open-questions.md](assumptions-and-open-questions.md):

- **A1.** A 1–5 ms network hop to a sidecar is acceptable on the protected path for the target
  users. If a material fraction require sub-millisecond in-process evaluation, the product's
  deployment model is wrong.
- **A2.** Customers accept adding one Redis-compatible store and one Postgres, or already run
  both. If the single-binary, zero-service pitch turns out to dominate adoption, the embedded
  store in ADR-0011's alternatives becomes worth building.
- **A3.** Requiring an idempotency key is acceptable friction. It is the correct design and it
  is documented; if it proves to be a real adoption barrier, the fallback is SDKs that generate
  keys automatically (v0.5), not making the key optional.
- **A4.** Metered quota enforcement, as distinct from boolean entitlements and from
  infrastructure rate limiting, is a durable standalone purchase. This is the load-bearing
  commercial assumption and it is the one most exposed to the competitive findings.
