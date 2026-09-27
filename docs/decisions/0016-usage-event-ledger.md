# ADR-0016 — The usage event log is a signed-delta ledger, and it is the recovery source

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [data model](../architecture/data-model.md), [overview](../architecture/overview.md), [research: ledger patterns](../research/metering-ledger-patterns.md)

## Context

Two requirements pull in opposite directions.

The product was scoped as tracking *current balances* only, with no historical audit trail,
because a per-request log was assumed to be expensive. Meanwhile, a service holding enforcement
state in an in-memory store needs a way to recover that state if the store is lost, and needs
some record of what happened for support and for the customer-facing question "why was I
blocked?".

The previous design resolved this by having no log at all, which left three gaps: no recovery
path after datastore loss, no way to answer a support question with evidence, and no
exportable usage record for a business that sells usage-based pricing to its own customers.

## Options considered

**A — No log. Balances only, as previously scoped.**
- Pros: cheapest possible. No write amplification, no retention policy, no growth planning.
- Cons: after a Redis loss, every balance is unknown and can only be reset to the full limit,
  which hands the entire installed base a free cycle. No support investigation is possible. And
  a customer billing *their* customers on usage has no data to bill from.

**B — Double-entry ledger.** Every grant and deduction as a balanced pair of journal entries,
  materialised balance derived by summation.
- Pros: the standard for financial systems. Auditable, reconcilable, and the correct foundation
  if usage ever becomes invoiced.
- Cons: a genuine accounting subsystem. Two entries per mutation, an accounts model, a
  reconciliation process, and a derived-balance query on every enforcement request or a
  maintained cache of the derived value. That is a large amount of machinery for a component
  whose job is to be small and fast, and the previous history explicitly excluded
  invoice-grade accounting.

**C — Append-only signed-delta event log, asynchronous, with a derived balance (chosen).**
  Every state-changing operation appends one row recording the signed change, the resulting
  balance, the cycle window, and the source. The balance in the data plane is authoritative at
  runtime; the log is the durable record.

**D — Full per-request logging of reads and denials as well.** Comprehensive.
- Cons: the volume of a hot enforcement path makes this an analytics project, and most of it is
  low-value. Reads are not state changes and recording them answers no question the delta log
  does not. Rejected.

## Decision

**Option C, with the reconstruction property designed in rather than bolted on.**

- **One row per applied mutation.** Applied means: an allowed `consume`, an applied `refund`, an
  admin `grant` or `set`, a plan apply-now, and a cycle rollover. A **denied** `consume` writes
  no row; denials are counted in metrics and are not durable state changes.
- **The balance is reconstructable exactly.** Within a cycle:

  ```
  balance = cycle_opening_allowance + Σ(delta of applied mutations in that cycle)
  ```

  The cycle-opening allowance is carried on the `cycle_rollover` event. This is an invariant, it
  is asserted by a test, and it is the mechanism by which a lost data plane is repaired. The
  previous design's `FLUSHDB` recovery — "reset everyone to the full limit", which is a
  fail-open event for the entire customer base — is replaced by a recomputation.
- **Rollover is an event, not a silent write.** The reset emits a `cycle_rollover` row carrying
  the new window and opening allowance. This is what makes the per-cycle history of a tenant
  durable even though the balance in the data plane is overwritten.
- **Writes are asynchronous and off the request path.** A buffered channel drained by a
  dedicated writer. Backpressure policy is explicit: when the buffer is full, the event is
  **dropped and counted** in `quotacore_event_sink_dropped_total`, and the request still
  succeeds. Dropping an event degrades reconstruction accuracy slightly; blocking the request
  path would make a slow database into a latency incident. Reconstruction is therefore exact up
  to the drain lag, and the failure mode is an under-count of consumption — which yields a
  slightly *higher* reconstructed balance. Stated plainly: recovery is fail-open by a bounded
  margin, never fail-closed.
- **`idempotency_key` is carried on runtime events with a unique constraint on
  `(tenant_id, idempotency_key)`.** This is the detection layer of ADR-0004.
- **Not a double-entry ledger.** There is no accounts model, no requirement for balanced
  entries, and no derivation of the runtime balance from the log. The runtime balance is
  authoritative; the log is a record. This is a deliberate rejection of accounting-grade
  guarantees, and the reasoning is that Quotacore does not move money and does not produce
  invoices.
- **Retention is configurable, default 90 days.** Enforced by a background job that deletes in
  bounded batches. The table is a plain table with a BRIN index on `at` for the MVP; monthly
  declarative range partitioning is the documented upgrade path if it exceeds roughly 50 million
  rows. Partitions were rejected for the MVP because they add a migration and a
  attach/detach operational dance for a table that will not reach that size in most deployments.
- **The table is not the billing source of truth** and the documentation says so. If a customer
  needs invoice-grade usage data, they keep their own log; Quotacore's log exists for recovery,
  support and internal analytics.

## Rationale

Asynchronous logging is the right default for a component whose primary obligation is latency,
and the failure mode of the alternative — an event sink that backpressures into the request path
— would turn a slow database into a latency incident on the customer's critical path. Choosing
drop-and-count over block keeps the latency promise intact, and the resulting inaccuracy is
bounded, measured, and in the direction that does not deny a paying customer.

Designing reconstruction as a property of the schema, rather than as a recovery script written
later, means the recovery path is continuously exercised by a test and cannot rot. The previous
design's recovery story — reset everyone to full — is a revenue event on the exact day the
customer has an incident, and this change removes that failure mode entirely.

Rejecting double-entry is the main judgement call. Double-entry is the correct answer for money,
and Quotacore does not move money: it holds integers that determine whether a request proceeds.
Building an accounting subsystem would multiply the write path and the data model to buy
properties no user of this product can observe.

## Consequences

**Positive**
- A lost or flushed data plane is repaired by recomputation rather than by resetting every
  customer to a full allowance.
- Support can answer "what happened to this tenant" with evidence.
- Usage volumes are queryable for internal analytics and for customers building their own
  billing on top of Quotacore.
- Denials are excluded, so the table's growth is proportional to real state changes, not to
  traffic.
- The uniqueness constraint on the idempotency key gives a real duplicate-detection capability
  that would otherwise not exist.

**Negative**
- Every applied mutation costs one asynchronous database insert, which is real write
  amplification against the control plane. At 25,000 requests per second sustained this is the
  dominant control-plane cost and must be sized for in [deployment](../architecture/deployment.md).
- Reconstruction is exact only up to the drain lag. A dropped event yields a slightly high
  reconstructed balance. Bounded, measured, and fail-open.
- The log is not reconcilable in the accounting sense, so it cannot serve as an audit artefact
  for financial reporting. Documented, and correct for the product's scope.
- Retention deletes history. A customer who needs multi-year usage history must export it, and
  the API for that should exist before a customer asks for it. Noted as a gap for post-MVP.

## Revisit when

- Usage ever becomes billable to the *end* customer by Quotacore itself, which would require
  double-entry and a reconciliation process.
- Write amplification against the control plane becomes the binding constraint, at which point
  batching, sampling of denials-in-context, or a pluggable sink becomes necessary.
