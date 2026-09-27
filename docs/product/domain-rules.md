# Domain Rules

These are the rules that must hold for the system to be correct. They are stated here, once,
with an identifier each, and referenced by use cases, API contracts, tests and code comments.
Anything not stated here and not derivable from a stated rule is undefined behaviour and must
not be relied upon.

Vocabulary is defined in [glossary.md](glossary.md). Rule identifiers are stable: a rule is
never renumbered, only superseded, with the superseding rule noted in place.

---

## Time and cycles

### DR-001 — The server is the sole time authority
All cycle and window computation uses the service's own clock. Clients cannot supply a
timestamp for enforcement purposes. A client-supplied time is rejected, not ignored.
*Rationale:* a client that could set its own clock could reset its own allowance.
*Consequence:* the host must be NTP-synchronised. The service records the zone-data version it
was built with in `GET /v1/admin/status` so that historical window disagreements are diagnosable
(ADR-0007).
*Verified by:* NFR-D8, which bounds the clock skew the service tolerates, and a request-validation assertion that a client-supplied timestamp is `400 validation_failed` rather than ignored.

### DR-002 — A cycle window is a pure function
```
window(n) = [ boundary(anchor, interval, timezone, n), boundary(anchor, interval, timezone, n+1) )
```
The window is computed from the tenant's **original anchor** and an integer cycle index `n`. The
cycle currently in effect is the largest `n` whose start is at or before the current time.
*Rationale:* computing the next window from the previous window's start causes drift — a tenant
anchored on 31 January would have successive starts of 31 Jan, 28 Feb, 28 Mar, 28 Apr… This rule
makes the sequence drift-free by construction.
*Consequence:* the anchor is the only per-tenant time state. There is no separate "current cycle
start" that can disagree with it.
*Verified by:* T-07, and the property test that the projected window equals the computed window.

### DR-003 — Boundaries are wall-clock times in the tenant's time zone
A boundary is a wall-clock time in `tenant.timezone`, never the addition of a duration to a
localised instant.
*Consequence:* `30 days` is not a month. Adding a fixed duration to a calendar boundary is
prohibited. See DR-004 through DR-007 for each interval.
*Verified by:* T-07, and NFR-OPS8 for the zone database the arithmetic depends on.

### DR-004 — Month ends are clamped
For a `monthly` boundary, the day is the anchor's day-of-month, clamped to the last day of the
target month.
*Example:* anchored 31 January, Berlin. Boundaries fall on 31 Jan, 28 Feb (29 in a leap year),
31 Mar, 30 Apr, 31 May, 30 Jun, and so on.
*Verified by:* T-07.

### DR-005 — 29 February degrades to 28 February
For a `yearly` boundary anchored on 29 February, non-leap years resolve to 28 February. The
anchor remains 29 February; the degradation is per-boundary and does not mutate the anchor.
*Verified by:* T-07.

### DR-006 — `hourly` and `weekly` are calendar, not duration-based, except where stated
`hourly` is exactly 3600 seconds. `weekly` is the same wall-clock time seven calendar days
later, in the tenant's zone, so a daylight-saving transition makes that boundary 167 or 169
hours after the previous one.
*Rationale:* a customer who says "resets every Monday at 09:00" means local 09:00, and accepting
the hour of drift is more surprising than the alternative. The asymmetry with `hourly` is
deliberate and is stated here so it is not read as an oversight.
*Verified by:* T-07, which asserts the 167- and 169-hour weeks that make the asymmetry deliberate rather than an oversight.

### DR-007 — A daylight-saving transition is normalised, never skipped
A boundary wall-clock time that does not exist on a spring-forward day resolves forward to the
first valid instant. One that occurs twice on a fall-back day resolves to the first occurrence.
Both are asserted in the test matrix in
[cycle-engine](../architecture/cycle-engine.md).
*Consequence:* a tenant in a spring-forward zone has one short cycle per year. That is correct
behaviour, not a defect.
*Verified by:* T-07, and NFR-OPS8.

