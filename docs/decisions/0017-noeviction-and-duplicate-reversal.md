# ADR-0017 — No eviction inside the window, and reversal for what eviction cannot prevent

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [deployment](../architecture/deployment.md), [data model](../architecture/data-model.md), [consistency and recovery](../architecture/consistency-and-recovery.md), [observability](../architecture/observability.md), [error catalogue](../product/error-catalog.md), [domain rules](../product/domain-rules.md), [ADR-0004](0004-idempotency-prevention-and-detection.md), [ADR-0005](0005-refund-as-first-class-operation.md)
- **Closes:** [Q-21](../product/assumptions-and-open-questions.md#2-open-questions)

## Context

ADR-0004 fixed the 24-hour idempotency window and stated the cost honestly: keys accumulate, and the
memory cost is "a documented capacity-planning input in deployment.md". It did not settle what
happens when the store cannot hold them.

The deployment guide then answered that question with `maxmemory-policy allkeys-lru` and a
recommended data-store memory of 1–4 GiB. Those two numbers are incompatible with each other, and
the guide said so while declining to resolve it:

- At NFR-T1, 24 hours of keys is roughly 170 million records, about 20 GB at the guide's own
  per-record estimate.
- 20 GB does not fit in 4 GiB.

So the shipped policy would have evicted idempotency records **inside** the window, and a client
retrying inside the window would have been charged twice. The guide named the contradiction,
identified it as pitting the second claim in the README against the first, and referred it to Q-21.

Two questions had to be answered, and the second was not visible until the first was.

**Can the window be protected by policy and sizing?** Yes, but only by refusing writes rather than
discarding records, and only by provisioning for the whole working set.

**Is that sufficient?** No. The fast store is rebuilt rather than backed up (NFR-OPS4), and an
idempotency record is **not derivable from the ledger** — `usage_events` records usage with its
`balance_after`, but nothing in it records which `Idempotency-Key` produced that usage. A total store
loss inside a window, which the recovery runbook already treats as an expected and rehearsed event,
destroys the idempotency set while leaving balances perfectly reconstructible. A retry across that
boundary is charged twice, and `noeviction` does nothing about it.

Choosing `noeviction` alone would have produced a document that claimed the second claim and did not
deliver it. That is the failure this ADR exists to prevent.

## Options considered

**A — Keep `allkeys-lru`.** Cheapest, and what the guide specified.
- Cons: discards records that are the only thing preventing a second charge, at a size threshold
  the operator cannot see. The failure is a billing error with no counter. Rejected.

**B — `noeviction`, sized for the window, and accept the restart hole.** Closes the capacity hole.
- Cons: the restart hole is real, documented and rehearsed, and leaving it open means the claim is
  still not true. Insufficient on its own, and kept as the first half of the decision.

**C — Persist the store (AOF) so records survive a restart.** Makes the window durable.
- Cons: an append on every `consume`, on the request path, puts NFR-L1 (p99 under 5 ms) and NFR-T1
  (2,000/s) at risk to protect a property that reversal already restores. It also contradicts the
  store's stated role as rebuildable rather than backed up, and adds a filesystem-fsync dependency
  to a product whose headline is that it needs no external infrastructure. Rejected, and rejected on
  the merits rather than on cost: the latency claim is worth more than an eventually-consistent
  correction.

**D — Split the idempotency namespace into a second store instance** so balances stay LRU-able and
records stay `noeviction`.
- Cons: one instance has one `maxmemory-policy`, so this means a second data store, contradicting
  ADR-0001's single-binary deployment and the "one `docker compose up`" claim. A large
  re-architecture to solve a memory-sizing problem. Rejected.

**E — Shorten the window.** Makes the working set fit.
- Cons: DR-029 fixes the window as a customer-facing contract, and a customer retry horizon is a
  property of their systems, not of our memory. Rejected as a breach of an existing decision.

**F — `noeviction` sized for the window, plus automatic reversal of detected duplicates (chosen).**
- Closes the capacity hole by refusing writes, and closes the restart hole by making the residual
  double charge correctable. Neither depends on the other.

## Decision

**F. The window is protected against eviction and capacity by policy and sizing. It is not
protected against total store loss, and the resulting double charge is reversed automatically.**

**1. `noeviction`, sized for the window (DR-048).** The fast store runs
`maxmemory-policy noeviction` for the whole key space. A write that cannot fit is refused
`503 service_unavailable` (FS-21). The data-store memory floor becomes 24 GiB rather than a
1–4 GiB range.

**2. The measured figure belongs to `IP-15`, not to this document.** 20 GB is a projection from a
~120-byte-per-record estimate that ignores the engine's per-key overhead, its expires dictionary and
hash load factor; the real figure is plausibly two to three times higher. The number is replaced by
a measurement from the 24-hour projected soak. Until then the table states a floor to stay above,
because under-provisioning costs an availability incident and over-provisioning costs only money.

**3. A detected duplicate is reversed (DR-049).** When the `usage_events` partial unique index
fires, reconciliation issues exactly one refund for the second deduction, through the first-class
refund operation from ADR-0005. It is idempotent on `event_id`, audited, and bounded by one
reconciliation cycle (NFR-T10).

**4. The claim is reworded to what is true.** The second claim becomes "a retrying client is charged
once, and a double charge caused by store loss is reversed" — eventual rather than immediate, and
stated as such in [README](../../README.md), the error catalogue and the recovery runbook.

## Rationale

The choice is between two failure modes, and they are not comparable in cost. Under `allkeys-lru`
the store loses a record, a customer is charged twice, and nothing anywhere records that it
happened: the loss is silent, uncorrelated with a metric, and discovered by the customer. Under
`noeviction` the store refuses writes, every tenant sees a `503`, and one gauge says why. The first
is a financial defect that hides; the second is an outage that is obvious. Obvious is recoverable,
and hidden is not.

Persistence was rejected because it protects the window by putting a durability cost on the request
path of every single `consume`. The p99 and throughput requirements are the product's performance
story; an `fsync` per deduction spends that story to avoid a correction that the ledger can already
compute. Reversal is the cheaper mechanism because the duplicate is *already detected* by the
control plane — ADR-0004 built that detection and specified that its only job was to report. This
decision gives it a second job.

The honest cost of F is that the correction is eventual. A customer is briefly double-charged
between the retry and the reconciliation pass. That is a real degradation of the guarantee, and it
is the reason the claim is reworded rather than quietly restated. An overstated guarantee is worse
than a narrow true one, because a customer who discovers the difference stops believing the rest.

## Consequences

**Positive**
- The window survives memory pressure and capacity exhaustion. The record is never displaced.
- A restart or a store loss inside a window ends with a single net charge, automatically.
- A detected duplicate is now a remedy with a counter, not a report with a counter.
- The data-store sizing floor is stated as a floor, so under-provisioning is caught by an alert
  before it becomes an incident.
- DR-045's exposure shrinks: under `allkeys-lru` a balance hash could be lost to ordinary memory
  pressure; under `noeviction` it is lost only through total store loss, which is rarer and
  rehearsed.

**Negative**
- The data store must be provisioned at roughly 24 GiB rather than 4 GiB. This is the single largest
  operational cost in the product and it is not optional.
- A full store is an availability event for **every** tenant, not for the unlucky one. Accepted
  deliberately, monitored by `quotacore_datastore_memory_used_ratio`, alerted at 0.8.
- The duplicate correction is eventually consistent, so a customer is briefly wrong. The window
  between the retry and the reversal is bounded by one reconciliation cycle, not by zero.
- Refund is now partly automatic. An operator reading a refund in the audit log must be able to tell
  a reconciliation reversal from a support-initiated one, which the audit reason distinguishes and
  `quotacore_duplicate_reversal_total` aggregates.

**Superseded detail, not superseded reasoning**
ADR-0004's consequence that "at 25,000 requests per second sustained" the key population is large
quotes a throughput figure that does not match NFR-T1's 2,000/s. The reasoning is unaffected — the
population is bounded by TTL in time and by the throughput envelope in volume — but the figure was
wrong, and it should be read as NFR-T1 with the measured per-record cost from `IP-15`. ADR-0004 is
not edited.

## Revisit when

- `IP-15` reports a measured per-record cost, at which point the 24 GiB floor is replaced by a
  measurement and this ADR's sizing figures are historical.
- Enforcement traffic makes the idempotency population dominant enough that a per-tenant ring or an
  admission filter is preferable to full retention, as ADR-0004 anticipated.
- Store durability becomes cheap enough — a replicated store with a synchronous local write — that
  the reversal path can be dropped in favour of prevention.
