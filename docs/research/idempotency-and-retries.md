# Research: Idempotency and Retry Safety for Metered Operations

**Date:** 2026-09-27 · **Status:** complete · **Decision:** [ADR-0004](../decisions/0004-idempotency-prevention-and-detection.md), [ADR-0005](../decisions/0005-refund-as-first-class-operation.md)

## Question

For an operation that moves a customer's balance, what can the server guarantee about retries,
what can it not guarantee, and where should the boundary between server and client responsibility
be drawn?

## Why it mattered

Every metered system eventually charges somebody twice, and the cause is almost never a bug in the
deduction logic. It is a timeout, a proxy retry, a client library with automatic retries, or a user
double-clicking. The question is not whether this happens — it is whether the design makes it
impossible, makes it detectable, or leaves it to hope. Getting the answer right is what separates
a metering system that can be trusted with a pricing page from one that cannot.

## Method

Worked through the retry scenarios systematically rather than treating idempotency as a feature to
be added, then examined how the established providers in this space handle it, since they have all
had the double-charge incident that motivates the design.

## Findings

### The scenarios

Every one of these is a real failure mode, not a hypothetical.

| # | Scenario | What a naive implementation does | What a correct one does |
| --- | --- | --- | --- |
| S1 | The client times out after the server applied the deduction, then retries **with the same key** | Charges twice | Replays the stored response. Charged once |
| S2 | The client times out, then retries **with a new key** | Charges twice | **Charges twice.** Undetectable server-side. Must be disclosed |
| S3 | A proxy or an HTTP library retries automatically, generating a fresh key | Charges twice | Same as S2. A retry-rewriting intermediary is indistinguishable from a client that changed its key |
| S4 | The user double-clicks, and the application sends two distinct requests | Charges twice | Two logical operations. Arguably correct: they are two operations |
| S5 | The same logical operation is retried after 25 hours | Charges twice | Correct. The idempotency window has expired (DR-029) |
| S6 | A key is reused for a *different* operation by mistake | Charges twice, or corrupts the accounting | `409`. The original record is preserved so the original outcome is still retrievable |
| S7 | The server applies the deduction, then crashes before responding, and the client retries | Charges twice | Replays, if the record was written atomically with the mutation |
| S8 | The server applies the deduction but the idempotency record write fails | Charges twice on retry | **The most important case.** It is why the record write is in the same atomic execution as the deduction |
| S9 | The client sends the same key concurrently 50 times | 50 charges | One charge, 49 replays each marked `replayed: true` |
| S10 | The client retries with the same key but a different amount, because it recalculated the cost | Rejected or silently accepted | `409 idempotency_key_reuse`, never silently accepted |

**S8 is the case that determines the design.** If the deduction and the idempotency record are
written in two separate operations, then every failure between them produces a double charge. There
is no clever algorithm that fixes this; the only fix is to make them one indivisible operation,
which means one script execution ([atomicity research](atomicity-mechanism-options.md)).

### S2 and S3: the honest limit

**This is the most important finding in the document, and it is a limitation rather than a
solution.** A server cannot detect that two requests carrying different idempotency keys represent
one logical operation, because nothing in the requests says so. A retry-rewriting proxy makes it
worse, and no amount of server-side design fixes it.

The available mitigations were evaluated:

| Mitigation | Effectiveness | Cost |
| --- | --- | --- |
| **Require an idempotency key, document the derivation** | High, if the client derives it from a durable operation identifier | One header, and a documented convention |
| **SDKs that generate and reuse keys across retries** | High, and it removes the reasoning burden entirely | The v0.5 work. Until then, the customer must do it |
| **Content-based deduplication** — reject a request identical to a recent one | **Rejected.** A legitimate repeat consumption of the same feature and amount is common and would be wrongly rejected. It would also be trivially evaded by a one-unit change | Severe false positives |
| **A short automatic deduplication window on identical request bodies** | **Rejected.** Same problem. Two identical legitimate operations in the same second are ordinary traffic |
| **Sequence numbers per client** | Rejected. A client without a durable store for its sequence numbers is worse off, and it is the same problem with more moving parts | — |
| **Server-side "same request within N ms" heuristic** | **Rejected.** Undetectable in the ambiguous cases, and it introduces a failure mode of its own: rejecting a legitimate operation | — |

So the design accepts the limit, and does three things about it:

1. **Requires** the key, so a compliant client is protected (DR-026).
2. **Discloses** the limit explicitly, in the API reference, in the use case and in
   [FS-03](../product/error-catalog.md) — because a customer who is told will handle it, and a
   customer who is not will file a bug we cannot reproduce.
3. **Makes the SDKs fix it**, which is the real answer and the strongest argument for the v0.5
   release. Making the key optional would have been the tempting compromise and is precisely wrong:
   it would make the *default* behaviour the unsafe one.

### The fingerprint

