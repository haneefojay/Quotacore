# Use Cases

Eighteen use cases. Each has a stable identifier, an actor, preconditions, a main flow,
alternative flows, a postcondition, and acceptance criteria in Given / When / Then form that can
be turned directly into a test.

Related rules are referenced by identifier from [domain-rules.md](domain-rules.md). Use cases
reference rules; they never restate them, so there is exactly one place where each rule is
defined.

**Status legend:** MVP = v0.1 · v0.2 · v0.3 · v0.4

---

## Control plane

### UC-01 — Define a metered feature
**Actor:** P-2 or P-3 operator · **Status:** MVP

A metered feature is a named, versioned unit of consumption. It is the atom of the whole
product: plans reference it, balances are per-feature, and every enforcement call names one.

**Preconditions:** an admin API key.

**Main flow**
1. Operator creates a feature with a globally unique `key`, a human `name`, a `unit` (for example
   `tokens`, `requests`, `seconds`), and a `description`.
2. Service validates the key format, records it as active, and audits the create.

**Alternative flows**
- Duplicate key → `409 feature_key_exists`, no state change.
- Invalid key format or unit → `400 validation_failed`.

**Postcondition:** the feature exists, is active, and is referenceable by plans.

**Acceptance**
- *Given* a feature with key `llm.tokens.output`
- *When* it is created and then referenced by a plan entitlement
- *Then* `POST /v1/consume` with `feature_key: "llm.tokens.output"` resolves to exactly that
  feature and the response names it
- *And* the create appears in the audit log with actor, before and after

---

### UC-02 — Define a boolean entitlement
**Actor:** P-2 operator · **Status:** v0.2

A boolean entitlement answers "is this capability included", with no counter.

**Main flow**
1. Operator marks a feature as boolean.
2. The plan entitlement carries a limit of `null`, meaning unbounded-once-included, with no
   reset interval.
3. `check` returns `allowed: true` while included; after removal it returns
   `403 feature_not_in_plan`.

**Alternative flows**
- Removing a boolean entitlement does not zero a metered balance, because the two models are
  distinct entities in the schema and cannot be conflated.

**Postcondition:** inclusion and exhaustion are distinguishable by error code, which is the whole
reason boolean entitlements are a separate model and not a limit of infinity (DR-012).

**Acceptance**
- *Given* a tenant on a plan that does not include a feature
- *When* it calls `consume` for that feature
- *Then* the response is `403 feature_not_in_plan`
- *And* a tenant whose plan includes the feature with limit `0` receives `429 quota_exceeded`
- *And* the two are never conflated

---

### UC-03 — Define a plan
**Actor:** P-2 operator · **Status:** MVP

A plan is a named, versioned set of entitlements, plus the reset interval applied to any
entitlement that does not specify its own.

**Main flow**
1. Operator creates a plan with a unique `key` and a display name.
2. Operator adds one entitlement per feature: limit, and optionally a per-entitlement reset
   interval and unit.
3. Service validates that no feature appears twice, that amounts are non-negative integers, and
   that every reset interval is recognised.
4. Plan becomes active and available for assignment.

**Alternative flows**
- Duplicate feature in one plan → `400 validation_failed`, listing the offending feature.
- A plan with no entitlements is allowed. It is a legitimate state — a trial that includes
  nothing yet — and it is not a validation error.

**Postcondition:** the plan is assignable.

**Acceptance**
- *Given* a plan with `llm.tokens.output` at limit `1000000` and reset interval `monthly`
- *When* a tenant is assigned to it
- *Then* the tenant's balance for that feature is `1000000` in the cycle in effect at assignment
- *And* `GET /v1/balance` reports `limit: 1000000`, `bonus: 0`, `balance: 1000000`

---

### UC-04 — Edit a plan limit
**Actor:** P-2 operator · **Status:** MVP

**Main flow**
1. Operator changes a plan entitlement's limit from `1000` to `5000`.
2. Service records the change and audits it.
3. For every existing tenant on that plan, **no balance changes now**. The new limit applies at
   each tenant's next boundary.

