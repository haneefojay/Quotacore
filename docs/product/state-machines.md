# State Machines

Every entity with a lifecycle has its states and legal transitions defined here. The tables are
normative: an implementation may not invent a state, and a transition not listed here does not
exist. Each table names the guard that must hold, the effect of the transition, and whether it is
audited.

Where a transition has a product-level consequence it is cross-referenced to a rule in
[domain-rules.md](domain-rules.md) and a use case in [use-cases.md](use-cases.md).

---

## 1. Tenant

```
                 ┌──────────────┐
   (absent) ────▶│   active     │◀────────────┐
                 └──────┬───────┘             │
        suspend         │ suspend            │ resume
                 ┌──────▼───────┐             │
                 │  suspended   │─────────────┘
                 └──────┬───────┘
                        │ delete
                 ┌──────▼───────┐
                 │   deleted    │  (terminal, soft)
                 └──────────────┘
```

| # | From | Trigger | To | Guard | Effect | Audited |
| --- | --- | --- | --- | --- | --- | --- |
| T-1 | absent | create (UC-05) | active | `external_id` unique, plan exists, zone valid | anchor = now; materialise balances; publish invalidation | yes |
| T-2 | active | suspend | suspended | — | none; state preserved | yes |
| T-3 | suspended | resume | active | — | none; state preserved | yes |
| T-4 | active or suspended | delete | deleted | — | no further consumption; history retained | yes |
| T-5 | active | change plan (UC-06) | active | target plan exists | limits applied; anchor = now; `plan_changed` event | yes |
| T-6 | active or suspended | change time zone | unchanged | valid IANA name | future boundaries recomputed; no re-anchor (DR-033) | yes |
| T-7 | deleted | any | — | — | rejected: `409 tenant_deleted` | no |

**Invariants**
- INV-T1 A `deleted` tenant is never returned by any data-plane endpoint; the response is
  `404 tenant_not_found`, so that deletion does not confirm the tenant's existence to a caller
  holding a runtime key.
- INV-T2 Suspension never changes a balance, a cycle window, or the cycle index (DR-032).
- INV-T3 A plan change always moves the anchor forward. There is no path that sets an anchor
  backwards.
- INV-T4 Soft deletion is ordered against in-flight requests by the commit point, and never against
  their outcome. A consume that entered the script before the delete committed stands; one that
  reads the tenant afterwards is denied. No completed deduction is ever unwound by a delete
  (DR-047), and the refund operation is the only way to reverse one.

**Recovery from a bad transition:** every control-plane mutation is idempotent by resource
version. A client that receives no response retries with the same expected version and either
sees the applied change or is told the version has advanced.

---

## 2. Plan

```
   (absent) ──create──▶ active ──archive──▶ archived  (terminal)
                          │                    ▲
                          └────unarchive───────┘
```

| # | From | Trigger | To | Guard | Effect | Audited |
| --- | --- | --- | --- | --- | --- | --- |
| P-1 | absent | create (UC-03) | active | key unique; entitlements valid | plan assignable | yes |
| P-2 | active | update entitlement (UC-04) | active | no duplicate feature | affects **new** assignments now, existing tenants at next boundary (DR-014) | yes |
| P-3 | active | apply-now | active | `reason` ≥ 8 chars | re-anchors and re-limits every assigned tenant | yes, with reason |
| P-3a | active | apply-now, at or below the threshold | active | `reason` ≥ 8 chars; assigned tenants ≤ `QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD` | applies immediately, as P-3 | yes, with reason |
| P-3b | active | apply-now, above the threshold, no token | active | `reason` ≥ 8 chars; assigned tenants above the threshold; no `confirmation_token` | **applies nothing**; `409 confirmation_required` with `affected_tenants`, `earliest_cycle_end` and a single-use token (DR-046) | yes, as a refusal |
| P-3c | active | apply-now, with a valid token | active | token unused; plan `resource_version` equals the version it was issued against; not past `QUOTACORE_CONFIRMATION_TTL` | applies immediately, as P-3; token consumed | yes, with reason |
| P-4 | active | archive | archived | — | not assignable; existing tenants continue unchanged | yes |
| P-5 | archived | unarchive | active | — | assignable again; no effect on existing tenants | yes |
| P-6 | any | delete | — | no tenant, plan assignment, or event reference | otherwise `409 plan_in_use` | no |

