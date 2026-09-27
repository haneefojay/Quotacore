# Research: Reset and Cycle Semantics

**Date:** 2026-09-27 · **Status:** complete · **Decision:** [ADR-0003](../decisions/0003-lazy-monotonic-cycle-rollover.md), [ADR-0008](../decisions/0008-cadence-model-anchored-and-rolling.md)

## Question

What are the realistic options for resetting a metered allowance on a schedule, which failure
modes does each have, and what does a customer actually mean by "resets monthly"?

## Why it mattered

Cycle reset is where a metering system silently loses or gives away money. Every option has a
failure mode that only appears after an outage, at a month end, or across a daylight-saving
transition — that is, only in production, only rarely, and only with a customer's balance at
stake. Getting this right before writing code was worth more than any other single piece of
investigation in the project.

## Method

Worked through the failure modes of each candidate reset strategy on paper before implementing
any, and separately investigated what the phrase "resets every month" means in billing practice and
in the products customers already use.

## Findings

### Reset strategies

| Strategy | Mechanism | Correctness properties | Fails when |
| --- | --- | --- | --- |
| **Cron / scheduled job** | A worker resets every tenant whose boundary has passed | None on its own. It depends on the job running | The service is down at the boundary, so **no** tenant is reset. Then a tenant's request either fails (bad) or is served with the old balance (worse) |
| **Lazy on access** | The request that arrives after the boundary performs the reset | Self-healing: the request that needs the new cycle creates it | A tenant that is idle has a stale reported window, which is cosmetic. A race between two requests, unless the transition is atomic |
| **Lazy, unguarded** | As above, with a plain read-then-write of the cycle state | Self-healing | Two requests race and both reset, so the tenant gets two allowances, or the balance is written twice |
| **Lazy, monotonic** | As above, with the transition refusing to move to a lower cycle index and performing the reset inside the same atomic execution as the deduction | Self-healing and race-free | Nothing in normal operation. A backwards clock jump makes the tenant stuck, which is correct-but-noted and alerted |
| **Key expiry as the reset** | Set a TTL on the key; it disappears and is recreated with a full balance | Simple | **Fundamentally wrong.** Expiry is not a scheduled event: it is lazy, per-key, and unmanaged. An idle key can expire late, and the store's eviction policy can remove a key early. A key removed by `allkeys-lru` looks identical to a key that expired |
| **Client-side countdown** | The client tracks the boundary and decides | Zero server work | Trust. The client can be wrong, restarted, have its clock skewed, or lie. It also means the customer reimplements the calendar logic we were trying to centralise |
| **Fixed duration from first use** | The boundary is set at the first request plus a fixed interval | Simple | Not a calendar. "Monthly" stops meaning the 1st, and a tenant that starts on the 31st gets a 31-day month, which then drifts |

### The failure mode that decides it

**The cron failure mode is the important finding.** Consider a monthly allowance and a service
outage from 00:00 to 06:00 on the 1st. The reset job did not run.

- If requests fail closed, every customer with a boundary in that window is blocked for six hours
  for a reason that has nothing to do with their usage.
- If requests are served with the old balance, customers who exhausted last month's allowance keep
  consuming for six hours. That is revenue given away, silently, at scale, on the first of the
  month — the single most predictable bad day in the business.
- If the reset is applied lazily on the next request, the tenant lands in the new cycle, gets one
  allowance, and nobody is blocked and nobody is over-served.

The lazy approach's property is not "it saves resources". It is **the boundary is enforced by the
request that needs it, so a missed boundary is not observable.** That is the property worth
having, and it is the reason for [ADR-0003](../decisions/0003-lazy-monotonic-cycle-rollover.md).

### The race the lazy approach introduces, and how it must be closed

Lazy reset moves the race from "two jobs" to "two requests", which is worse because requests are
high-volume. Closing it requires:

1. The rollover happening **inside** the same atomic execution as the deduction. Anything less
   reintroduces the race that [atomicity-mechanism-options.md](atomicity-mechanism-options.md)
   rejected.
2. A **monotonic** guard, so a duplicated or delayed worker cannot re-grant a stale allowance. A
   plain "is it time to reset" check is not enough: a slow worker computing a window that has
   already passed would reset a second time.
3. **One implementation** used by both the request path and the worker. Two implementations of a
   boundary calculation will disagree, and the disagreement is a revenue bug.

### The drift trap

A separate and very common defect: computing each boundary from the previous boundary.

```
anchor 31 Jan → 28 Feb → 28 Mar → 28 Apr → 28 May → 28 Jun → 28 Jul …
```

The correct sequence, computed from the anchor each time:

```
anchor 31 Jan → 28 Feb → 31 Mar → 30 Apr → 31 May → 30 Jun → 31 Jul …
```