**Alternative flows**
- `POST /v1/admin/plans/{plan_id}/apply` with a required `reason` applies immediately to all
  assigned tenants by re-anchoring their cycles (DR-014). This is audited separately and the
  reason is mandatory.
- An operator who wants one tenant changed now uses a tenant override (UC-13), which does not
  affect the rest of the plan.
- **The apply is wider than the threshold.** The call returns `409 confirmation_required` and
  applies nothing, carrying `affected_tenants`, `earliest_cycle_end` and a single-use
  `confirmation_token`. The operator reads the count, and re-issues with the token to proceed
  (DR-046). A token issued against an earlier `resource_version` is refused, because the count it
  was computed from is no longer the count.
- The operator can call `GET /v1/admin/plans/{plan_id}/impact` first (UC-19) to see the same
  numbers without provoking a `409`.

**Postcondition:** the new limit is in the plan record; enforcement of existing balances is
unchanged; the audit log shows the old and new values.

**Acceptance**
- *Given* a tenant with balance `300` of a `1000` limit, mid-cycle
- *When* the plan limit is raised to `5000`
- *Then* `GET /v1/balance` still reports `balance: 300` and `limit: 1000` immediately after the
  edit
- *And* after the tenant's next boundary the balance is `5000` and the limit is `5000`
- *And* `apply-now` changes both at once and writes an audit row containing the supplied reason

---

### UC-05 — Provision a tenant
**Actor:** P-2 operator or P-1 developer · **Status:** MVP

**Main flow**
1. Caller creates a tenant with a unique `external_id`, a `plan_id`, a `timezone`, and a display
  `name`.
2. Service creates the tenant, sets its anchor to the current instant, materialises each plan
  entitlement as a balance, publishes the invalidation message, and audits the create.
3. Caller receives the tenant's internal `tenant_id` and the resolved plan and cycle window.

**Alternative flows**
- Duplicate `external_id` → `409 tenant_exists`, no state change. The API is intentionally
  idempotent-unfriendly here, because two tenants sharing an identity is a data error worth
  surfacing.
- Invalid time zone → `400 validation_failed` naming the offending value, with the IANA name
  echoed back unaltered.
- Duplicate `plan_id` in the request body and the path → `400 validation_failed`.

**Postcondition:** the tenant exists, is active, and enforcement succeeds on the next request
without a database read (DR-039).

**Acceptance**
- *Given* a provisioned tenant
- *When* a `consume` arrives before any cache refresh has run
- *Then* it succeeds, because provisioning published an invalidation and the snapshot honours it
  within the documented bound
- *And* the tenant appears in `GET /v1/admin/tenants` with its plan, zone and cycle window

---

### UC-06 — Change a tenant's plan
**Actor:** P-2 operator · **Status:** MVP

**Main flow**
1. Operator assigns the tenant to a different plan.
2. Service applies the new plan's limits immediately, sets the anchor to the current instant, and
   emits a `plan_changed` event.
3. The previous balance is discarded, not carried over (DR-013).

**Alternative flows**
- A tenant with an outstanding custom override keeps it, because an override is a contract term
  and survives a plan change. This is stated explicitly because the opposite is also defensible.
- Any related idempotency key held for the old plan is unaffected; idempotency is scoped to the
  operation, not the plan.

**Postcondition:** the tenant is on the new plan with a fresh cycle and a fresh allowance.

**Acceptance**
- *Given* a tenant with `balance: 300` of a `1000` monthly limit
- *When* its plan is changed to one with a `500` limit
- *Then* the balance is exactly `500`, `cycle_start` is the assignment instant, and the previous
  `300` is not carried
- *And* one `plan_changed` event exists for that instant

---

## Enforcement

### UC-07 — Check before an expensive action
**Actor:** P-1 developer · **Status:** MVP

**Precondition:** the caller is about to perform a cost-bearing action and wants to avoid paying
for it if the allowance is gone.

**Main flow**
1. Caller invokes `check` with a feature key and a proposed amount.
2. Service rolls the cycle if needed, then reports `allowed`, `balance`, `limit`, `bonus`,
   `cycle_end`, and whether a reset is approaching.
3. Caller decides.

