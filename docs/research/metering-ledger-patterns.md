# Research: Metered Ledger and Event Patterns

**Date:** 2026-09-27 · **Status:** complete · **Decision:** [ADR-0016](../decisions/0016-usage-event-ledger.md)

## Question

What is the right shape for a record of balance mutations — an append-only event ledger, a
materialised balance plus history, or a full event-sourcing model — given that the balance must
also be readable and writable at request rate?

## Why it mattered

The ledger serves three purposes that pull in different directions: reconstructing a balance after
data loss, explaining a disputed charge to a customer, and feeding the customer's own reporting.
Choosing the wrong shape means one of those three is permanently bad, and the choice is expensive
to reverse because it becomes a documented interface the customer writes queries against
([data-model.md](../architecture/data-model.md#6-migration-policy)).

## Method

Enumerated the plausible shapes, evaluated each against the three purposes plus the constraint that
the hot path cannot touch the durable store, then examined how the products in
[competitive-landscape.md](competitive-landscape.md) handle the same problem, since they have all had
to solve it with real money on the line.

## Findings

### The candidate shapes

| Shape | Hot-path cost | Recovery | Dispute explanation | Verdict |
| --- | --- | --- | --- | --- |
| **Counter column, updated in place** | One `UPDATE` | Nothing to recover | No history. A customer disputing a charge cannot be helped | **Insufficient.** This is what customers have today and it is why they are looking for a product |
| **Event-sourced: events only, balance computed on read** | Aggregate every event on every request | Trivially correct | Perfect | **Rejected on latency.** An aggregate over even fifty events is tens of milliseconds and grows without bound |
| **Event-sourced with a cached projection** | Fast reads, but the cache is the balance again | Trivially correct | Perfect | This is what Quotacore has, described differently: the fast store *is* the projection, and the ledger is the source of truth behind it. The distinction is that the projection is not derived on read |
| **Materialised balance plus an event stream** | Fast | Replayable | Good | **Chosen** |
| **Write-through: durable write on every mutation** | One durable write per request | Nothing to recover | Perfect | **Rejected on latency and on the two-plane split.** This is the option that makes Postgres or another durable store part of the hot path |
| **Periodic snapshots plus an event log** | Fast | Fast replay for recent history | Good | **Considered for the customer's export path.** Rejected as unnecessary at the volumes involved, since a full replay is seconds |

### The three requirements, stated as constraints

1. **The hot path must not wait for a durable write.** A metered action that has been applied
   cannot be un-done by failing the response, because the client will retry and be charged twice.
   So the durable write cannot be on the response path. This constraint alone rules out write-through
   and rules out event-sourced-with-computed-reads.
2. **A balance must be reconstructible from durable state.** Otherwise a data-store loss is a
   revenue event rather than an inconvenience.
3. **A disputed charge must be explainable.** Which means a per-mutation record with enough context
   to reconstruct the sequence, not just a running total.

Constraints 1 and 2 together fix the shape: a **materialised balance in the fast store, with an
append-only event stream in the durable store, written asynchronously.**

### The consequence that had to be faced head-on

**If the event write is asynchronous, the event stream is not guaranteed complete.** That is not a
detail to be softened in the documentation; it is the direct consequence of constraint 1.

Three options were considered for handling a failed event write:

| Option | Behaviour | Verdict |
| --- | --- | --- |
| Block the response until the event is written | Durable history, but the hot path now waits on a database, and a slow database becomes a latency incident and then a queue-draining incident | **Rejected.** Reintroduces the entire problem [ADR-0001](../decisions/0001-control-plane-data-plane-split.md) solved |
| Write the event to the fast store first, flush to the durable store asynchronously | No history loss unless the fast store is also lost | **Rejected.** It adds a second write on the hot path for a class of loss (both stores down simultaneously) that is far less likely than a slow database. Two writes to protect against a rare correlated failure is a bad trade |
| Accept the gap, make it visible and countable | Enforcement is unaffected; the gap is a metric and a visible discontinuity in history | **Chosen** |

The chosen option's cost is real and is stated in the product documentation rather than buried: a
customer relying on the event history for a period with drops has incomplete history, and the
correct response is to say so. The alternative — a silent gap — is far worse, because a customer who
is right and cannot be shown to be wrong is a different and much more expensive problem.

**The decisive framing:** the guarantee is stated as *at-least-once enforcement, best-effort
history*. That sentence is the whole design, and it is preferable to the alternative framing —
*durable history, at-risk enforcement* — because the failure of the second is silent revenue loss
and the failure of the first is a countable gap.

### The event shape

Each event must be sufficient to answer "what happened and why" without a second query, and
sufficient to reconstruct a cycle.

| Field | Purpose |
| --- | --- |
| `tenant_id`, `feature_id` | The subject |
| `cycle_index` | Which cycle this belongs to, so a per-cycle sum is a filter rather than a comparison against timestamps |
| `delta` | The signed change. Non-zero always, enforced by a constraint |
| `balance_after` | So history is readable without replay, and so the reconciliation identity can be checked directly |
| `source` | `api`, `admin`, `system`. Answers "who or what moved this" |
| `event_type` | `consumed`, `refunded`, `granted`, `set`, `plan_changed`, `cycle_rolled_over`, `force_rolled_over`. Distinguishes an API deduction from an administrative one from a rollover, which is the question a support conversation actually asks |
| `request_id` | The join key to the request logs, which is how a customer's report becomes an answer in one query (J-5) |
| `idempotency_key` | How a disputed charge is resolved: look up the key, get the recorded outcome (UC-16) |
| `metadata` | The customer's correlation data. Bounded, unindexed, and never logged |
| `created_at` | Ordering and retention |

**`balance_after` is the field whose value is not obvious.** Storing it denormalises, and
denormalisation is normally to be avoided. It is stored anyway because two things depend on it: a
customer reading history should not have to replay to see a balance, and the reconciliation
identity in [consistency-and-recovery.md](../architecture/consistency-and-recovery.md#5-reconciliation)
is only checkable if each row states the balance it produced.

### The reconciliation identity

```
within a cycle:  balance = cycle_opening_allowance + Σ(delta)
```

This is the property that makes the ledger useful, and it has one failure mode worth stating: the
*opening* allowance of a cycle is not itself an event. A cycle's opening allowance is implied by
the plan and the override at that moment, so a tenant that received a mid-cycle `set`, a `grant` or
a `plan_changed` may not reconcile from events alone.

The handling is to emit an event for those operations too, so that the opening allowance is always
recoverable from the previous cycle's closing balance plus the recorded deltas. The remaining
residual case is a cycle whose opening allowance was set by a plan change that itself followed a
loss of history, and that case is flagged by the verification tool rather than guessed at. The
rebuild procedure marks tenants it cannot reconstruct as unreconcilable rather than producing a
plausible number.

### How the products in this space do it

Observed patterns across the competitors in [competitive-landscape.md](competitive-landscape.md):

| Pattern | Who does it | Note |
| --- | --- | --- |
| Durable-first event write, balance derived or cached | Most usage-based billing platforms | Their hot path is a billing request, not an application request, so the trade is different. They can afford a durable write on the critical path in a way this product cannot |
| Stream to a queue, aggregate asynchronously, with at-least-once and dedup on the consumer | The mature metering pipelines | Well understood, and it has the same best-effort property, handled with an explicit dedup key rather than an assumption |
| A separate metering service with its own storage tier | Several | More infrastructure than this product will accept |
| Ingest-then-enforce, where the allowance check is advisory and the bill is authoritative | One notable pattern | This inverts the priority: correct billing, approximate enforcement. Wrong for this product, whose entire value is that enforcement is exact |

That last row is the strategic observation worth carrying forward. **The market's default is to
optimise for accurate billing and treat enforcement as approximate.** This product optimises for
exact enforcement and treats billing as the customer's problem, which is why the architecture is
simpler than most of the field's and why the hot path is fast. It is also why the product cannot
compete on billing features, and should not try.

## What was rejected, and why

| Rejected | Reason |
| --- | --- |
| Pure event sourcing with on-read aggregation | Unbounded latency growth on the hot path |
| Write-through to a durable store on every mutation | The response path waits on a database; a slow database becomes an outage |
| An in-place counter with no history | Cannot answer a dispute, which is the moment the product is evaluated |
| A second fast-store write as a write-ahead buffer | Two writes per request to protect against a rare correlated failure |
| Blocking or failing the request when the event write fails | A double charge, which is worse than a gap |
| Snapshot plus event log for the customer's export | Complexity with no benefit at these volumes; a full replay is seconds |
| An event stream in the fast store as the primary record | A stream is unbounded memory, and losing it is losing the history it was meant to protect |

## Decision impact

- [ADR-0016](../decisions/0016-usage-event-ledger.md): append-only signed-delta events,
  asynchronously written, best-effort, with a visible drop counter.
- The `usage_events` schema, including the `delta <> 0` constraint, the partial unique index on
  the idempotency key, and the deliberate absence of any `UPDATE` or `DELETE` path
  ([data-model.md](../architecture/data-model.md#27-usage-events-the-ledger)).
- The `event_sink_dropped_total` metric, the P2 alert on it, and the recovery guidance that
  **fabricating events to fill a gap is forbidden** — a synthetic event is indistinguishable from
  a real one in every future query.
- The four-way reconciliation check, and the P1 severity of a mismatch.
- The retention defaults: 90 days for events, 400 for audit, with the asymmetry explained rather
  than left as a number.
- The honest statement in [error-catalog.md](../product/error-catalog.md) (FS-10) that a ledger gap
  is possible and what a customer should do about it.

## Confidence and what would change this

**High confidence** on the shape. It is the standard answer to "durable history plus a fast
balance", and the three constraints that produce it are not negotiable for this product.

**Genuinely uncertain, and worth revisiting:** whether best-effort history is acceptable to every
customer. Assumption A-16 in the [assumptions register](../product/assumptions-and-open-questions.md)
tracks this and proposes asking operators directly during the pilot while showing them the metric.
The answer shapes the product's trust story more than any other single design decision, and it
deserves a real conversation rather than a documentation paragraph.

**Would change the decision:** a requirement for audit-grade completeness — a regulated customer
who needs every balance mutation in a durable, tamper-evident record. That customer needs
write-through on the hot path, or a separate synchronous ingestion path, and it is a legitimate
requirement that would change the latency story. It is better to discover that in a design review
than to discover it as a bug report.

## Sources

- Event sourcing and CQRS literature, for the materialised-projection pattern and its trade-offs.
- Stripe's published idempotency guidance, for the request-key-plus-replay pattern and the
  fingerprint-mismatch case.
- The metering and usage-billing pipelines described by the products in
  [competitive-landscape.md](competitive-landscape.md), for at-least-once delivery with
  consumer-side deduplication.
- Redis/Valkey stream documentation, for the "events in the fast store" option that was rejected.
- `outbox` and transactional-outbox patterns, for the write-through option that was rejected.
