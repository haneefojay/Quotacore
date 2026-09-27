# ADR-0008 — One cadence model: anchored calendar cycles, with rolling windows as a separate mechanism

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [cycle engine](../architecture/cycle-engine.md), [research: cycle semantics](../research/reset-cycle-semantics.md)

## Context

The motivating problem statement contains two different requirements that were later conflated:

- *"Pro users get 50,000 AI tokens per month"* — a **quota**. A total allowance, restored on a
  schedule, spent over the period.
- *"Pro users get 1,000 API calls per minute"* — a **rate limit**. A ceiling on the rate of
  consumption, with no meaningful total.

These require genuinely different data structures and different algorithms. A quota is an
integer that is decremented and periodically restored. A rate limit is a function of *recent
history*: "1,000 in the last 60 seconds" cannot be evaluated from a single number, because the
correct answer depends on how much of that number has already aged out of the window.

The project history contained the requirement in section 1, dropped it from the MVP, and then
reintroduced "metered/rolling" as a parenthetical in a user-journey example without ever
specifying what rolling means. The previous design document then listed only
`daily, weekly, monthly, yearly, never` as reset intervals, which does not express the
per-minute case at all.

## Options considered

**A — Anchored calendar cycles only.** `hourly, daily, weekly, monthly, yearly, never`. One
integer per tenant per feature, one Lua script, one code path.
- Pros: minimal, one algorithm, exhaustively testable, no unbounded collections in the data
  plane, memory is O(tenants × features).
- Cons: cannot express "1,000 per minute". A customer who needs it must implement it elsewhere,
  and the product's own problem statement promises it.

**B — Anchored cycles plus fixed windows.** Add a window period and evaluate the count within
it. Still one integer, reset on a schedule.
- Cons: a *fixed* window is a rate limit that admits a 2× burst at the boundary — 1,000 calls
  at 10:00:59 and 1,000 at 10:01:00 — which is a well-known and widely criticised weakness.
  Shipping it and calling it "1,000 per minute" would be misleading. Rejected as the
  implementation for the per-minute case.

**C — Sliding window counter, exact, via a sorted set (chosen for v0.2).** Each request adds one
  member scored by timestamp; evaluation removes members older than the window, then counts.
- Pros: exact, no boundary burst, single atomic script.
- Cons: memory is O(requests in window) and per-operation cost is O(log n) plus the pruning
  scan. Needs a hard cap on `limit` so memory is bounded.

**D — Sliding window counter, approximate, two-bucket.** Two counters for adjacent windows,
weighted proportionally by how far into the current window we are. O(1) memory, O(1) cost.
- Pros: extremely cheap, bounded memory, no collections.
- Cons: approximate. Under- or over-estimates by up to the previous window's residual. For
  revenue protection that direction matters: an over-estimate denies paying customers.

**E — Token bucket / leaky bucket.** Refill rate plus burst capacity.
- Pros: the correct primitive for infrastructure rate limiting; smooth, bounded burst.
- Cons: a different concept again — it expresses an average rate, not "this much per period".
  It is what API gateways and DDoS protection already do, and competing with them on
  infrastructure-grade limiting is not where this product's advantage is.

## Decision

**Anchored calendar cycles are the only cadence model in the MVP, and they are the only model
implemented on the data plane. Rolling/sliding windows are a distinct feature kind introduced
in v0.2 with a distinct script, key and data structure.**

- MVP feature kinds: `metered` (anchored calendar cycle) only.
- v0.2 adds feature kind `rolling`, with `plan_entitlement.rolling_window_seconds` required and
  `limit_amount` capped at 100,000 units to bound memory. Implementation is the exact sorted-set
  approach (option C) because the approximation in option D can deny paying customers, and
  denying paying customers is the failure mode this product exists to prevent.
- The two kinds never share a key, a script or a cycle rule. They are two features of the
  product, not one feature with a flag. A tenant may hold both.
- The choice between exact and approximate is recorded as an **OPEN DECISION** for the v0.2
  design spike, with the recommendation above and the requirement that any decision must state
  its error bound.

The MVP documentation will state plainly, in the feature and API references, that per-minute
rate limiting is **not** provided and points at an API gateway for that case. Telling a customer
to use the right tool is cheaper than shipping a mediocre one and being blamed for its burst
behaviour.

## Rationale

Two algorithms with different memory and accuracy profiles should not share an abstraction
pretending they are the same thing. Folding them together would have produced one code path with
two modes, which is how a quota engine ends up with a `limit_type` column and untested
interactions.

Shipping anchored cycles first is also a sequencing judgement. The hard part of this product is
atomicity and correct cycle arithmetic, not window mathematics. Rolling windows are a
well-understood, additive feature that can be built on a proven core. Building both at once
means the first release's correctness story is diluted by a second algorithm.

Declining the per-minute case for the MVP is a real positioning decision and it is the honest
one: API gateways, Envoy, Kong, Cloudflare and dedicated services already do infrastructure
rate limiting better than a quota engine should try.

## Consequences

**Positive**
- The MVP has exactly one atomicity script on the data plane, which is the thing that must be
  provably correct.
- Calendar correctness is the entire complexity of the MVP's time handling, and it is fully
  specified in [cycle-engine](../architecture/cycle-engine.md).
- Adding rolling windows in v0.2 is additive: a new feature kind, a new key, a new script, new
  tests. No change to the cycle engine.
- Customers are told the truth about what the product does not do.

**Negative**
- The product does not deliver one of the two examples in its own originating problem statement
  in the MVP. This is a marketing exposure: every piece of product copy must say "quotas", not
  "rate limits", until v0.2 ships.
- A customer with a genuine per-minute requirement must run two enforcement components until
  v0.2. Documented in the integration guidance rather than hidden.
- Two enforcement mechanisms to reason about operationally from v0.2 onward.

## Revisit when

- A customer requirement for infrastructure-grade per-second limiting dominates the roadmap, at
  which point option E is the answer and it should be delegated rather than built.
- Rolling-window memory becomes the dominant data-plane cost at scale, at which point option D
  becomes attractive provided its error bound is accepted and documented.