**Alternative flows**
- Cycle rollover happens during the check. The reported window is the new one, and the check
  still answers truthfully.
- Redis unavailable → `503`, never `allowed: true`.

**Postcondition:** nothing changed. No event, no balance movement, no idempotency record.

**Acceptance**
- *Given* a balance of `50`
- *When* `check` asks for `100`
- *Then* the response is `allowed: false` with `balance: 50`, and is **not** `429`; it is a
  successful `200` carrying a decision
- *And* the balance and event count are byte-identical before and after

---

### UC-08 — Consume quota
**Actor:** P-1 or P-2 developer · **Status:** MVP

This is the use case the product exists for. Everything about its design follows from the
requirement that it be correct under retry and under concurrency.

**Preconditions:** tenant exists, is active, plan includes the feature, caller holds a
`runtime`-scoped key, and the request carries an `Idempotency-Key`.

**Main flow**
1. Caller posts `{ feature_key, amount, metadata? }` with an `Idempotency-Key`.
2. In one atomic step the service: checks the idempotency key; resolves the current cycle and
   rolls over if required; verifies the feature is included, not archived, and the tenant not
   suspended; verifies `balance >= amount`; and deducts.
3. Service responds `200` with the new balance, the cycle window, and `replayed: false`.
4. Service appends a `usage_events` row with a signed delta, subject to sink backpressure rules
   (DR-042).

**Alternative flows**
| Condition | Response | State changed |
| --- | --- | --- |
| Key replayed, fingerprint matches | `200`, `replayed: true`, stored response | None |
| Key replayed, fingerprint differs | `409 idempotency_key_reuse` | None |
| `amount` is `0` | `200`, no event | None |
| Unknown tenant | `404 tenant_not_found` | None |
| Feature absent from plan | `403 feature_not_in_plan` | None |
| Feature archived | `403 feature_archived` | None |
| Tenant suspended | `403 tenant_suspended` | None |
| `balance < amount` | `429 quota_exceeded` with `cycle_end` | None |
| Data store unavailable | `503 service_unavailable` | None |
| Control plane unavailable, tenant not cached | `503 control_plane_unavailable` | None |

**Postcondition:** balance reduced by exactly `amount`, or unchanged. Never anything else
(DR-017, DR-042).

**Acceptance**
- *Given* balance `100`
- *When* two requests for `60` arrive concurrently
- *Then* exactly one succeeds with balance `40` and the other receives `429 quota_exceeded` with
  balance `40`
- *And* the sum of all event deltas in the cycle equals `40` minus the cycle opening allowance
- *And* the same idempotency key replayed 10 times returns the recorded state (with `replayed: true`)
  and deducts once

---

### UC-09 — Refund a mistaken deduction
**Actor:** P-1 or P-2 developer · **Status:** MVP

**Preconditions:** an `Idempotency-Key`, a feature key, and a positive amount.

**Main flow**
1. Caller posts to `refund` with the same fields as `consume`.
2. Service performs the identical atomic sequence, with the direction reversed, and additionally
   enforces `balance + amount <= limit + bonus` (DR-019).
3. Service responds with the new balance and credits the current cycle (DR-021).

**Alternative flows**
- Refund would exceed the ceiling → `409 refund_exceeds_grant`, no state change. The remedy is an
  admin `grant`, which raises the ceiling.
- Refund on an exhausted balance is permitted; that is precisely the case a refund exists for.

**Postcondition:** balance increased by exactly `amount`, still within the ceiling.

**Acceptance**
- *Given* balance `0`, limit `100`, bonus `0`, and a previous over-deduction
- *When* a refund of `100` is applied
- *Then* the response is `200` with balance `100`, and `GET /v1/balance` agrees
- *And* a refund of `1` on the same state returns `409 refund_exceeds_grant` with the balance
  unchanged at `100`
- *And* the event deltas sum to `100`

---

### UC-10 — Enforce an allocation cap
**Actor:** P-2 operator · **Status:** MVP

"Enough seats" or "up to three projects" is the most common non-monetary limit in B2B software,
and it is a metered feature with an unusual call pattern: a `refund` whenever an entity is
deleted.