A tenant anchored on the 31st would, under the drifting implementation, never be reset on the 31st
again after February. It is invisible in testing with a short window, and permanent in production.
The fix is structural — boundaries are a pure function of `(anchor, interval, zone, n)` — and the
test table in [cycle-engine.md](../architecture/cycle-engine.md#6-test-matrix) is what catches it.

### Daylight saving

| Case | What happens | Correct behaviour |
| --- | --- | --- |
| Spring forward, boundary at local 02:00–03:00 | The wall-clock time does not exist | Resolve forward to the first valid instant. The cycle is one hour short |
| Fall back, boundary at local 01:00–02:00 | The wall-clock time happens twice | Resolve to the first occurrence. The cycle is one hour long |
| `hourly` interval | A real duration | 3600 seconds, always. Crosses the gap without shortening |
| `daily`, `weekly` | Wall-clock commitments | Local midnight stays local midnight, so the cycle is 23 or 25 hours |

The asymmetry between `hourly` and `daily` is worth stating explicitly because it looks like an
inconsistency: `hourly` is unambiguous, so a duration is exactly right, while `daily` is a promise
about local time, so a duration would break the promise. The customer's mental model is what
decides, not symmetry.

### What "monthly" means to customers

| Customer says | They usually mean | Implement as |
| --- | --- | --- |
| "Resets on the 1st" | A specific local calendar date | Anchored calendar cycle, boundary on day 1 |
| "Every 30 days" | A rolling period from the start | Anchored cycle is close enough and simpler; a rolling window in v0.2 is the exact answer |
| "Monthly, from signup" | An anniversary | Anchored cycle with the anchor at provisioning time |
| "A month of usage" | Approximately 30 days, and they do not care | Anchored cycle. The difference is invisible and the anchoring is more useful |
| "Top up monthly, never resets" | A wallet | A `never` entitlement, or a v0.2+ credit mechanism |

The finding that mattered: **customers overwhelmingly care about the reset *day*, not the
interval's exact length.** Anchored calendar cycles match that expectation, and a fixed-duration
model quietly fails it — which is a second reason, independent of the drift bug, to prefer calendar
arithmetic.

## What was rejected, and why

| Rejected | Reason |
| --- | --- |
| Cron-only reset | A missed boundary is either customer-visible downtime or silent overselling (FS-05) |
| Key expiry as the mechanism | Eviction and expiry are indistinguishable, both lazy, and neither is a scheduled event. Correctness cannot rest on a memory-management feature |
| Client-side countdown | Requires the customer to trust us enough to reimplement our logic and get it right |
| Fixed duration from first use | Not a calendar, and drifts away from the customer's stated intent |
| Boundary-from-previous-boundary | Permanent drift for any tenant anchored after the 28th |
| Storing a `current_cycle_start` field | Two stored notions of time that can disagree; the disagreement is a correctness bug that only appears at month end |
| A separate reset service | Duplicates the cycle engine, and duplication of the boundary calculation is exactly how the two implementations disagree |

## Decision impact

- [ADR-0003](../decisions/0003-lazy-monotonic-cycle-rollover.md): lazy, monotonic, atomic, with the
  worker as an optional observer.
- [ADR-0008](../decisions/0008-cadence-model-anchored-and-rolling.md): anchored calendar cycles in
  the MVP, rolling windows in v0.2, and per-minute rate limiting explicitly excluded.
- The anchor is the only stored cycle state, and boundaries are a pure function
  ([cycle-engine.md](../architecture/cycle-engine.md)).
- The test matrix exists as a direct consequence of the drift and DST findings.
- `cycle_end` is returned with an explicit offset, because a cycle's length is not 24 hours times
  its number of days once a zone is involved.

## Confidence and what would change this

**High confidence** on the anchored model, the monotonic transition, and the rejection of expiry
as a mechanism. These are not novel designs and the failure modes are documented and widely
discussed.

**Genuinely open, and worth revisiting:** whether customers who need a rolling lookback will accept
it as a v0.2 addition rather than a v0.1 requirement. There is no numbered assumption for this,
deliberately: it is a release-order risk rather than a technical unknown, and it is tracked by
[ADR-0008](../decisions/0008-cadence-model-anchored-and-rolling.md),
[ADR-0015](../decisions/0015-release-slicing.md) and question Q-11, which carries the `window`
parameter's shape forward to v0.2. If the pilot
cohort treats a rolling window as table stakes, the release order in
[ADR-0015](../decisions/0015-release-slicing.md) changes, and the release boundary moves earlier
than v0.2.

## Sources

- Calendar arithmetic in Go: `time.Date` normalisation semantics for non-existent and ambiguous
  wall-clock times, and the `time/tzdata` embedding mechanism.
- Industry practice for usage-based billing periods, in particular the prevalence of calendar-anchored
  resets and the rarity of fixed-duration billing periods.
  - Documented behaviour of Redis/Valkey key expiry and of `allkeys-lru` eviction, and the
    indistinguishability of the two from a client's perspective.
  - [atomicity-mechanism-options.md](atomicity-mechanism-options.md), for the mechanism the lazy
    transition depends on.

**Update, 2026-09-27 — the eviction premise is now stronger, so the conclusion is unchanged.** This
note argues that a reset must not rest on the store removing a key, and one of its two failure
mechanisms was `allkeys-lru`. That mechanism is no longer available: the fast store runs
`maxmemory-policy noeviction` for the whole key space ([ADR-0017](../decisions/0017-noeviction-and-duplicate-reversal.md),
DR-048). The conclusion does not weaken, because the other mechanism remains and is less
distinguishable, not more: a **total** store loss destroys a balance hash just as an eviction would,
and nothing about the key's absence tells a client which happened (DR-045, FS-21). The requirement
that the lazy transition is the only reset, computed in the application and atomic with the
deduction, is therefore unchanged and better founded than when this note was written.
