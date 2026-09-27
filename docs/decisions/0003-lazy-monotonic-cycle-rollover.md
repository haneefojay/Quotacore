# ADR-0003 — Cycle rollover is lazy, monotonic, and computed in the application

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [cycle engine](../architecture/cycle-engine.md), [request lifecycle](../architecture/request-lifecycle.md)

## Context

A tenant on a monthly allowance must receive a full allowance at the start of each cycle. The
balance is mutated in two places: the request path, which decrements it, and cycle
boundaries, which restore it. Those two places race. Whoever wins must be correct, or the
tenant either loses a cycle's worth of quota or receives two cycles' worth.

Two mechanisms have been proposed for cycle boundaries, and the previous project history
proposed both simultaneously, which is the source of a contradiction in the earlier design.

**Mechanism 1 — the background worker writes the reset.** A periodic job queries the control
plane for tenants whose cycle has ended and overwrites their balance in Redis.
**Mechanism 2 — expiry is the trigger.** Balance keys carry a TTL equal to the cycle end, and
the first request after expiry finds the key missing and rebuilds it.

Mechanism 1 alone is not sufficient: if the worker is down, restarted, or paused at the exact
boundary, tenants are denied quota they have paid for. Mechanism 2 alone is not sufficient
either: expiring the key destroys the balance before anything can record how much was consumed,
and a key that expires is indistinguishable from a key that was never created, from a key that
was evicted under memory pressure, or from a key that was flushed.

## Options considered

**A — Worker writes resets, TTL as failsafe (the previous design).**
- Correct only if the worker is healthy. The two writers race: a request arriving at the
  boundary can read a balance the worker is in the middle of replacing.
- Expiry-as-failsafe conflates "cycle ended" with "state was lost", and the repair path for the
  two cases is different.

**B — Request-path-only lazy evaluation (no worker at all).**
- The request path detects an elapsed cycle and restores the balance.
- Pros: single writer, no race, correctness independent of any background job. This is the
  property we want.
- Cons: nobody ever learns that a tenant's cycle rolled over unless a request arrives. Cycle
  rollovers, per-cycle usage rollups, and near-exhaustion signals become invisible. A tenant who
  is idle through a boundary produces no event at all.

**C — Lazy evaluation on the request path plus a worker that performs the identical
transition (chosen).** Both paths call the same transition function, which is idempotent and
monotonic.

## Decision

**Option C, with the rule that the request path is authoritative and the worker is an
observer.**

1. **The application layer is the only thing that computes cycle windows.** `cycleWindow(anchor,
   interval, timezone, now)` is a pure function in `internal/domain/cycle`. The data plane is
   never asked "when is the next boundary"; it is *told* the current window as two arguments.

2. **A single Lua transition, strictly monotonic.** The reset script applies a new window only
   when the stored cycle end is less than or equal to the supplied one. It can therefore only
   ever move a balance forward in time. Called twice with the same target, it does nothing the
   second time. Called by two processes concurrently, exactly one of them performs the
   transition and the other observes that it already happened.

   ```
   if stored.ce is not 0 and tonumber(ARGV.target_ce) >= tonumber(stored.ce) and now >= stored.ce then
       -- perform the rollover, set bal = lim, cs = ARGV.target_cs, ce = ARGV.target_ce
   end
   ```

3. **A key's absence is never the trigger.** Missing key, evicted key, flushed key and expired
   key are all handled by the same path: reconstruct the current window from the snapshot and
   initialise the balance. No distinction is attempted, because none of them is safely
   distinguishable and all of them have the same correct repair.

4. **TTL is garbage collection only.** Balance keys carry an expiry of *cycle end plus a
   24-hour grace period*, not cycle end. A key therefore usually still exists when the boundary
   passes, and the monotonic comparison in step 2 is what performs the reset. A tenant idle
   across three boundaries is jumped directly to the current window, not reset three times.

5. **The worker exists to produce events, not to maintain state.** It scans for tenants whose
   next boundary is within a short horizon, applies the same transition, and — when the
   transition actually fired — appends the cycle-opening `usage_event`. Losing the worker delays
   reporting; it does not delay or deny quota.

## Rationale

The requirement is a guarantee about *access*, not about bookkeeping. Only a mechanism whose
correctness does not depend on a background process can provide that guarantee. Making the
request path authoritative and the worker an observer gives the guarantee for free, and then
preserves the reporting capability the worker was wanted for.

Deriving the window in the application rather than inside the script is the other half of the
fix. If the script computed boundaries itself it would need the tenant's anchor and timezone
passed in on every call anyway, and it would need a second copy of the calendar logic in a
language with no time-zone database. Keeping the calendar arithmetic in Go, where `time/tzdata`
and the standard library's location handling are available and unit-testable, is what makes
leap years, month-end clamping and daylight-saving transitions tractable.

The monotonicity rule is what makes the two writers safe. Two callers computing the same
window converge on the same state; two callers computing different windows converge on the
later one. There is no interleaving that produces a state neither caller intended.

## Consequences

**Positive**
- No tenant can be denied a fresh allowance because a background process was down. This is the
  single most damaging failure mode for a quota product and it is now structurally impossible.
- Worker and request path cannot corrupt each other's writes; the race is resolved by
  monotonicity rather than by a lock.
- One implementation of rollover, exercised by both paths, so the tested behaviour and the
  production behaviour cannot diverge.
- Cycle arithmetic is a pure function and is unit-testable against a large date matrix without
  a datastore.

**Negative**
- Cycle computation runs on the request path. It is pure, allocation-light and bounded, and the
  performance budget in [performance-budget.md](../architecture/performance.md) allocates
  it a share, but it is work that the alternative design would not do inline.
- A tenant that is completely idle produces no rollover event until it returns, so a
  never-returning tenant's cycle history is not archived eagerly. Acceptable: the balance
  carries no information that the opening event of the next cycle cannot reconstruct.
- `never` allowances have no boundary, so they never roll over and their keys must not be given
  a cycle-end TTL. The script therefore has to treat "no end" as a distinct sentinel, which is
  one extra branch and one extra test case.

## Revisit when

- Cycle windows become configurable per-tenant in ways that make the pure function unable to
  describe them (for example, holidays, or a business calendar).
- A tenant base large enough that the worker's scan becomes the dominant control-plane cost
  requires a different scheduling index; the current design assumes the `next_cycle_at` index
  is selective.