**Invariants**
- INV-P1 Archiving a plan does not alter any assigned tenant's balance or cycle. Stopping new
  sign-ups and affecting existing customers are different operations.
- INV-P2 `apply-now` is the only operation that changes an existing balance because a plan
  definition changed. It always writes an audit row containing the reason.
- INV-P3 An archived plan is never un-deleted, and a deleted plan never existed.
- INV-P4 A `409 confirmation_required` re-anchors nothing. The count is taken before any write, and
  the token is issued only on the refusal path, so a wide `apply-now` is a decision the operator
  makes twice rather than a change that half-happened (DR-046).

---

## 3. Feature

```
   (absent) ──create──▶ active ──archive──▶ archived  (terminal)
                                       └── (v0.2) set boolean
```

| # | From | Trigger | To | Guard | Effect | Audited |
| --- | --- | --- | --- | --- | --- | --- |
| F-1 | absent | create (UC-01) | active | key unique and format-valid | referenceable by plans | yes |
| F-2 | active | update metadata | active | key immutable | — | yes |
| F-3 | active | archive | archived | — | new consumption → `403 feature_archived`; balances intact (DR-016) | yes |
| F-4 | active | set metered (default) | active | no assignment yet | defines unit and default interval | yes |
| F-5 | active | set boolean | active | v0.2; no metered assignment yet | entitlement becomes inclusion-only | yes |
| F-6 | any | delete | — | no plan, tenant, or event reference | otherwise `409 feature_in_use` | no |

**Invariants**
- INV-F1 Archiving never resets a balance. The balance remains readable until its cycle ends and
  its key expires.
- INV-F2 A feature's `unit` is immutable once any assignment exists, because a unit change would
  silently reinterpret every historical amount. Attempting it is `409 feature_unit_immutable`.
- INV-F3 `key` is immutable always. Renaming is archive-then-create, which preserves history.

---

## 4. Plan entitlement

The row joining a plan to a feature. Its lifecycle is a strict subset of its parents'.

| # | From | Trigger | To | Effect |
| --- | --- | --- | --- | --- |
| E-1 | absent | add to plan (UC-03) | present | assignable to new tenants |
| E-2 | present | change limit | present | new assignments use the new limit; existing tenants at next boundary |
| E-3 | present | change reset interval | present | new assignments only, until `apply-now` |
| E-4 | present | remove from plan | absent | new assignments lack the feature; existing tenants keep it until their next boundary |
| E-5 | present | parent plan archived | present | unchanged; the plan is simply not assignable |

**Invariants**
- INV-E1 Removing an entitlement from a plan does not remove it from an already-assigned tenant
  mid-cycle. There is no mid-cycle entitlement revocation in the MVP, because it would let a
  customer lose paid-for allowance without notice, which is a contract change and not a
  configuration change.
- INV-E2 At most one entitlement per plan per feature. Enforced by a unique constraint; a
  duplicate is `400 validation_failed` naming the feature.

---

## 5. Tenant entitlement (the runtime binding)

Not a table the customer edits. It is the *effective* entitlement for one tenant and one feature:
the plan entitlement, overlaid with the tenant override if one exists. It is the thing the data
plane resolves, and it is why the precedence rule can be stated exactly once.

**Resolution, in order, first match wins**
1. If the feature is archived → `403 feature_archived`.
2. If the tenant is suspended → `403 tenant_suspended`.
3. If the plan has no entitlement for the feature → `403 feature_not_in_plan` (DR-012).
4. If a tenant override exists for the feature → use it.
5. Otherwise → use the plan entitlement.

**Invariant INV-TE1:** steps 1 to 3 are evaluated before the override, so an override can never
grant a feature the plan excludes (DR-015).

---

## 6. API key

```
   (absent) ──issue──▶ active ──revoke──▶ revoked  (terminal)
                          │
                          └──expire (wall clock)──▶ expired  (terminal)
```

| # | From | Trigger | To | Effect | Audited |
| --- | --- | --- | --- | --- | --- |
| K-1 | absent | issue (UC-15) | active | plaintext returned once; hash and prefix stored | yes, prefix only |
| K-2 | active | revoke | revoked | rejected on the next request, no cache delay | yes, prefix only |
| K-3 | active | expiry reached | expired | same as K-2, by clock | on transition |
| K-4 | active | rotate | active | a new key is issued by K-1; the old key is retired by K-2 only when the caller chooses | yes |