**Main flow**
1. On entity creation the caller invokes `consume` with `amount: 1` against the cap feature.
2. On entity deletion the caller invokes `refund` with `amount: 1` against the same feature.
3. `GET /v1/balance` reports the cap's current count.

**Alternative flows**
- Creation fails downstream after the consume succeeds → the caller refunds. This is why refund
  is a first-class operation rather than an admin-only correction (ADR-0005).
- The count drifts upward if a delete path is missed. `GET /v1/admin/tenants/{id}/events` shows
  the deltas, so drift is detectable; there is no automatic reconciliation of a counter against
  the customer's actual entities, because Quotacore does not know what they are (ADR-0013).

**Postcondition:** the reported count equals the number of successful creations minus refunds
applied.

**Acceptance**
- *Given* a cap of `3` and a current count of `3`
- *When* a fourth creation calls `consume` with `amount: 1`
- *Then* the response is `429 quota_exceeded` and no entity may be created by the caller
- *And* after one deletion refunds `1`, the count is `3` again and a creation succeeds

---

## Cycle management

### UC-11 — Automatic cycle rollover
**Actor:** system · **Status:** MVP

**Main flow**
1. A `consume`, `refund` or `check` call arrives for a tenant whose `cycle_end` is at or before the
   current instant. `GET /v1/balance` is excluded: it is read-only, runs no script, and reports the
   stored window until a write or a `check` advances it (request-lifecycle step 6).
2. The atomic script advances the tenant to the current window, computing the new boundary from
   the anchor and the cycle index (DR-002), and sets the balance to the current limit.
3. A `cycle_rolled_over` event is appended, and the TTL becomes `cycle_end + 24h` (ADR-0003).

**Alternative flows**
- Two callers race the rollover. Exactly one advances; the other observes the new cycle and
  proceeds. No duplicate allowance, no lost deduction.
- The worker races the request path. Same outcome, because the transition is monotonic and
  idempotent.
- The service was down across several boundaries. The tenant lands in the current window and does
  not receive multiple allowances (DR-009).

**Postcondition:** balance equals the new cycle's limit, and no window was skipped or replayed.

**Acceptance**
- *Given* a monthly tenant whose cycle ended one hour ago
- *When* a `consume`, `refund` or `check` is called
- *Then* the response reports the new window and the reset allowance
- *And* the balance after a rollover equals the limit, not the previous balance plus the limit
- *And* exactly one `cycle_rolled_over` event exists for that boundary

---

### UC-12 — Force a rollover
**Actor:** P-4 support operator · **Status:** MVP

**Preconditions:** admin key, and a `reason` of at least 8 characters.

**Main flow**
1. Operator forces a rollover for a tenant, optionally supplying a new anchor.
2. Service advances the tenant to the current window by the same monotonic transition, emits a
   `force_rolled_over` event distinct from an automatic one, and audits with the reason.
3. Operator receives the before and after window so the change can be explained to the customer.

**Alternative flows**
- Forcing a rollover on a `never` entitlement → `409 reset_not_supported`. The interval is a
  property of the entitlement, not an operator preference.
- A forced rollover discards the unused balance. It is a grant of a fresh allowance, not a
  top-up; the operator grants more instead if that is the intent.

**Postcondition:** the tenant is in the current window, and the action is in the audit log with a
reason.

**Acceptance**
- *Given* a monthly tenant mid-cycle
- *When* an operator forces a rollover with a reason
- *Then* the balance becomes the limit, the window is the current one, and an audit row contains
  the before window, the after window and the reason
- *And* calling it twice produces one transition, because the tenant is already current

---

### UC-13 — Apply a tenant override
**Actor:** P-2 operator · **Status:** MVP

The escape hatch that lets a B2B SaaS stop hard-coding plan names in application code.

**Main flow**
1. Operator sets a per-tenant override for a feature: a limit, a reset interval, or both.
2. The override applies immediately to new cycles and is evaluated ahead of the plan value.
3. Service audits the change with before and after.

**Alternative flows**
- An override for a feature the plan does not include → `400 validation_failed`. Overrides
  customise an included feature; they never add one (DR-015).
