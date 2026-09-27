# ADR-0005 — Refund is a first-class operation with a hard invariant

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [request lifecycle](../architecture/request-lifecycle.md), [domain rules](../product/domain-rules.md), [research: idempotency and refunds](../research/idempotency-and-retries.md)

## Context

A consumption engine that can only subtract has no way to correct itself, and real AI and API
workloads are full of cases where the amount deducted is wrong:

- **Over-reservation.** A caller that does not know the eventual cost of an operation
  deduucts the maximum it might need, then returns the difference. A 200,000-token generation
  capped at `max_tokens` consumes 200,000 units of which it may use 340. Without a credit-back
  the customer is charged 199,660 units they never spent, and the complaint is not a bug report,
  it is a churn event.
- **Failed operations.** An upstream provider times out or returns an error after the customer
  was already charged. The customer's app owes the user nothing; the quota system owes the
  customer a credit.
- **Allocation caps.** The product's own motivating example is "free tier users can create up to
  3 projects". That is not a consumable quota, it is a cap on simultaneously existing entities.
  It can only be expressed if creating a project decrements a counter and deleting a project
  increments it back.
- **Support concessions.** "Give this customer 500 more tokens" is a refund with a reason
  attached.

The previous project history specified `/consume` and `/check` and nothing else. Without a
credit-back path, the first mispriced request in production becomes a manual database edit,
and manual database edits against enforcement state are the mechanism by which a quota engine
loses its audit trail.

## Options considered

**A — No refund. Over-reserve exactly.** Requires the caller to know the exact cost before
deducting.
- Cons: impossible for LLM token counting, where cost is only known after the call. Rejected.

**B — Negative amounts on `/consume`.** One endpoint, direction inferred from the sign.
- Pros: one code path, one script.
- Cons: a signed amount makes the API's failure modes ambiguous — is a negative amount a
  refund, a typo, or an attempt to mint quota? It also makes the request log and the event
  stream harder to read, and it puts the safety check in the wrong place. Rejected in favour of
  an explicit verb.

**C — Admin-only correction.** Support edits a balance directly.
- Cons: correct for concessions, useless for the per-request case, which is the one that
  happens thousands of times a day. Rejected as the *only* mechanism.

**D — First-class `/v1/refund` with a hard cap invariant (chosen).** An explicit operation,
callable with the runtime scope, bounded by an invariant that makes it incapable of creating
quota.

## Decision

**`POST /v1/refund` is a first-class runtime operation, with an invariant enforced inside the
Lua script.**

The invariant is:

```
balance <= limit + bonus
```

where `limit` is the effective limit for the current cycle and `bonus` is the total amount ever
granted outside the plan, by an admin, within the current cycle. `bonus` is a hash field
maintained only by admin operations; the runtime scope cannot change it.

A refund that would take the balance above `limit + bonus` is rejected with
`409 refund_exceeds_grant`, and the balance is unchanged. This is what makes it safe to give
the runtime scope refund rights at all: a buggy or hostile client that calls `/v1/refund` in a
loop, or refunds an amount it never consumed, cannot inflate its own allowance beyond what it
was already granted.

Consequences that follow directly from the invariant and must be documented as behaviour:

- Refunding more than was consumed **in aggregate** is allowed up to the cap, because the cap,
  not the cycle total, is the guard. A customer who received a 500-unit bonus can be refunded
  up to 500 even if they consumed nothing.
- A refund after the cycle has rolled over credits the **current** cycle, not the old one.
  Cross-cycle refunds are not supported; see [domain-rules](../product/domain-rules.md) DR-011.
- Refunds are recorded as positive `usage_events` deltas with a required `Idempotency-Key`, so
  double refunds are prevented by ADR-0004 in the same way double charges are.
- `/refund` requires a `reason` on the admin path. On the runtime path the reason is optional
  and free-form, capped in length, because a library may not know why it is crediting back.

## Rationale

Correctness under LLM pricing is not a nice-to-have; it is the product's reason for existing in
its target segment. A quota engine that can only subtract is unusable for token metering, and
token metering is the demand the evidence in [competitive-landscape](../../docs/research/competitive-landscape.md)
points at.

The cap is the part worth arguing about. A naive refund endpoint is a quota-minting primitive
with an HTTP interface. Binding it to `limit + bonus` means the total allowance in any cycle can
only ever grow through an *admin* action, which is audited, attributable and reversible. That
property is what allows refund to be a runtime operation rather than an admin one, which in turn
is what makes it usable from a customer's request handler at all.

Making it a separate verb rather than a negative amount keeps the audit trail readable — the
event stream says `consume` or `refund`, not a signed integer that a reader has to interpret —
and keeps validation errors specific.

## Consequences

**Positive**
- Over-reservation is expressible: deduct the maximum, credit the remainder. This is the
  standard pattern for LLM integration and it now works.
- Failed upstream operations can be corrected without engineering intervention.
- "Max 3 projects" is expressible as `consume 1` on create, `refund 1` on delete, giving an
  accurate live count of active entities.
- Support concessions use the same machinery, so every balance change has one code path and one
  event type.
- The runtime scope can hold refund rights without becoming a quota-minting hole.

**Negative**
- A third enforcement script, and a third operation to test for atomicity and idempotency.
- The cap can reject a legitimate refund in a specific case: a customer who was granted a bonus
  in cycle 1, consumes in cycle 2, and is refunded in cycle 2 for a cycle-1 operation. The
  refund is refused because the bonus has expired. This is a real limitation, documented rather
  than engineered around, with the workaround being an admin grant.
- `bonus` is per-cycle state that resets with the cycle, so the cap is per-cycle. Documented.

## Revisit when

- A reservation lifecycle (`reserve` → `commit`/`release`) is introduced, which is the natural
  generalisation of over-reservation and would subsume this operation.
- Cross-cycle refunds are required, which needs a decision about which cycle's bonus a refund
  draws against.
