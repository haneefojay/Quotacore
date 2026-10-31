# v0.2 — entitlements and rolling windows

Quotacore · 2026-09-27

Four phases, `IP-17` to `IP-20`. The two capabilities most often asked for, and the two that change
the data model rather than extend it. They ship together because
[ADR-0008](../decisions/0008-cadence-model-anchored-and-rolling.md) designed the keyspace and the
script interfaces for them now and uses them properly only then.

Read [roadmap-index.md](roadmap-index.md) first.

## Phase map

| ID | Phase | Depends on | Status |
| --- | --- | --- | --- |
| `IP-17` | Boolean entitlements | `IP-16` | `COMPLETE` |
| `IP-18` | `set` and `grant` as first-class operations | `IP-17` | `COMPLETE` |
| `IP-19` | Rolling windows | `IP-16` | `COMPLETE` |
| `IP-20` | Per-tenant runtime keys and cap reporting | `IP-18`, `IP-19` | `NOT STARTED` |

`IP-19` may run in parallel with `IP-17` and `IP-18`. It depends on the same finished calendar engine
and touches a different keyspace.

---

## IP-17 — Boolean entitlements

**Status** `COMPLETE`. Opened and closed 2026-10-31, 7 of 7 DoD items met.

**Objective.** An entitlement that is present or absent, with no counter, so the API can say
`feature_not_in_plan` and `quota_exceeded` distinctly rather than collapsing both into zero.

**Scope.**
- A boolean feature type in the schema, and a boolean path in the `consume`, `check` and `balance`
  scripts.
- Mixed cadences: a boolean entitlement alongside metered entitlements in one plan, per feature.
- The metric that distinguishes an absent entitlement from an exhausted one.
- The CLI and contract surface for a boolean feature.

**Specification references.**
- [mvp-scope.md](../product/mvp-scope.md) §2, v0.2
- [DR-012](../product/domain-rules.md): a feature absent from the plan is denied, not zero
- [DR-024](../product/domain-rules.md), [DR-025](../product/domain-rules.md)
- [UC-02](../product/use-cases.md), which is the use case this capability exists to answer
- [INV-E2](../product/state-machines.md), at most one entitlement per plan per feature
- [ROADMAP.md](../../ROADMAP.md) §v0.2, the exit criterion that a boolean check costs the same as a
  metered one

**Dependencies.** Blocked by `IP-16`. Blocks `IP-18` and `IP-20`.

**Definition of Done.**

1. A boolean entitlement allows every `check` and every `consume 1`, and its balance has no ceiling,
   no `bonus` and no exhaustion path.
2. A metered `consume 5000` against a boolean entitlement is refused, with a code that names the
   mismatch rather than reporting insufficient units.
3. A feature absent from the plan is `403 feature_not_in_plan`; a feature present with a balance of
   zero is `429 quota_exceeded`. A test asserts the two are distinguishable in the response, the
   metric and the ledger.
4. A boolean check costs the same as a metered check: the round-trip count is one in both cases,
   asserted in the client, and the boolean path is at or below the metered path's p99 in the
   benchmark.
5. Mixed cadences work in one plan: a tenant with a boolean entitlement and three metered ones
   enforces each by its own type, and the balance response reports each correctly.
6. The schema's feature-type enum is a real enum, and the API's filter values match it exactly; the
   checker's schema check passes.
7. `additionalProperties: false` still rejects a `limit` supplied for a boolean entitlement, rather
   than ignoring it.

**Exit criteria.** A customer can be told, in one response, whether they are not entitled or have run
out.

**Deferred.** Rolling allocation of a shared pool, which is not a boolean question.