- Clearing an override restores the plan value from the next boundary, or immediately when the
  clearing request sets `apply_at: immediate`. `DELETE /v1/admin/tenants/{id}/overrides/{feature_key}`
  is the clearing route, because the `tenant_overrides_not_empty` constraint makes a row with both
  values null impossible: clearing is the absence of the row, not a row of nulls.

**Postcondition:** effective limit for the feature is the override when present, else the plan
value; this precedence rule is stated once and used by every read path.

**Acceptance**
- *Given* a plan limit of `1000` and a tenant override of `5000`
- *When* the next cycle begins
- *Then* the new cycle's balance is `5000` and `GET /v1/balance` reports `limit: 5000`
- *And* clearing the override returns the tenant to the plan's `1000` at the next boundary

---

### UC-14 — Grant bonus quota
**Actor:** P-4 support operator · **Status:** MVP

**Main flow**
1. Operator grants a bonus amount for a feature on a tenant, with a `reason`.
2. Service raises `bonus` for the current cycle, subject to the DR-019 ceiling, and applies the
   increase immediately.
3. Service audits the grant with the reason.

**Alternative flows**
- A grant that would exceed the ceiling → `409 grant_exceeds_ceiling`. The operator may instead
  raise the limit via an override, which is the correct remedy for a lasting increase.
- A grant on a `never` entitlement increases the balance with no ceiling tied to a cycle, and no
  expiry.

**Postcondition:** balance increased; the grant is attributable to `source: admin` in the event
history.

**Acceptance**
- *Given* `balance: 0`, `limit: 100`, `bonus: 0`
- *When* an operator grants `50`
- *Then* `balance: 50`, `bonus: 50`, and one event with `delta: 50`, `source: admin`
- *And* a grant of `51` returns `409 grant_exceeds_ceiling` with no change
- *And* after the next boundary both `balance` and `bonus` equal the new limit and `0`
  respectively

---

## Client and operator lifecycle

### UC-15 — Issue and rotate an API key
**Actor:** P-2 or P-3 operator · **Status:** MVP

**Main flow**
1. Operator issues a named key with a scope set and, optionally, an expiry.
2. Service returns the plaintext key exactly once. Only a hash and a public prefix are stored.
3. Operator uses the new key before revoking the old one; then revokes it.
4. Revocation is audited, and the prefix appears in the audit log so a leak can be traced.

**Alternative flows**
- A revoked key is rejected on the next request with no cache delay.
- A lost key is replaced, not recovered. There is no "show key" endpoint and none will be added.
- The bootstrap admin key exists only on first start, is printed once with a warning, and is
  regenerated only by an explicit CLI command.

**Postcondition:** the new key works, the old key does not, and the plaintext exists nowhere but
in the caller's possession.

**Acceptance**
- *Given* a rotated pair, old and new
- *When* a request uses the old key
- *Then* the response is `401 api_key_invalid` within one request, with no cache delay
- *And* the database contains no row from which the new key's plaintext can be recovered

---

### UC-16 — Investigate a tenant's usage
**Actor:** P-4 support operator · **Status:** MVP

The use case that decides whether a team keeps the product, because the first time a customer
disputes a deduction is the first time anyone evaluates the tool.

**Main flow**
1. Operator searches events by tenant, feature, time range, and source, newest first.
2. Each event shows instant, delta, balance after, cycle window, source, and request identifier.
3. Operator sums the deltas for a cycle and compares with the reported balance; they agree.
4. For a specific disputed charge, the operator supplies the customer's `Idempotency-Key` and
   receives the recorded outcome, proving whether it was applied once or twice.

**Alternative flows**
- Events that the sink could not deliver are visible as a gap, with the documented sink-drop
  counter, so an operator is never misled into believing a complete history (ADR-0016).
- Audit history is queried separately from usage history; they answer different questions and
  are not merged.

**Postcondition:** the dispute is explained without a direct database query and without an
engineer.

