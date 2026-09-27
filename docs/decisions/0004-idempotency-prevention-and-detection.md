# ADR-0004 — Idempotency: prevention in the data plane, detection in the control plane

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [request lifecycle](../architecture/request-lifecycle.md), [api conventions](../architecture/api-conventions.md), [data model](../architecture/data-model.md), [research: idempotency](../research/idempotency-and-retries.md), [SECURITY.md](../../SECURITY.md)

## Context

The enforcement path is a network call from the customer's application. Networks lose
responses. A customer that sends `POST /v1/consume` and does not receive a response cannot
distinguish "the server never processed it" from "the server processed it and the response was
lost". The only safe client behaviour is to retry, and a naive retry charges the customer twice
for one operation.

This is not a theoretical concern. Retry amplification is endemic in AI applications: agent
frameworks, HTTP client libraries and SDKs each add their own retry layer, and a single logical
action can reach the enforcement path two or three times. Double-charging a customer is
immediately visible, immediately expensive in trust, and is one of the first things an
integrating engineer will hit in production.

The previous project history contained no idempotency mechanism at all.

## Options considered

**A — Rely on the customer to retry safely.** Document "do not retry non-idempotent requests".
- Cons: unsatisfiable. Retrying is the correct response to an ambiguous network failure, and
  a system whose correct usage requires suppressing the correct client behaviour is a system
  that will be misused. Rejected.

**B — Client-supplied idempotency key, stored in Redis (chosen, together with C).**
The customer generates a key per logical operation, sends it on every attempt, and the server
replays the original result for a repeat.
- Pros: the customer's retry storm is a non-event; double-charging becomes structurally
  impossible. Standard practice, already implemented by payment providers and LLM gateways.
- Cons: requires a key. Optional keys mean a silent correctness hole.

**C — Deduplicate on a request fingerprint instead of a key.** Hash
`(tenant, feature, amount, arrival-second)`.
- Cons: genuinely distinct operations that happen to look identical are collapsed. Two
  legitimate 150-token completions in the same second are one operation under this rule. It
  destroys revenue, not just precision. Rejected.

**D — Deduplicate at the ledger in Postgres with a unique constraint.** This is what several
competing systems do, and it works for a system whose write path is already a database write.
- Cons: it requires a Postgres write on the enforcement path, which is exactly the dependency
  ADR-0001 removed. It also cannot *prevent* the double charge — the Redis decrement has already
  happened by the time the insert fails.

**E — Deduplicate on the event stream only.** Insert-only-after-the-fact detection.
- Cons: same problem as D, worse. Nothing prevents the charge; it only reports it later.

## Decision

**Both B and D, each doing the one thing it is actually capable of, with the distinction stated
plainly in the API contract rather than blurred.**

**Layer 1 — prevention, in the data plane.** The idempotency check and the balance mutation
happen in the *same* Lua script execution, so they are atomic with respect to each other and
with respect to any concurrent request.

- The key is `qc:{t:<tenant>}:idem:<idempotency_key>`, in the tenant's hash slot, so it is
  reachable by the same script that mutates the balance.
- The script stores a fingerprint of the request — a hash of operation, tenant, feature and
  amount — alongside the recorded result, with a **24-hour** expiry.
- On a repeat with a **matching** fingerprint, the stored result is returned with
  `replayed: true`. No balance change occurs.
- On a repeat with a **mismatched** fingerprint, the request is rejected with
  `409 idempotency_key_reuse`. Silently accepting a reused key for a different amount is how
  quota engines lose money in ways nobody notices.
- **Only mutations are recorded.** A rejected consume records nothing, so a retry of a
  rejection is re-evaluated against current state. This is deliberate: rejections have no side
  effect, so there is nothing to protect against double-applying, and re-evaluating lets a
  client that was refused a moment ago succeed after an admin grant.
- **`Idempotency-Key` is required** on `/v1/consume` and `/v1/refund`. Not optional. An optional
  key is a correctness hole that will be discovered in production, not in review.

**Layer 2 — detection, in the control plane.** Every applied mutation is written to
`usage_events`, which carries `idempotency_key` with a unique constraint on
`(tenant_id, idempotency_key)`. Because those writes are asynchronous, this layer cannot
prevent a double charge. Its only job is to *detect* one after the fact, which is why the
failure is counted, alerted on, and attributable.

The API documentation and the SDK documentation both state which layer does what. A reader
should never come away believing the database is protecting them on the hot path.

## Rationale

Layer 1 is the only layer that can prevent, because prevention requires the check and the
mutation to be atomic with respect to concurrent requests — which means inside the datastore.
Layer 2 is still worth having because a Redis flush, a failover to a replica that never
received a write, or a bug in the script itself will produce a duplicate that only the
durable record can surface. Claiming the database prevents duplicates would be a false
guarantee; omitting it would leave a real failure unobservable.

Requiring the key rather than defaulting it is the difference between a guarantee and a
suggestion. A default-generated key would be generated per attempt by a naive client and would
protect nothing while appearing to.

## Consequences

**Positive**
- Client retries, including multiplicative retry stacks, cannot double-charge.
- A reused key with different parameters is a loud, attributable error rather than silent
  corruption.
- Duplicate charges remain detectable and alertable even if the data plane is bypassed or lost.
- The 24-hour window matches the practical retry horizon: no legitimate client retries a
  completed billing-relevant operation the next day.

**Negative**
- One additional key write per mutation, inside the atomic block. It is a `SET` with an expiry
  in the same slot, so it costs no extra round trip, but it does add work to the script.
- Keys accumulate at 24 hours. At 25,000 requests per second sustained, that is a large
  population; the memory cost is bounded per entry and is a documented capacity-planning input
  in [deployment.md](../architecture/deployment.md). Mitigations, if a deployment needs them,
  are a shorter window or per-tenant key namespacing, both post-MVP.
- A customer that generates a fresh key per retry defeats the mechanism. The API docs state the
  requirement, and the v0.5 SDKs generate one key per logical operation automatically.
- A genuine, deliberately duplicated operation — a customer who really does want to charge a
  customer twice for two identical actions — must send two different keys. This is documented,
  because it surprises people.

## Revisit when

- Enforcement traffic makes the idempotency key population the dominant memory cost, at which
  point a per-tenant ring or a probabilistic admission filter becomes preferable to full
  retention.
- Reservations are introduced (a `reserve`/`commit`/`release` lifecycle rather than a direct
  decrement), which changes the atomic unit and requires this design to be re-derived.