> **Closed 2026-10-31.** The behaviour is [DR-050](../product/domain-rules.md), and T-16 in
> [testing-strategy.md](../architecture/testing-strategy.md) proves items 1, 2, 3 and 5.
>
> | Item | Met by |
> | --- | --- |
> | 1 | `TestT16BooleanEntitlementHasNoCounter`: fifty of each `check` and `consume 1` allowed, `null` balance, limit and bonus, no balance hash, a retry replays |
> | 2 | The same test: `consume 5000`, `check 5000` and a `refund` are `409 feature_not_metered`, a new code, and record nothing; the endpoint matrix provokes it on every route that lists it |
> | 3 | `TestT16NotInPlanIsNotExhausted`: `403 feature_not_in_plan` against `429 quota_exceeded` in the response, in `quotacore_errors_total{code}` and in the ledger. Neither refusal writes a row (DR-025), so the ledger tells them apart by history: the exhausted feature has the consumption that exhausted it and the absent one has none. No new metric: the errors counter's `code` label is the distinguishing metric, and observability.md §1.2 says so |
> | 4 | `TestABooleanCallIsOneCall` counts one evaluation per boolean `check` and `consume` in the client, beside `TestOneConsumeIsOneCall`; `TestBooleanCostsNoMoreThanMetered` compares interleaved p99s against the real store. The boolean check makes its round trip on purpose, so it fails with `503` when a metered check does (DR-050) |
> | 5 | `TestT16MixedPlanEnforcesEachByItsType`: one boolean and three metered entitlements on monthly, daily and `never` cadences |
> | 6 | `feature_kind` is a Postgres enum and `FeatureKind` the contract's, held equal by `internal/db`'s enum test; `GET /v1/admin/features?kind=` takes `FeatureKind` by reference |
> | 7 | The admin error matrix: a plan entitlement with `limit: 5` on a boolean feature is `400 validation_failed` naming `entitlements[0].limit`. `PlanEntitlement`'s `limit` and `reset_interval` are required and nullable (api-conventions §7.4), and the feature's kind decides which is valid in the transaction that writes the row, with `plan_entitlements_limit_pair` keeping the two null together |
>
> Two questions were added and answered with their defaults: `Q-33`, no ledger row for a boolean
> consume, and `Q-34`, `not_implemented` kept as a reserved code now that its only surface has
> shipped. The data plane's boolean path is two new scripts, `consume_boolean.lua`, which holds
> `consume.lua`'s idempotency block byte for byte, and `check_boolean.lua`. A boolean feature is
> never pinned and has no balance hash, so provisioning, re-anchors, the reset worker, rebuild and
> reconciliation skip it, and removing it from a plan removes it at once.

---

## IP-18 — `set` and `grant` as first-class operations

**Status** `COMPLETE`. Opened and closed 2026-10-31, 7 of 7 DoD items met, with item 3 read by a
dated note.

**Objective.** The two administrative operations that are currently available but under-specified,
given a contract, a script, an audit record and a test.

**Scope.**
- `set`: an absolute balance for the current cycle, with its reason required.
- `grant`: an additive credit bounded by the ceiling, per cycle, not accumulating.
- Their effect on `bonus`, on the event ledger, and on the reconciliation identity.
- Their reversal, to the extent one exists.

**Specification references.**
- [mvp-scope.md](../product/mvp-scope.md) §2, v0.2
- [DR-018](../product/domain-rules.md), [DR-019](../product/domain-rules.md),
  [DR-020](../product/domain-rules.md), [DR-041](../product/domain-rules.md)
- [INV-C5](../product/state-machines.md), [INV-P2](../product/state-machines.md)
- [error-catalog.md](../product/error-catalog.md): `grant_exceeds_ceiling`
- [T-04](../architecture/testing-strategy.md), the ceiling invariant

**Dependencies.** Blocked by `IP-17`. Blocks `IP-20`.

**Definition of Done.**

1. `set` sets an absolute balance for the current cycle, records the previous value in the audit
   row, and emits a signed delta so the cycle still reconciles (DR-042, INV-C4).
2. `grant` adds to `bonus`, and `bonus` is zero at the start of every cycle thereafter
   (DR-020, INV-C5). A test grants in three consecutive cycles and asserts no accumulation.