**Acceptance**
- *Given* a cycle with an opening allowance of `1000` and events summing to `-340`
- *When* the operator lists the events and reads the balance
- *Then* the balance is `660` and the operator can state the arithmetic
- *And* a disputed request is resolved by the recorded idempotency key

---

## Later releases

### UC-17 — Enforce a rolling window
**Actor:** P-1 developer · **Status:** v0.2

**Main flow**
1. Caller consumes with `window: rolling_1h` on a feature configured for rolling windows.
2. Service evaluates the requested lookback across the current cycle's buckets and denies if the
   sum plus the amount would exceed the limit.
3. Buckets are trimmed and expired by TTL, which is safe because expiry is only garbage
   collection for a window whose own cycle has passed (ADR-0008).

**Alternative flows**
- A rolling request against an anchored feature → `400 validation_failed`, with the message
  naming the mismatch. The two cadences are never silently mixed, because a silent mix would make
  the balance a lie.
- Crossing a cycle boundary with rolling data: rolling consumption is counted in the cycle in
  which each event occurred, and the report states the boundary.

**Postcondition:** the rolling sum is correct to the documented precision, which is one bucket,
and the API says so.

**Acceptance**
- *Given* `rolling_5m`, limit `10`, and `8` consumed four minutes ago
- *When* a request for `3` arrives now
- *Then* it is allowed, and the total in the last five minutes is `11` only after the first
  request's events age out of the window
- *And* the response states the window and its precision

---

### UC-18 — Receive a threshold webhook
**Actor:** P-4 operator, downstream system · **Status:** v0.3

**Main flow**
1. Operator configures a tenant's endpoint, secret, and subscribed event types.
2. A threshold crossing enqueues a delivery with a stable `event_id`.
3. Service signs the body with the shared secret and delivers it, retrying with exponential
   backoff and jitter, then dead-lettering after a bounded number of attempts.

**Alternative flows**
- A non-2xx response is retried. A 2xx is final, and a duplicate delivery is possible by design,
  so the payload's `event_id` is the receiver's deduplication key.
- A dead-lettered delivery is visible in the admin API and replayable. A silently dropped
  notification is worse than no notification.

**Postcondition:** the customer learns of the crossing without polling, and delivery is
auditable.

**Acceptance**
- *Given* an endpoint that fails twice then returns 204
- *When* a threshold crossing occurs
- *Then* three delivery attempts are recorded, the third succeeds, and all three carry the same
  `event_id`
- *And* a receiver that has already processed an `event_id` can detect the duplicate

### UC-19 - Preview a plan edit's blast radius
**Actor:** P-2 operator · **Status:** MVP
**Main flow**
1. Operator has just edited a plan limit and is about to decide whether to `apply-now`.
2. Operator calls `GET /v1/admin/plans/{plan_id}/impact`.
3. Service returns `affected_tenants` and the earliest affected `cycle_end`.
**Alternative flows**
- The operator does not call it and applies directly, provoking the same numbers in a
  `409 confirmation_required` instead (DR-046). The route exists so that the common case is a
  quiet read rather than an error.
**Postcondition:** nothing changed. No tenant is re-anchored, no audit row records a mutation, and
no token is issued.
**Acceptance**
- *Given* a plan with 12 assigned tenants whose anchors are spread over three months
- *When* the operator requests the impact
- *Then* `affected_tenants` is `12`
- *And* `cycle_end` is the earliest of the twelve boundaries
- *And* the plan `resource_version`, and every tenant's anchor, is unchanged

---

## Coverage check

| Concern | Covered by |
| --- | --- |
| Defining and changing the offer | UC-01, UC-02, UC-03, UC-04 |
| Tenancy and plan lifecycle | UC-05, UC-06, UC-13, UC-19 |
| Enforcement and correction | UC-07, UC-08, UC-09, UC-10 |
| Cycle correctness | UC-11, UC-12, UC-14 |
| Client lifecycle and trust | UC-15, UC-16 |
| Later-release surfaces | UC-17, UC-18 |
| Denial and failure responses | [error-catalog.md](error-catalog.md) |
| Entity state and legal transitions | [state-machines.md](state-machines.md) |
| End-to-end narratives | [user-journeys.md](user-journeys.md) |