**Invariants**
- INV-K1 A key is never rotated implicitly. Rotation is always an explicit pair of calls, so the
  operator can deploy the new key before the old one dies.
- INV-K2 The bootstrap admin key cannot be created through the API. It exists only on first
  start, or by an explicit CLI command against a stopped or local instance, and both paths log
  at `warn`.
- INV-K3 No endpoint returns a key's plaintext after issuance. There is no recovery path, by
  design (DR-043).

---

## 7. Cycle

The per-tenant, per-feature window. This is the state machine with the most subtle correctness
property, so it is specified as a monotonic transition rather than a mutable field.

```
   (uninitialised) ──provisioning──▶ current ──boundary──▶ current ──boundary──▶ ...
                                         ▲                    │
                                         └── monotonic, irreversible ──┘
```

State is materialised at provisioning or plan assignment, and at no other moment. An absent state
at request time is a fault, not a first access (DR-045).

State is represented as `{ cycle_index, window_start, window_end, balance, bonus, limit }`, and
the only permitted mutation is:

```
advance(now):
  target_index = index such that boundary(n) <= now < boundary(n+1)     # DR-002
  if target_index == cycle_index: no-op
  if target_index <  cycle_index: REJECT                            # monotonic
  else: cycle_index = target_index
        balance    = current limit
        bonus      = 0
        window     = (boundary(n), boundary(n+1))
        emit cycle_rolled_over
```

| # | From | Trigger | To | Guard | Effect |
| --- | --- | --- | --- | --- | --- |
| C-1 | uninitialised | tenant provisioned, or plan assigned (UC-05) | current | tenant active, feature in plan | materialise allowance; `window` computed from anchor. **Not** a data-plane trigger: an absent hash at request time is a `503` fault, not an initialisation (DR-045) |
| C-1a | uninitialised | data-plane access with the hash absent | unchanged | — | **rejected** with `503 service_unavailable`; `balance_key_missing_total` rises; the tenant-feature joins the rebuild queue (DR-045) |
| C-2 | current | access at or after `window_end` | current(n+1) | `n+1 > cycle_index` | reset to limit, clear bonus, new window |
| C-3 | current | access with `n < cycle_index` | unchanged | — | **rejected**, not applied. A stale writer cannot re-grant a past allowance |
| C-4 | current | admin set / grant | current | ceiling respected | balance or bonus changes, no window change |
| C-5 | current | admin force-rollover (UC-12) | current(n+1) | interval ≠ `never` | same transition as C-2, distinct event source |
| C-6 | current | interval is `never` | current | — | no boundary exists; C-2 can never fire; no TTL |
| C-7 | current | plan change (UC-06) | current(0) | — | re-anchor to now; new limit; previous balance discarded (DR-013) |

**Invariants**
- INV-C1 `cycle_index` is monotonically non-decreasing. No code path writes a smaller value. This
  is what makes an out-of-order or duplicated worker harmless (ADR-0003).
- INV-C2 The request path and the worker use the same transition, so the two racing on a boundary
  converge. There is no second implementation of rollover to drift out of agreement.
- INV-C3 Key expiry is `window_end + 24h`, which is strictly later than any rollover could
  legitimately need, so a live tenant's key is never absent (garbage collection only).
- INV-C4 Within a cycle, `balance = opening_allowance + Σ(delta)` (DR-042).
- INV-C5 `bonus` is zero at the start of every cycle except when a grant has been applied to the
  new cycle (DR-020).
- INV-C6 A `never` cycle has no window end, so `cycle_end` is `null` in every response. Clients
  must handle the null; it is the documented shape, not an omission.

---

## 8. Idempotency record

```
   (absent) ──apply──▶ recorded ──24h TTL──▶ absent
```

| # | From | Trigger | To | Effect |
| --- | --- | --- | --- | --- |
| I-1 | absent | first `consume` or `refund` with a key | recorded | stores the fingerprint and the response; expiry = now + 24 h |
| I-2 | recorded | repeat, fingerprint matches | recorded | replays the stored response, `replayed: true`; no further mutation |
| I-3 | recorded | repeat, fingerprint differs | recorded | `409 idempotency_key_reuse`; stored record untouched |
| I-4 | recorded | TTL expires | absent | a later use of the key is treated as a new operation (DR-029) |
| I-5 | recorded | store at capacity | recorded | the **write** is refused `503 service_unavailable`; the record is not displaced (DR-048) |
| I-6 | recorded | fast store lost | absent | not rebuildable from the ledger; a retry inside the window applies again and is then reversed by reconciliation (DR-049) |