3. A grant that would break `balance <= limit + bonus` is `409 grant_exceeds_ceiling` and applies
   nothing, at the same instant as the check (DR-019, T-04).
4. Neither operation moves a cycle window, and neither changes the anchor. A test asserts the window
   is byte-identical before and after.
5. Both are audited with before and after values, attributable to a key, in the same transaction
   (DR-041).
6. Both appear in the CLI and in the contract with the same semantics as the API, and the CLI
   mapping table lists them.
7. A grant does not survive a plan change, because a plan change is a hard reset (DR-013, and the
   `Q-01` answer).

   > **Amended 2026-10-31.** Item 3 is read with T-04's amendment of 2026-10-20: a grant raises the
   > bonus with the balance, so it cannot break `balance <= limit + bonus`, and the ceiling a grant
   > can break is the representable bound `2^53 − 1` (DR-022). The `limit + bonus` refusal is a
   > `set` above it. Both are `409 grant_exceeds_ceiling`, decided inside the script that would
   > apply them, and apply nothing. No behaviour changes.

**Exit criteria.** An operator can correct a balance and a customer can see why, with the correction
reconciling like any other mutation.

**Deferred.** Scheduled or recurring grants, which would need a schedule the data model does not
have.

> **Closed 2026-10-31.** `IP-10` built both operations; this phase specified their reversal, made
> their audit rows carry the whole ceiling on both sides, and proved each item by T-17 in
> [testing-strategy.md](../architecture/testing-strategy.md).
>
> | Item | Met by |
> | --- | --- |
> | 1 | `TestT17ACorrectionReconciles`: a set down and a set up record the previous balance in the audit row and signed deltas of −350 and +500 in the ledger, an equal set records none, `balance = opening + Σ(delta)` holds and `verify-tenant` is clean |
> | 2 | `TestT17AGrantNeverAccumulates`: grants of 30, 40 and 50 in three consecutive cycles each leave the bonus at that cycle's grant; `TestAGrantIsCycleScoped` across a real boundary |
> | 3 | `TestT17ARefusedCorrectionChangesNothing` and its script twin: a set above `limit + bonus` and a grant past `2^53 − 1` are `409 grant_exceeds_ceiling`, and the hash, audit log and ledger are unchanged |
> | 4 | `TestT17ACorrectionReconciles` compares the cycle index, both window fields and the tenant's anchor before and after every grant and set; `TestT17NeitherMovesTheWindow` in the script |
> | 5 | `TestT17ACorrectionReconciles`: each audit row records balance, limit and bonus before and after, the reason and an admin key's `actor_key_id`, written in the transaction that holds the tenant's lock while the script runs. The audit `before` previously had the balance alone, and a zero limit or bonus was omitted on either side |
> | 6 | [cli.md](../architecture/cli.md) §1 maps `grant <tenant>` to `grantTenant` and `set <tenant>` to `setTenantBalance`; `TestGrantSetAndRolloverFromTheCLI` |
> | 7 | `TestAGrantIsCycleScoped` (its plan-change half) and `TestAPlanChangeIsAHardReset` |
>
> Reversal is [DR-051](../product/domain-rules.md) (new): there is no undo, and a set from the
> audited figure is the reversal, proved by `TestT17ACorrectionIsReversedByASet`. `Q-35` asks
> whether a grant's reversal should also take its bonus back, and is answered with its default, no.
> A stated limit is unchanged from `IP-10`: the script runs in the fast store inside the Postgres
> transaction that audits it, so a commit that fails after the script applied leaves a moved
> balance with no audit row; the route answers `5xx`, logs it, and hands the ledger nothing, and
> reconciliation reports the drift.

---

## IP-19 — Rolling windows

**Status** `COMPLETE`. Opened and closed 2026-10-31, 7 of 7 DoD items met.

**Objective.** A requested-duration lookback inside the current cycle, correct under concurrency,
without touching the calendar engine.