A repeat is a replay only if the request describes the same operation. The fingerprint is a hash of
`operation | tenant | feature | amount`:

| Repeat | Fingerprint | Result |
| --- | --- | --- |
| Same key, same operation | Matches | Replay the stored response, `replayed: true` |
| Same key, different feature | Differs | `409 idempotency_key_reuse` |
| Same key, same feature, different amount | Differs | `409 idempotency_key_reuse` |
| Same key, different tenant | Scoped per tenant, so it is a different record | Two independent operations |

**Including the amount is the correct and slightly uncomfortable choice.** A client that retries
after recalculating the cost gets a `409` rather than a silent success. That is the right
behaviour: the two requests are genuinely different operations, and applying the smaller one and
ignoring the larger would be worse than refusing. The error message says so.

**`request_id` must not be part of the fingerprint.** A retry has a new request identifier by
definition, so including it would make every retry a mismatch. This is a small detail that is very
easy to get wrong and produces a system where retries always fail.

### Why the record is stored, and what is stored

| Field | Purpose |
| --- | --- |
| Fingerprint | Distinguish a replay from a misuse |
| Transition verdict | Whether the window was rolled, so the replay answers the same window the first delivery saw |
| Response body | Replay the original decision's state, so a retry reads what the first delivery saw (T-02) |
| Expiry | 24 hours (DR-029) |

**The decision state is stored rather than recomputed.** A replay must return the balance and the
window *as they were when the mutation was applied*, not as they are now, and the only way to do that
is to store them. Storing them is also what makes the seven state values identical across a retry,
which is what stops clients from recomputing and concluding the service is inconsistent; the retry is
marked with `replayed: true` and the `Idempotent-Replay` header rather than by a difference in the
state (DR-027).

**A denial is not recorded** (DR-025). A denial changed nothing, so there is nothing to protect
against re-applying, and recording it would mean a client could not retry a denied operation after
an administrative grant — it would keep getting the stale denial replayed. Not recording denials is
both simpler and more correct.

### Key scope and lifetime