### DR-008 — `never` has no boundary
An entitlement with `reset_interval = never` never rolls over. Its balance persists
indefinitely and is not expired. Its key receives no expiry.
*Consequence:* a `never` allowance is a lifetime allowance. It is the correct model for
one-off grants such as "1,000 lifetime generations".
*Verified by:* T-07, whose row 13 asserts a `null` `cycle_end` four years after the anchor, and the
`reset_interval = never` script test, which asserts that no expiry is set on the key.

### DR-009 — Missed boundaries are skipped, not replayed
If a tenant is idle across three boundaries, the next request places it directly in the current
window. It is not reset three times, and it does not receive three allowances.
*Consequence:* a service outage of any length cannot inflate a customer's allowance.
*Verified by:* T-06.

### DR-010 — Use-it-or-lose-it
Unused allowance does not carry into the next cycle. At rollover the balance becomes the current
cycle's allowance, discarding whatever was unused.
*Rationale:* rollover accumulation is a real product feature and it is a materially more complex
model. It is deferred, not accidentally omitted. See [mvp-scope.md](mvp-scope.md).

---
*Verified by:* T-06, which asserts one rollover and one allowance across a multi-cycle outage.

## Entitlements and plans

### DR-011 — One plan per tenant
A tenant is assigned exactly one plan. A tenant may additionally carry per-feature **overrides**
that modify or replace a single entitlement.
*Rationale:* multiple simultaneous plans, plan composition and add-ons are all deferred. A single
plan plus per-feature overrides covers both "custom limits for an enterprise customer" and "give
this one customer SSO" without a composition model.
*Consequence:* an override may set a limit, a reset interval, or both. An override never adds a
feature that the plan does not include; see DR-012.
*Verified by:* the `tenants.plan_id` column and the `tenant_entitlement_overrides` table in data-model.md, which make a second simultaneous plan unrepresentable. There is no behavioural test, because the schema is the proof.

### DR-012 — A feature absent from the plan is denied, not zero
If a tenant's plan contains no entitlement for the requested feature, the request is denied with
`403 feature_not_in_plan`. A limit of `0` and an absent entitlement are different states: the
first means "included, with nothing available", the second means "not part of this plan".
*Consequence:* the distinction must be visible to the caller, because "you get none of this
because you are on Free" and "your Free plan includes zero of this" are different product
messages.
*Verified by:* T-08, and the `Endpoint × error matrix` contract test for the `403 feature_not_in_plan` mapping.