**Scope.**
- `rolling_5m` to `rolling_30d`, requested-duration lookback, per
  [ADR-0008](../decisions/0008-cadence-model-anchored-and-rolling.md).
- The window keys, their expiry, and the atomic decrement that keeps a sliding window correct.
- A fixed window and a sliding window on one key per tenant-feature.
- The `check` and `consume` semantics for a rolling entitlement.

**Specification references.**
- [mvp-scope.md](../product/mvp-scope.md) §2, v0.2
- [ADR-0008](../decisions/0008-cadence-model-anchored-and-rolling.md)
- [DR-006](../product/domain-rules.md), [DR-017](../product/domain-rules.md),
  [DR-022](../product/domain-rules.md)
- [cycle-engine.md](../architecture/cycle-engine.md) §9, the review checklist that exists to stop
  rolling windows from modifying calendar arithmetic
- NFR-T1, unchanged by the added keyspace

**Dependencies.** Blocked by `IP-16`, and by the keyspace work in `IP-04` and `IP-05`, which the
rolling keys extend. May run in parallel with `IP-17` and `IP-18`. Blocks `IP-20`.

**Definition of Done.**

1. A sliding window is correct under concurrency: 200 concurrent decrements of a rolling allowance
   of 100 succeed exactly 100 times inside the window, and the window sum is 100 at every instant
   checked.
2. A fixed window and a sliding window coexist on one key per tenant-feature, and the balance
   response distinguishes them.
3. The calendar engine is unchanged. A test asserts the boundary table from `IP-03` still passes
   byte for byte, which is the mechanical form of the review checklist in
   [cycle-engine.md](../architecture/cycle-engine.md) §9.
4. A window key expires on its own schedule and cannot outlive its cycle's usefulness, so the key
   count per tenant-feature stays bounded; the count is asserted.
5. Clock skew affects a rolling window the same way it affects a calendar one: bounded and alerted,
   never silently wrong (NFR-D8).
6. The p99 at the sustained load in NFR-T1 does not regress against the `IP-16` measurement, and
   the round-trip count is still one.
7. `invalid_window` is returned for a duration outside the supported set, and for a duration that
   does not fit the feature's cadence.

**Exit criteria.** A rolling allowance behaves correctly at 2,000 requests per second, and the
calendar engine that was proved at `IP-03` is still exactly as it was.

**Deferred.** Longer windows, sub-minute precision, and per-window overrides.