| Decision | Value | Reasoning |
| --- | --- | --- |
| Scope | Per tenant | Two tenants using the same key string are two unrelated operations. A global namespace would make unrelated collisions a `409`, which would be baffling |
| Window | 24 hours | Long enough to cover every realistic retry chain including a multi-hour client-side backoff; short enough that the key space stays bounded (the sizing analysis in [deployment](../architecture/deployment.md#5-sizing)) |
| Storage | The fast store, authoritative | Only an atomic execution can prevent S8 |
| Control-plane mirror | A unique index in Postgres, detection only | Asynchronous, and it cannot prevent anything (DR-030) |
| Reuse after expiry | A new operation | Documented, and consistent with a bounded window |
| Generation | Client-supplied | A server-generated key cannot be stable across a retry, which is the entire requirement |

**Why 24 hours specifically.** It is not derived from anything; it is a judgement based on two
bounds. Below about an hour, a client with an aggressive backoff or an overnight job retrying
overnight falls out of the window. Above about a week, the key space at any realistic volume
becomes a memory problem and the records are stale enough that a replay would be misleading. 24
hours sits between them, and it is fixed rather than configurable: DR-029 makes the 24-hour window
part of the customer-facing contract, so a customer integrating against it can rely on it. The
`QUOTACORE_IDEMPOTENCY_TTL` key exists for operational testing and is pinned to 24 hours; changing
it is a contract change and needs an ADR, not a config edit. Whether the *storage* for that window
is allowed to be evictable is a separate question, tracked as Q-21.

### The relationship to refunds

`refund` carries an idempotency key with identical semantics, and the atomic script also enforces
the ceiling (DR-019). The interaction that needed care:

| Situation | Problem | Resolution |
| --- | --- | --- |
| The same refund retried with the same key | Would credit twice | Idempotency, as above |
| A legitimate second refund of the same amount, same key pattern | Two operations | Distinct keys. Documented |
| A refund that would exceed the ceiling | Would inflate the balance | `409 refund_exceeds_grant`, which is also the control on the runtime scope's refund rights (ADR-0005) |

## What was rejected, and why

| Rejected | Reason |
| --- | --- |
| An optional idempotency key | Makes the unsafe behaviour the default. The friction is small; the cost of a double charge is a customer relationship |
| Server-generated keys | A generated key is not stable across a retry, which is the whole requirement |
| Content-based deduplication | Legitimate identical operations are common. Severe false positives, and trivially evaded |
| A server-side "same request in N ms" heuristic | Undetectable in the ambiguous cases, and introduces rejections of legitimate operations |
| Sequence numbers per client | Same problem, more machinery, and worse for a client without a durable store |
| Storing the fingerprint without the response | A replayed denial would report a stale balance, and the retry would not return the original decision's state |
| Recording denials | A client could not retry a denied operation after an administrative grant |
| An unbounded idempotency window | Unbounded key growth in the fast store, which is a memory exhaustion vector |
| Relying on the Postgres unique constraint to prevent double charges | It is asynchronous and detects after the fact. It cannot prevent, and relying on it would be a correctness claim that is false |
| A distributed lock around the operation | Rejected in [atomicity research](atomicity-mechanism-options.md); the record and the mutation must be one atomic unit, and a lock makes that two things again |

## Decision impact

- [ADR-0004](../decisions/0004-idempotency-prevention-and-detection.md): a required key, an
  atomic record, a 24-hour window, per-tenant scope, and a fingerprint that excludes `request_id`.
- [ADR-0005](../decisions/0005-refund-as-first-class-operation.md): `refund` is a first-class
  operation with the same key semantics, and the runtime scope is trusted with it because the
  ceiling invariant bounds the damage (DR-019).
- `DR-026` to `DR-030` in the [domain rules](../product/domain-rules.md), including the explicit
  statement that prevention is in the data plane and detection is in the control plane.
- The S8 test and the state-identity assertion in [testing-strategy](../architecture/testing-strategy.md#t-02--idempotent-replay), where a replay answers the recorded state and is marked `replayed`.
- The S2 disclosure in [error-catalog.md](../product/error-catalog.md) and in J-7, and the
  SDK-generated-key plan as the actual fix in the [SDK release](../product/mvp-scope.md#v05--client-libraries).
- The idempotency key space in the data store's memory sizing, and the `allkeys-lru` decision
  there: an evicted idempotency record means a lost replay, which is acceptable, whereas a
  `noeviction` write rejection is a `503` on the hot path.

**Update, 2026-09-27 — the last bullet is no longer the decision, and the reason has changed.** The
trade-off written above treated an evicted idempotency record as an acceptable lost replay. It is
not: the record is the only thing preventing a second charge, so a lost replay is a double charge,
and that is a customer-visible money event rather than a degraded cache. [Q-21](../product/assumptions-and-open-questions.md#2-open-questions)
is answered by [ADR-0017](../decisions/0017-noeviction-and-duplicate-reversal.md): the store runs
`noeviction` for the whole key space, sized for the 24-hour window (DR-048), and the residual
total-store-loss case is answered by an automatic reversal of the detected duplicate (DR-049)
rather than by accepting the loss. The `noeviction` `503` on the hot path is the price, and it is now
paid deliberately and sized for.

**Update, 2026-09-30 — the replay is not byte-identical, and the record write needs no `NX`.** `IP-06`
implemented the record inside the same script execution, and two statements above were superseded.
A repeat returns the recorded seven state values with `replayed: true` and the `Idempotent-Replay`
header (DR-027, [api-conventions](../architecture/api-conventions.md) §6 and §7.1), so it differs
from the first delivery by exactly that marker; what is identical is the state, not the bytes, and
that is what [T-02](../architecture/testing-strategy.md#t-02--idempotent-replay) now asserts. The
write is `SET … EX 86400` with no `NX`, because the fingerprint check and the write are one
execution and there is nothing left to guard against ([data-model](../architecture/data-model.md)
§3.3, ADR-0002).

## Confidence and what would change this

**High confidence** on the mechanism. Atomic record-plus-mutation in one execution is the correct
answer to S8, and every alternative has a demonstrable failure.

**High confidence that S2 is a genuine server-side limit.** This is a well-understood property of
the pattern, not an implementation gap. Any design claiming otherwise should be asked what it does
when a retry arrives with a fresh key.

**The genuinely uncertain part is behavioural, not technical:** will clients actually reuse keys?
This is assumption A-03 in the [assumptions register](../product/assumptions-and-open-questions.md).
The measurement is available: the ratio of `replayed: true` responses to total consumes, which is
high-and-rising when clients are behaving correctly and near-zero when they are not. A near-zero
ratio in the pilot means the SDK work should move earlier, and that is a decision this research
makes easier to make because the metric already exists.

**Would change the design:** a reliable way to correlate retries without client cooperation. A
standardised header carrying a client-side operation identifier, adopted across the industry, would
move S2 from undetectable to detectable. If such a convention appears, this document should be
revisited, because the current requirement on clients is the weakest part of the design.

## Sources

- Stripe's published idempotency documentation, for the key-and-replay pattern, the parameter-mismatch
  behaviour, and the 24-hour key expiry convention.
- HTTP semantics for idempotent methods and the semantics of automatic client and proxy retries.
- The AWS Builders' Library guidance on retries, timeouts and backoff, and the reasoning for
  requiring an operation token for non-idempotent operations.
- Standard message-queue and payment-processor literature on at-least-once delivery and
  consumer-side deduplication, for the parallel with the ledger in
  [metering-ledger-patterns](metering-ledger-patterns.md).
- Redis/Valkey scripting semantics for `SET … EX` inside a script, for the atomic record write.