### DR-013 — Plan assignment change is a hard reset
Assigning a tenant to a different plan applies the new plan's limits immediately and sets the
tenant's anchor to the current instant, starting a fresh cycle.
*Consequence:* the previous balance is discarded, not carried over. Proration is deferred. The
discard is recorded as a `plan_changed` usage event so it is visible in history.
*Note:* this is a different rule from DR-014, and the difference is intentional. Assignment is a
change to a contract; a plan edit is a change to an offer.
*Answers:* [Q-01](../product/assumptions-and-open-questions.md#2-open-questions) and
[Q-02](../product/assumptions-and-open-questions.md#2-open-questions), both of which asked what a
grant and a balance become after a plan change. They were open questions when first written and
were already answered by this rule and DR-020; closing them required no new decision.
*Verified by:* T-11, which reconciles the discarded balance against the `plan_changed` event.

### DR-014 — A plan edit takes effect at each tenant's next boundary
Editing a plan's entitlement changes the limit that applies to new assignments immediately, and
to existing tenants at their next boundary. It does not change any current balance.
`POST /v1/admin/plans/{plan_id}/apply` overrides this by re-anchoring and re-limiting every
assigned tenant immediately, and is audited with a required reason. (ADR-0006)
*Verified by:* T-05, which asserts that a limit change never moves a boundary and never rewrites an open cycle.

### DR-015 — An override can never exceed what the plan allows unless it is a grant
A tenant override may raise a limit above the plan's limit. This is intentional and is the
mechanism for "custom limits for an enterprise customer". It cannot, however, grant a feature
the plan does not include (DR-012).
*Verified by:* T-04.

### DR-016 — Archived features are preserved, not deleted
A feature or plan that is referenced by any plan, tenant or historical event may be **archived**
but not deleted. Archiving a feature denies new consumption with `403 feature_archived` while
leaving existing balances intact until their natural cycle end.
*Consequence:* the audit and event history never dangles.

---
*Verified by:* NFR-D4, for the durability of the history an archive preserves, and the `Endpoint × error matrix` contract test for `403 feature_archived`.

### DR-046 — A wide `apply-now` requires a confirmation token
`POST /v1/admin/plans/{plan_id}/apply` first counts the assigned tenants the change would affect. If
that count exceeds `QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD` the call applies nothing and returns
`409 confirmation_required`, carrying `affected_tenants`, the earliest affected `cycle_end`, and a
single-use `confirmation_token`. Re-issuing the call with `confirmation_token` present and the
plan's `resource_version` unchanged applies the change.
*Rationale:* `apply-now` re-anchors every assigned tenant at once, and the cost of that grows with
the number of assigned tenants. A guard that engages on blast radius rather than on operator intent
is the only one that catches the accident, because the operator's intent is exactly what is wrong in
the accident.
*Consequence:* a token is consumed on use, is bound to the plan `resource_version` it was issued
against, and expires on `QUOTACORE_CONFIRMATION_TTL`. A plan edited between issue and use invalidates
the token, which forces a fresh count rather than applying a stale number. The threshold is a
configuration key rather than a constant so that the guard can be moved without a code change; its
default is an operator-workflow judgement, sourced and dated in
[a-01-design-partner-validation.md](../research/a-01-design-partner-validation.md).
*Answers:* [Q-04](../product/assumptions-and-open-questions.md#2-open-questions), which asked
whether a destructive bulk re-anchor gets a confirmation step.
*Verified by:* T-12, which asserts that a call above the threshold applies nothing and returns the
token, that a call at or below it applies without a token, and that a second use of the same token
is rejected.

## Balances and consumption

### DR-017 — A balance may never be reduced below zero by consumption
`consume` is all-or-nothing. If the balance is less than the requested amount, the request is
denied, and **no** partial deduction occurs.
*Example:* balance 50, request 100 → denied, balance remains 50. A "partial success" of 50 is
never returned. This also holds for a variable cost: a request that estimates cost and then
discovers a higher actual cost must deduct the higher amount atomically or fail; it must never
deduct the estimate and allow the difference.
*Verified by:* T-01.

### DR-018 — Consumption never goes negative; administration may
`consume` cannot produce a negative balance. An admin operation may set a negative balance
explicitly, to suspend consumption for an abusive customer, and the system honours it: every
subsequent `consume` is denied until the next boundary or an admin correction.
*Consequence:* a negative balance is a deliberate administrative state, distinguishable in the
event history by `source = admin`.
*Verified by:* T-01, for the atomic refusal of an over-deduction, and T-08 for the denial that follows an administratively negative balance.

### DR-019 — The allowance ceiling is `limit + bonus`
For every cycle, at every instant:
```
balance <= limit + bonus
```
where `bonus` is the total granted outside the plan during the current cycle. The invariant is
enforced inside the atomic script, not in application code.
*Consequence:* a refund, a grant and a consume can never produce a balance above this ceiling.
This is what makes it safe for the runtime scope to hold refund rights (ADR-0005).
*Verified by:* T-04, and the property test that the balance stays within `limit + bonus` at every step.

### DR-020 — `bonus` is per-cycle and does not accumulate
An admin grant increases `bonus` for the current cycle only. At rollover, `bonus` returns to
zero and the balance becomes the new cycle's `limit`.
*Consequence:* a grant made on the 30th of a monthly cycle is worth the remainder of that cycle
only. Documented in the API reference, because it surprises people, and it is the reason a
support grant for a long extension is repeated rather than issued once.
*Alternative rejected:* carrying bonus forward, which requires deciding how a carried bonus
interacts with a shrinking plan limit.
*Verified by:* T-04, and T-05 for the return to zero at rollover.

### DR-021 — Refunds credit the current cycle
A refund always credits the balance of the cycle in effect when the refund is applied. Refunds
are never applied to a closed cycle.
*Consequence:* a refund issued after a boundary for work performed in the previous cycle credits
the new cycle. Where the ceiling in DR-019 prevents that, the refund is refused with
`409 refund_exceeds_grant` and an admin grant is the correct remedy.
*Verified by:* T-04, for the `409 refund_exceeds_grant` refusal, and T-11 for the attribution of the credit to the open cycle.

### DR-022 — Amounts are signed 64-bit integers in the feature's unit
`amount` is an `int64` count of the feature's unit — tokens, requests, seconds, minutes. There
are no fractional amounts in the MVP.
*Consequence:* a feature requiring fractional accounting (for example, per-thousandth of a
second) must be configured in a scaled integer unit, such as `millisecond`, or the feature is
not representable. This is a deliberate exclusion, not an oversight, and the alternative — a
decimal type — is deferred.
*Verified by:* the `2^53` bound script test, which asserts the limit is enforced inside the script as well as at the API and in the DDL.

### DR-023 — An amount of zero is valid; a negative amount is not
`amount: 0` is a well-formed request that is always allowed and records no event. A negative
`amount` on `consume` or `refund` is a `400 invalid_amount`. Direction is expressed by the
endpoint, never by a sign (ADR-0005).
*Verified by:* the `Endpoint × error matrix` contract test, for `400 invalid_amount`, and the `Argument validation inside the script` script test, for the third enforcement point.

### DR-024 — `check` is advisory and is never a gate
`POST /v1/check` reports what would happen. It performs no deduction. It is atomic with respect
to concurrent consumption — it observes a consistent state — but the state it reports may be
stale by the time the caller acts.
*Consequence:* "check then act" is a user-experience optimisation, not a correctness mechanism.
The only correctness mechanism is `consume`. A caller that gates on `check` and skips `consume`
can be overdrawn by a concurrent request. This is stated in the API reference explicitly.
*Verified by:* NFR-L5, measured separately from `consume` precisely because `check` is not on the enforcement path, and the `Endpoint × error matrix` contract test, which requires `check` to enumerate every code it can return.

### DR-025 — Denials do not change state
A denied `consume` records no usage event, consumes no idempotency key, and does not alter the
balance. It is counted in metrics only.
*Rationale:* denials have no side effect, so there is nothing to protect against double-applying,
and re-evaluating a denial lets a client that was refused a moment ago succeed after an admin
grant without the client having to detect the state change.

---
*Verified by:* T-08, and the property test that a denial changes nothing.

## Idempotency and correctness of effect

### DR-026 — An idempotency key is required on `consume` and `refund`
The client generates one key per **logical operation** and reuses it across every attempt of
that operation, including retries.
*Consequence:* a client that generates a fresh key per attempt receives no protection. The
requirement is documented in the API reference, generated automatically by the v0.5 SDKs, and
enforced with `400 validation_failed` when absent.
*Verified by:* T-02, and the `Endpoint × error matrix` contract test for `missing_idempotency_key`.

### DR-027 — A repeat with a matching fingerprint replays the recorded result
The fingerprint is a hash of operation, tenant, feature and amount. A repeat with a matching
fingerprint returns the originally recorded response with `replayed: true` and applies no
further change.
*Verified by:* T-02.

### DR-028 — A repeat with a mismatched fingerprint is rejected
Reusing a key for a different operation is `409 idempotency_key_reuse`. It is never silently
accepted and never treated as a fresh request.
*Rationale:* accepting it would let a client corrupt its own accounting in a way that is
extremely hard to diagnose.
*Verified by:* T-03.

### DR-029 — The idempotency window is 24 hours
Recorded outcomes expire 24 hours after application. A key first seen more than 24 hours ago is
treated as new.
*Verified by:* NFR-D2, which measures the window the guarantee depends on, and T-02 for the replay inside it.

### DR-030 — Idempotency prevents in the data plane and only detects in the control plane
The atomic check is performed inside the same script execution as the mutation, and is the only
thing that prevents a double charge. The unique constraint on `usage_events` is asynchronous and
detects a duplicate after the fact; it cannot prevent one.
*Consequence:* the API documentation must not imply the database protects the customer on the
hot path, and no design may rely on the control-plane constraint for correctness.

---
*Verified by:* T-02, for prevention inside the script execution, and T-11, for the detection that reconciliation is expected to find afterwards.

### DR-048 - An idempotency record is not evictable inside the window
The fast store runs `maxmemory-policy noeviction` for the whole key space. An idempotency record
lives for the full `QUOTACORE_IDEMPOTENCY_TTL` window, and the store is sized to hold the working
set that window produces at the throughput in NFR-T1. A write rejected for capacity is a
`503 service_unavailable`, never a silent eviction.
*Rationale:* inside the window the record is the only thing standing between a retrying client and a
second deduction. A policy that may discard it converts a capacity problem into a billing error
that no operator sees and no metric that was not already counted shows.
*Consequence:* the deployment guide states the memory floor implied by the window, and the floor is
a measured figure owned by IP-15 rather than an estimate, because an estimated floor that is too low
becomes an availability incident. The balance-hash cost that `allkeys-lru` would have imposed no
longer applies, which strengthens DR-045 rather than weakening it: under `noeviction` a balance hash
is lost only through total store loss, not through memory pressure.
*Alternative rejected:* an LRU policy with a `qc:*:idem:*` key space protected by a separate
instance, because one instance has one `maxmemory-policy` and a second store contradicts the
single-binary deployment (ADR-0001).
*Verified by:* T-13, which fills the store to its configured
limit and asserts that a `consume` is refused with `503` rather than allowed to displace an existing
idempotency record.

### DR-049 - A duplicate that reconciliation detects is reversed automatically
When the `usage_events` unique constraint reports a duplicate `event_id`, reconciliation issues a
`refund` for the second deduction against the same idempotency key, using the first-class refund
operation (ADR-0005). The reversal is idempotent on `event_id` and is recorded in the audit log.
*Rationale:* `noeviction` closes the capacity hole but not the restart hole, because the store is
rebuilt rather than backed up (NFR-OPS4) and an idempotency record is not derivable from the ledger,
which records usage and not keys. A total store loss inside a window therefore still admits a
double charge, and detection without remedy would leave the customer carrying it.
*Consequence:* the second claim is "charged once, and a double charge caused by store loss is
reversed", not "charged once, unconditionally". The correction is eventual rather than immediate,
which is a weaker promise stated honestly rather than a stronger one that is not true. A detected
duplicate is therefore an alert condition, not an informational counter.
*Verified by:* T-14, which drops the fast store mid-window, replays the original request, and
asserts that reconciliation issues exactly one refund and that the tenant's net balance matches a
single charge.

## Tenants

### DR-031 — Provisioning is explicit
A tenant must exist before it can consume or be checked. There is no implicit creation on first
use and no lazy hydration from a control-plane read on the enforcement path.
*Consequence:* an unprovisioned tenant is `404 tenant_not_found`, which is a fail-closed answer.
The SDKs retry `404` for a bounded period immediately after a provisioning call, to absorb the
configuration-propagation delay documented in
[api-conventions](../architecture/api-conventions.md).
*Verified by:* NFR-D3, for the propagation delay the SDKs absorb, and T-10, for the absence of a key created by a request after a restart.

### DR-032 — A suspended tenant is denied, and its state is preserved
A suspended tenant is refused with `403 tenant_suspended` on every enforcement request. Its
balance, cycle window and history are untouched, and it resumes exactly where it was.
*Consequence:* suspension is not deletion, and not a balance reset. It is reversible without loss.
*Verified by:* T-10, which asserts the balance, window and history survive a restart untouched, and the `Endpoint × error matrix` contract test for `403 tenant_suspended`.

### DR-033 — A tenant is created with a plan, a time zone and an anchor, and may be changed
The time zone may be changed at any time and applies to future boundaries without re-anchoring.
The anchor is changed only by a plan assignment change (DR-013) or an explicit apply-now
(DR-014).
*Consequence:* changing the time zone does not restart a cycle, which is almost never what an
operator wants and is therefore almost never surprising.
*Verified by:* the `tenants` table in data-model.md, which requires `plan_id`, `timezone` and `anchor_at`, and T-10 for the anchor surviving a restart.

### DR-034 — Tenant identifiers are opaque and bounded
`external_id` matches `[A-Za-z0-9._:-]{1,128}`. The character set deliberately excludes `@`, so
an email address cannot be a valid tenant identifier. Enforced at the API boundary.
*Consequence:* no personally identifiable information is required or accepted as a tenant
identifier (ADR-0013). `tenant.metadata` is a small JSON object, explicitly not PII, and is not
indexed or searched by content.
*Verified by:* NFR-S2, and the `No PII can be stored` security test, which attempts an email-like
`external_id` and asserts the schema constraint rejects it.

### DR-035 — Deletion is soft and blocked while referenced
A tenant may be soft-deleted. Its event and audit history is retained. A plan that is assigned
to any tenant, including a soft-deleted one, may be archived but not deleted.

---
*Verified by:* the foreign keys in data-model.md, which make deletion of a referenced plan impossible, and NFR-D4 for the retention of history.

### DR-047 - Soft deletion is not retroactive
A soft delete takes effect for every request that reads the tenant after the delete commits. A
request that entered the protected script before the delete committed may complete, and its effect
stands. Deletion is never unwound, and a completed request is never retroactively denied.
*Rationale:* the delete and the request are ordered by the commit point, and that ordering is the
only thing both participants can observe. Retroactively denying a request the server already
acknowledged would leave the caller holding a deduction the ledger does not show, which is worse
than the alternative.
*Consequence:* the audit log shows both facts, the delete and the later completion, and the
deletion is not a mechanism for undoing a deduction. An operator who needs the effect reversed uses
`POST /v1/refund`, which is a first-class operation (ADR-0005) rather than a side effect of deletion.
*Answers:* [Q-16](../product/assumptions-and-open-questions.md#2-open-questions), which asked what
happens to a request that is in flight when a tenant is deleted.
*Verified by:* T-15, which races a consume against a delete and asserts that a completed consume
stands, and FS-20 for the response an in-flight request receives when the tenant is already deleted.

## Availability and failure

### DR-036 — The data plane is authoritative at runtime; the control plane is authoritative for schedule
The balance in the Redis-compatible store is what enforcement reads. Postgres is the authority
for tenant configuration and for which cycle a tenant is in.
*Consequence:* the two can disagree transiently, and every mechanism that writes enforcement
state is either monotonic (ADR-0003) or reconstructable from the ledger (ADR-0016). No mechanism
is allowed to be a non-idempotent overwrite.
*Verified by:* NFR-A2, which requires enforcement to survive a control-plane outage, and NFR-D1 for balance consistency between the two stores.

### DR-037 — Failure is fail-closed, and it is fast
If enforcement cannot be completed, the answer is a `503` within the latency budget, never a
hang, never a partial deduction, and never a silent allow.
*Consequence:* the customer chooses whether to fail open or fail closed in *their* application.
Quotacore never chooses for them, because the correct choice is a business decision — giving the
service away versus blocking a paying customer — and no infrastructure component should make it
unilaterally.
*Explicitly rejected:* a server-side "fail open" configuration, which would convert a
dependency outage into a revenue outage.
*Verified by:* NFR-L4, which bounds the tail when no dependency answers, and NFR-A3 for availability during a data-store outage.

### DR-038 — An unknown tenant is an error, not an allowance
`404 tenant_not_found`. Never an implicit zero allowance, never an implicit allow, and never an
implicit creation (DR-031).
*Verified by:* the `Endpoint × error matrix` contract test, which fails any endpoint that answers an unknown tenant with anything but `404 tenant_not_found`.

### DR-039 — The request path may not block on the control plane
A cache hit is answered entirely from the bounded in-process snapshot. A cache miss may perform one
bounded, rate-limited configuration lookup, and that is the only control-plane read on the request
path. No per-request query, no query "just for this one feature", and no read that scales with
traffic.
*Consequence:* an uncached tenant during a control-plane outage is `503
control_plane_unavailable`, distinguishable by the customer from a quota denial so that the
correct operational response is taken. Enforcement for cached tenants is unaffected.
(ADR-0001)
*Verified by:* NFR-L2, which the request path must still meet with the control plane unreachable, and NFR-L6, measured separately so that a regression in either is attributable.

### DR-040 — A rate-limited service is a distinct outcome from an exhausted quota
The service's own protective rate limiting returns `429 rate_limited` with a `Retry-After`. An
exhausted business allowance returns `429 quota_exceeded` with the cycle end. They are different
codes, different remedies, and a client must be able to tell them apart without string matching.

---
*Verified by:* the `Endpoint × error matrix` contract test, which keeps the two 429 codes distinct, and NFR-O7 for the alert that fires on each.

## Audit

### DR-041 — Every administrative mutation is audited
Every create, update, archive, delete, grant, set, override, apply-now, force-rollover, key
issue and key revocation writes an `audit_log` row with actor, action, subject, before, after
and request identifier.
*Consequence:* the audit log is the authoritative record of who changed a customer's entitlement.
*Verified by:* NFR-S5, which requires an admin action to be attributable to a key and to record before and after values.

### DR-042 — Every applied balance mutation is recorded as a signed delta
`consume`, `refund`, `grant`, `set`, plan apply-now and cycle rollover each attempt exactly one
`usage_events` row. Denials attempt none (DR-025).
*Invariant:* within a cycle, `balance = cycle_opening_allowance + Σ(delta)`. This is asserted by
a test and is the recovery mechanism (ADR-0016).
*Stated limit:* "attempted" is deliberate. The append is at-least-once through a bounded buffer
that drops rather than blocks the hot path, so a saturated ledger loses rows rather than latency
(FS-10). A loss is counted in `quotacore_event_sink_dropped_total` and leaves a detectable gap, so
the invariant holds over the events that were written and the gap is what reconciliation checks. A
claim of a complete history would be false, and the error catalogue says so.
*Verified by:* T-11, and the property test that the balance equals the opening allowance plus the sum of the deltas within a cycle.

### DR-043 — Secrets are never logged
API keys are logged only as their public prefix. Request bodies, authorization headers, and
`tenant.metadata` free text are not written to logs by default. (SECURITY requirements.)
*Verified by:* NFR-S1, which requires that no key is recoverable from any store, log or metric, and NFR-O6 for the absence of personally identifiable information in telemetry.

### DR-044 — Audit records are append-only
No endpoint updates or deletes an `audit_log` row. Retention is applied by a bounded background
deletion with its own configuration, never by an API call.
*Verified by:* NFR-C6, which requires retention to be applied by a background job rather than by an API call, and NFR-D4 for audit durability.

### DR-045 — A balance is provisioned, never lazily re-granted
A balance hash is materialised exactly twice: when the tenant is provisioned or its plan assignment
changes, and when a script observes a cycle transition on a key that already exists. An absent
balance hash encountered on the request path is **not** an initialisation opportunity. It is a
fault: the service returns `503 service_unavailable`, increments
`quotacore_balance_key_missing_total`, and queues the tenant-feature for the ledger rebuild in the
recovery runbook.
*Consequence:* a mid-cycle loss of the fast store degrades enforcement for the affected
tenant-features to fail-closed until the rebuild runs. It never restores a full allowance, because
a full allowance is indistinguishable from free service, and the data store's eviction policy is
not permitted to create it. The rebuild is an explicit, audited operation, never a side effect of a
request.
*Verified by:* the `A balance hash that does not exist` script test, which asserts `503 service_unavailable` and that the script never initialises from `ARGV`, and T-11 for the rebuild that restores the balance.