> **Closed 2026-10-31.** A rolling feature is a new feature kind with a limit and a window on its
> entitlement, counted in sixty buckets in its own hash by three scripts of its own (DR-052, new).
> The specification disagreed with itself on three points, each settled by DR-052 and a dated note
> where it was written: the window is configured and named rather than chosen per call, a rolling
> feature has no cycle (ADR-0008 over mvp-scope's "inside the current cycle"), and a mismatch is
> `invalid_window` (the catalogue over UC-17). ADR-0008's open choice between exact and
> approximate is `Q-36`, answered with sixty buckets and the error bound DR-052 states.
>
> | Item | Met by |
> | --- | --- |
> | 1 | `TestT18TwoHundredAgainstAHundred`: 200 concurrent consumes of 1 against 100 apply exactly 100 times, and the window sum, read from the store throughout, is never above 100 and is 100 after |
> | 2 | `TestT18AFixedAndASlidingWindowCoexist`: a metered and a rolling feature on one tenant, each in exactly one key of its own, told apart in `GET /v1/balance` by `kind`, `window` and `window_precision_seconds` |
> | 3 | `TestT18TheCalendarEngineIsUnchanged` pins every file of `internal/cycle`, the IP-03 boundary table among them, by digest; `TestNoRollingCodeImportsTheCycleEngine`; and the rolling scripts hold no transition block (`TestTheRollingScriptsShareOneWindow`) |
> | 4 | `TestT18TheKeyIsBounded`: across three windows of virtual time one key, never more than 60 buckets, and an expiry at the newest bucket plus the window. "Its cycle's usefulness" is read as its window's, because a rolling window has no cycle |
> | 5 | `TestT18ClockSkewIsBoundedAndVisible`: a bucket from a clock two seconds fast is counted and never pruned by a slow one, and the two views differ by at most the bucket the skew spans; the skew itself is `quotacore_clock_skew_seconds`, alerted at 1 s (`Q-26`) |
> | 6 | `TestARollingCallIsOneCall` (one evaluation for consume, check and refund), `TestRollingCostsAboutWhatMeteredCosts` against a full window, and `TestRollingSustainedThroughput`, the load suite's rolling row at 2,000/s against a metered run on the same stack, which the `full-suite` label runs; `Q-30` is answered with its default, the service's own time |
> | 7 | `TestT18InvalidWindow`: a window outside the set, not a string, on a metered feature, missing on a rolling one, or different, is `400 invalid_window` on consume, refund and check, and writes nothing |
>
> `feature_not_anchored` is new, for a grant, set, rollover or override on a rolling feature.
> Rolling consumption writes no ledger row (`Q-37`), and `Q-11` is answered by the contract's
> `RollingWindow` enum.

---

## IP-20 — Per-tenant runtime keys and cap reporting

**Status** `BLOCKED`, gated on `IP-18` and `IP-19`.

**Objective.** A runtime key that can address only its own tenant, and a balance response that says
which resource a cap counts and what the current count is.

**Scope.**
- Per-tenant runtime API keys, closing gap 1 in [SECURITY.md](../../SECURITY.md) §6.
- The tenant binding on a key, enforced in the script rather than in application code.
- Allocation cap reporting in `GET /v1/balance`: the resource type counted and the current count.
- The admin surface for issuing, listing and revoking a tenant-scoped key.

**Specification references.**
- [mvp-scope.md](../product/mvp-scope.md) §2, v0.2
- [ROADMAP.md](../../ROADMAP.md) §v0.2, the exit criterion that a per-tenant runtime key cannot
  address another tenant
- [SECURITY.md](../../SECURITY.md) §6, known gaps
- [ADR-0012](../decisions/0012-hashed-scoped-api-keys.md)
- [INV-K1](../product/state-machines.md) to [INV-K3](../product/state-machines.md),
  [INV-X7](../architecture/data-model.md)
- [DR-034](../product/domain-rules.md), [DR-036](../product/domain-rules.md), NFR-S1, NFR-S4

**Dependencies.** Blocked by `IP-18` and `IP-19`. Blocks nothing in v0.2.

**Definition of Done.**

1. A per-tenant runtime key cannot address another tenant, and the refusal is `403 insufficient_scope`
   or `404 tenant_not_found` by the specification's choice, recorded as a rule. The test attempts
   every cross-tenant address and asserts none succeeds.
2. The tenant binding is checked inside the atomic script, so the guarantee does not depend on
   application code having run correctly. A test that bypasses the application layer still cannot
   cross tenants.
3. A tenant-scoped key is revoked with the same immediacy as a global one, and a revoked key's next
   request is `401 api_key_invalid` (INV-K1).
4. No per-tenant key is recoverable in plaintext after issuance, and the plaintext-storage test from
   `IP-09` covers the tenant-scoped case too (NFR-S1).
5. `GET /v1/balance` reports, for a capped resource, which resource type the cap counts and the
   current count, and the field is present only where a cap applies.
6. A soft-deleted tenant has no usable per-tenant key within one refresh interval
   (INV-X7), asserted as a test rather than as an intention.
7. The cardinality of `quotacore_consume_total` is unchanged by having per-tenant keys; the tenant is
   not a label (NFR-O2).
8. `not_implemented` is gone from these routes on a v0.2 build, and a v0.1 build still returns it
   (501).

**Exit criteria.** A customer can issue a key to a specific tenant, and that key is structurally
incapable of reaching another tenant.

**Deferred.** Customer-managed key rotation schedules, and keys with a subset of the global scopes.