**Invariants**
- INV-I1 The check and the mutation are one atomic step. There is no window in which two
  concurrent requests both see "absent" and both apply (DR-030).
- INV-I2 A record is never overwritten. I-3 leaves the original intact, so the original
  operation's outcome remains retrievable for support.
- INV-I3 A record is never evicted inside its window. I-5 refuses the write rather than displacing
  an existing record, because inside the window the record is the only thing preventing a second
  charge (DR-048).
- INV-I4 Absence after total store loss is a **distinct** state from absence by expiry. I-4 is a new
  operation; I-6 is a lost one, and it is detected by reconciliation and reversed. Treating them
  alike is what made the second claim unprovable.

---

## 9. Usage event

Append-only. There is no state machine: an event is written or it is not.

| # | Situation | Event written |
| --- | --- | --- |
| U-1 | `consume` applied | `consumed`, delta negative |
| U-2 | `refund` applied | `refunded`, delta positive |
| U-3 | admin grant | `granted`, delta positive, `source: admin` |
| U-4 | admin set | `set`, delta = new − old, `source: admin` |
| U-5 | plan change or apply-now | `plan_changed`, delta = new allowance − old balance |
| U-6 | boundary crossed | `cycle_rolled_over`, delta = new limit − old balance |
| U-7 | forced rollover | `force_rolled_over`, delta as U-6, with the reason |
| U-8 | any denial | **nothing** (DR-025) |

**Invariants**
- INV-U1 Exactly one event per applied mutation, matching the reconciliation identity in DR-042.
- INV-U2 Events are never updated or deleted by an API. Retention is a configured background
  deletion.
- INV-U3 An event may fail to reach the sink. That is visible as
  `quotacore_event_sink_dropped_total` and a gap in history, never as a silent omission
  (ADR-0016).

---

## 10. Webhook delivery (v0.3)

```
   queued ──attempt──▶ delivered ──▶ complete
      ▲                   │
      │                   └──non-2xx──▶ retrying ──(attempts exhausted)──▶ dead_letter
      └─────────────────────── backoff ────────────────────────────────────┘
```

| # | From | Trigger | To | Guard |
| --- | --- | --- | --- | --- |
| W-1 | queued | delivery attempt | delivered | 2xx within the timeout |
| W-2 | queued | delivery attempt | retrying | non-2xx or timeout; `attempt` incremented |
| W-3 | retrying | backoff elapsed | queued | `attempt` < max |
| W-4 | retrying | `attempt` = max | dead_letter | — |
| W-5 | dead_letter | operator replay | queued | `attempt` reset to 0; same `event_id` |
| W-6 | queued | endpoint disabled | dead_letter | no further attempts |

**Invariants**
- INV-W1 At-least-once. A duplicate `event_id` is possible by design, and every payload carries
  it, including in the `Quotacore-Delivery` header.
- INV-W2 A delivery never blocks or slows the enforcement path. It is enqueued, not awaited.
- INV-W3 A dead-lettered delivery is visible and replayable. A silently dropped notification is
  worse than none.

---

## 11. Reset worker

Not a state machine over business data; a loop with a defined safety property, recorded here
because its interaction with cycle state is the single most dangerous part of the design.

| Step | Behaviour |
| --- | --- |
| Tick | Every `reset_scan_interval`, default 60 s |
| Select | Tenants whose `window_end` is within the look-ahead, in bounded pages |
| Act | Invoke the **same** transition as C-2 for each tenant, through the same code path |
| On stale rejection | Treat as success; the tenant is already current (C-3) |
| On data-store error | Log, count, continue. Never abort the batch, never retry in a tight loop |
| On control-plane error | Log, count, continue |

**Invariants**
- INV-WK1 The worker is an optimisation. Deleting it, stalling it, or running it twice changes
  nothing observable, because the request path repairs any tenant it touches (DR-009).
- INV-WK2 The worker holds a runtime-scoped credential and has no direct database access.
