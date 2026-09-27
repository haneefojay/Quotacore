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
| `IP-17` | Boolean entitlements | `IP-16` | `BLOCKED` |
| `IP-18` | `set` and `grant` as first-class operations | `IP-17` | `BLOCKED` |
| `IP-19` | Rolling windows | `IP-16` | `BLOCKED` |
| `IP-20` | Per-tenant runtime keys and cap reporting | `IP-18`, `IP-19` | `BLOCKED` |

`IP-19` may run in parallel with `IP-17` and `IP-18`. It depends on the same finished calendar engine
and touches a different keyspace.

---

## IP-17 — Boolean entitlements

**Status** `BLOCKED`, gated on `IP-16`.

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

---

## IP-18 — `set` and `grant` as first-class operations

**Status** `BLOCKED`, gated on `IP-17`.

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

**Exit criteria.** An operator can correct a balance and a customer can see why, with the correction
reconciling like any other mutation.

**Deferred.** Scheduled or recurring grants, which would need a schedule the data model does not
have.

---

## IP-19 — Rolling windows

**Status** `BLOCKED`, gated on `IP-16`, and on the keyspace work in `IP-04` and `IP-05`.

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
