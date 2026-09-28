# Cycle Engine

How a wall-clock boundary is computed from three stored values, and why the computation is a pure
function with no I/O and no clock read. This is the most subtle component in the system, because
it is the one place where an innocuous-looking implementation choice produces a silent, systematic
revenue error.

## 1. The problem

A "monthly" allowance has to mean something unambiguous. The two obvious implementations are both
wrong:

```go
// WRONG — drifts, and breaks month ends
next = time.Date(y, m+1, 1, 0,0,0,0, loc)  // anchored to the previous boundary
```

```go
// WRONG — a fixed duration, which is not a calendar month
next = previous.Add(30 * 24 * time.Hour)
```

The first drifts because each boundary is derived from the last, so an error in one month
propagates forever: a tenant anchored on 31 January would have starts on 31 Jan, 28 Feb, 28 Mar,
28 Apr, 28 May… and would never return to the 31st. The second is simply a different product, and
it makes "resets on the 1st" impossible.

## 2. The rule

```
window(n) = [ boundary(anchor, interval, zone, n), boundary(anchor, interval, zone, n+1) )
current(n) = max { n : boundary(n) <= now }
boundary(a, i, z, 0) = a
boundary(a, i, z, n+1) = add(a, i, z, n)          // the n-th boundary, always from the anchor
```

Three consequences follow, and they are the whole design:

1. **Every boundary is derived from the anchor**, never from the previous boundary. No drift, ever.
2. **The current cycle is a pure function of the anchor and the clock.** The cycle index is
   derived, not stored.
3. **A missed boundary cannot be observed.** Because the function is a function of `n`, jumping
   from `n` to `n+3` produces the same answer as three successive advances (DR-009).

Only the **anchor** and the **interval** are stored, and both live in the control plane. There is no
stored "current cycle start" precisely because two stored notions of time can disagree, and the
disagreement would be a correctness bug that only appears at month end.

The runtime balance hash *does* carry `cycle_index`, `window_start` and `window_end`, and that is
not a second source of truth. Those three fields are a memo of the last transition the script
performed, and they exist for two reasons only: the monotonic guard, which must reject a stale
writer (C-3), and the response, which must report a window without recomputing it in every caller.
The authoritative value is always `boundary(anchor, interval, n)`, so a wrong memo degrades to a
detectable inconsistency rather than a wrong boundary — and the reconciliation check in the testing
strategy asserts that the memo equals the recomputed value.

## 3. Boundary computation by interval

All arithmetic is done in the tenant's zone on wall-clock values, then converted to an instant.
The host clock itself is used only for `now`, in UTC, and never for calendar arithmetic
(ADR-0007).

```go
// add returns the n-th boundary after the anchor.
// n is a non-negative integer; n == 0 is the anchor itself.
func add(anchor time.Time, iv Interval, loc *time.Location, n int64) time.Time
```

| Interval | Rule | Edge behaviour |
| --- | --- | --- |
| `hourly` | `anchor.Add(n * time.Hour)` | Exact, 3600 s always. The one interval that is a duration, and it is unambiguous |
| `daily` | Same wall clock, `n` days later | DST: 23 or 25 hours. Local midnight stays local midnight |
| `weekly` | Same wall clock, `7n` days later | DST: 167 or 169 hours, by design (DR-006) |
| `monthly` | Day clamped to the month's last day | 31 Jan → 28/29 Feb → 31 Mar (DR-004) |
| `yearly` | Same month, day clamped; 29 Feb → 28 Feb in non-leap years (DR-005) | Anchor stays 29 Feb |
| `never` | No boundary exists | `cycle_end` is null, no TTL, no rollover possible (DR-008) |

### 3.1 Clamping, precisely

```go
func addMonths(t time.Time, n int64, loc *time.Location) time.Time {
    y, m, d := t.Date()
    total := int64(m) - 1 + n
    yy := y + int64(total/12)
    mm := total % 12
    if mm < 0 { mm += 12; yy-- }
    month := time.Month(mm + 1)
    last := daysInMonth(yy, month)
    if d > last { d = last }                 // the clamp
    return time.Date(yy, month, d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
}
```

The clamp is applied **when computing each boundary, from the anchor's day-of-month**, not from
the previous boundary's day. This is the entire fix for the drift bug:

```
anchor = 31 Jan, Berlin
n=0 → 31 Jan 00:00
n=1 → 28 Feb 00:00   (2026, not a leap year)
n=2 → 31 Mar 00:00   ← day restored from the anchor, not carried from 28
n=3 → 30 Apr 00:00
n=4 → 31 May 00:00
```

An implementation that carries the previous day forward produces `28 Mar`, then `28 Apr`, and
never recovers. The test table in section 6 exists to make that class of bug impossible to merge.

### 3.2 Daylight saving

A wall-clock time can be **non-existent** (spring forward) or **ambiguous** (fall back). Go's
`time.Date` normalises both, and the normalisation is exactly the required behaviour:

| Situation | Local time requested | Go resolves to | Effect |
| --- | --- | --- | --- |
| Gap, spring forward | 02:30 on the transition day | 03:30 local | The cycle is one hour short (DR-007) |
| Repeat, fall back | 02:30 on the transition day | 02:30, first occurrence | The cycle is one hour long (DR-007) |
| Normal | any | unchanged | — |

No special code is required, which is the desired outcome: a special case is a special case that
can be wrong. The **behaviour** is asserted by tests, so the normalisation is a documented
decision rather than an accident of the standard library (FS-14, J-9).

Note the deliberate asymmetry with `hourly`, which is a real duration and crosses a DST gap
without shortening. `hourly` is unambiguous, so it stays a duration; the calendar intervals are
wall-clock commitments, so they stay wall-clock (DR-006).

### 3.3 Finding the current cycle index

```go
func currentIndex(anchor time.Time, iv Interval, loc *time.Location, now time.Time) int64 {
    if iv == Never { return 0 }
    // Exponential doubling, then binary search. Never a linear scan:
    // a yearly tenant idle for a decade would otherwise be 10 iterations
    // per request, and an hourly tenant idle for a year would be 8,760.
    hi := int64(1)
    for !add(anchor, iv, loc, hi).After(now) {
        hi *= 2
    }
    lo := int64(0)
    for lo < hi {
        mid := lo + (hi-lo)/2
        if add(anchor, iv, loc, mid+1).After(now) {
            hi = mid
        } else {
            lo = mid + 1
        }
    }
    return lo
}
```

Cost is `O(log n)` in the number of elapsed intervals, with a small constant. For a monthly
tenant that is under six iterations. The `add` call is cheap — no allocation, no database, no
zone database load once the `*time.Location` is resolved.

**Performance note.** `time.Date` with a resolved `*time.Location` does not read the zone database;
the location is resolved once per tenant and cached. Resolving a location per request would be a
measurable regression and is explicitly avoided.

## 4. Where the engine runs

The engine is a pure function library with **no I/O, no clock reads and no state**. It is called
from two places, and from nowhere else:

1. **Inside the Lua script**, which receives the computed boundary as an argument. The script
   decides whether the current time has passed it, using the value the application computed.
2. **In the control plane**, when rendering a plan's or tenant's cycle window for a human, and when
   computing the `apply-now` and impact-preview results.

The script does not compute calendar arithmetic. Lua has no time-zone database, and a
time-zone-aware boundary computed in Lua would either need the offsets passed in for every future
cycle or approximate. Instead the application computes the current target window and passes
`target_index`, `window_start` and `window_end` in, and the script performs the **transition**:
a comparison, a reset, and a monotonicity check. That split is deliberate and it is what keeps
the script small enough to meet NFR-L2.

```
  Go:  currentIndex(anchor, interval, zone, now) → (n, start, end)
  ARGV: cycle_index, window_start, window_end, limit, bonus, amount, now_ms
  LUA: if now >= window_end: if n < cycle_index → REJECT
                            else → cycle_index = n; balance = limit; bonus = 0
       if balance < amount → DENY
       else balance -= amount
```

## 5. The transition

The full transition, as implemented in the atomic script. This is ADR-0003 in code form.

```
CYCLE_TRANSITION(cycle_index, target_index, window_start, window_end, limit, bonus, now):

  1  if now < window_start of the current cycle:        # a stale writer
         return STALE                                   # no change, not an error for the caller

  2  if target_index < cycle_index:                     # out-of-order worker
         return STALE                                   # DR-004 / C-3: refuse, never re-grant

  3  if target_index == cycle_index:                    # no boundary crossed
         return CURRENT                                 # the common path, one comparison

  4  cycle_index  = target_index
     window_start = window_start
     window_end   = window_end
     balance      = limit                               # fresh allowance, no carry-over
     bonus        = 0                                    # DR-020
     EXPIREAT     = window_end + 24h
     emit cycle_rolled_over
     return ROLLED
```

Four properties, each of which corresponds to a failure that a naive implementation has:

| Property | Prevents |
| --- | --- |
| Step 2 refuses to move backwards | A duplicated or delayed worker re-granting a stale allowance. The single most dangerous bug in a lazy-rollover design |
| Step 3 is a single comparison | The hot path does not reset on every request, which is the whole point of lazy rollover |
| Step 4 computes from the passed target | No accumulation of floating-point or rounding error, and identical results in the request path and the worker |
| `EXPIREAT` is `window_end + 24h` | A key expiring before a rollover could need it. A `DUE` expiry is a correctness bug; a late expiry is only a dangling key (INV-C6) |

**The worker and the request path call the same function.** There is no second implementation. If
the two ever diverged, a race would produce either a duplicate allowance or a lost deduction, and
the divergence would be invisible in testing. This is the strongest structural guarantee in the
system and it costs nothing.

## 6. Test matrix

Every row is an assertion in the test suite, not a comment. Interval boundaries are asserted as
exact RFC 3339 timestamps with offsets.

| # | Zone | Interval | Anchor | `now` | Expected `cycle_start` | Expected `cycle_end` | Rule |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | UTC | hourly | 00:00 | 03:30 | 03:00 | 04:00 | DR-003 |
| 2 | UTC | daily | 2026-01-01 00:00 | 2026-01-05 12:00 | 2026-01-05 | 2026-01-06 | DR-003 |
| 3 | UTC | daily | 2026-03-28 00:00 | 2026-03-30 12:00 | 2026-03-30 | 2026-03-31 | DR-003 |
| 4 | Europe/Berlin | daily | 2026-03-01 00:00 | 2026-03-30 12:00 | 2026-03-30 00:00 +02:00 | 2026-03-31 00:00 +02:00 | DR-007 |
| 5 | Europe/Berlin | weekly | 2026-01-05 09:00 | 2026-04-01 | 2026-03-30 09:00 +02:00 | 2026-04-06 09:00 +02:00 | DR-006 |
| 6 | UTC | monthly | 2026-01-31 00:00 | 2026-04-10 | 2026-03-31 | 2026-04-30 | DR-004 |
| 7 | UTC | monthly | 2026-01-31 00:00 | 2026-03-01 | 2026-02-28 | 2026-03-31 | DR-004 |
| 8 | UTC | yearly | 2026-01-01 | 2028-06-01 | 2028-01-01 | 2029-01-01 | DR-005 |
| 9 | UTC | yearly | 2024-02-29 00:00 | 2025-06-01 | 2025-02-28 | 2026-02-28 | DR-005, degraded |
| 10 | UTC | yearly | 2024-02-29 00:00 | 2028-06-01 | 2028-02-29 | 2029-02-28 | DR-005, restored on a leap year |
| 11 | America/New_York | daily | 2026-03-07 00:00 | 2026-03-09 12:00 | 2026-03-09 00:00 −04:00 | 2026-03-10 00:00 −04:00 | DR-007 |
| 12 | Europe/Berlin | daily | 2026-10-24 00:00 | 2026-10-26 12:00 | 2026-10-26 00:00 +01:00 | 2026-10-27 00:00 +01:00 | DR-007 |
| 13 | UTC | never | 2026-01-01 | 2030-01-01 | 2026-01-01 | `null` | DR-008 |
| 14 | UTC | monthly | 2026-01-01 00:00 | 2026-05-01 00:00 | 2026-05-01 00:00 | 2026-06-01 00:00 | DR-009, exact-boundary |
| 15 | UTC | monthly | 2026-01-01 00:00 | 2026-05-01 00:00 − 1 ms | 2026-04-01 00:00 | 2026-05-01 00:00 | DR-003, just-before |
| 16 | UTC | monthly | 2026-01-01 00:00 | 2026-04-15 | 2026-04-01 | 2026-05-01 | INV-C1, target ≥ stored |
| 17 | UTC | monthly | 2026-01-01 00:00 | 2026-02-15 | 2026-02-01 | 2026-03-01 | C-3, stored is ahead: refused |
| 18 | Asia/Kolkata | daily | 2026-01-01 00:00 | 2026-01-02 12:00 | 2026-01-02 00:00 +05:30 | 2026-01-03 00:00 +05:30 | DR-003, half-hour offset |
| 19 | UTC | hourly | 2026-01-01 00:00 | 2026-01-01 02:30 | 2026-01-01 02:00 | 2026-01-01 03:00 | DR-001, host clock only |
| 20 | UTC | monthly | 2026-01-15 14:30 | 2026-01-15 15:00 | 2026-01-15 14:30 | 2026-02-15 14:30 | DR-033, zone change, no re-anchor |

Row 20 is the one that catches an over-eager re-anchor on a time-zone change: the cycle must not
restart when the zone changes (DR-033).

Rows 9 and 10 are a pair, and they are the pair that separates a correct implementation from a
plausible one. Both boundaries are computed from the anchor, so a leap-day anchor yields
2025-02-28 **and then 2028-02-29**. An implementation that adds a year to the previous boundary
instead produces 2025-02-28, 2026-02-28, 2027-02-28, 2028-02-28 — permanently one day early, with
no error, no exception and no failed test anywhere else in the matrix. Row 10 is the assertion that
finds it, which is why it is in the table rather than left to review.

**Correction, 2026-09-28, by `IP-03`.** Rows 4–7 originally carried four date-math errors that
neither the checker nor the rest of the matrix could detect: a Berlin daily boundary at `2026-03-30
00:00` was written `+01:00` though EU summer time began 2026-03-29 (DR-007, [ADR-0007](../decisions/0007-per-tenant-timezone.md));
a weekly boundary was written on a Sunday, two full days before the anchor's weekday; and the two
monthly rows with a 2026-01-31 anchor were written as if the anchor were the 1st, contradicting
DR-004's clamping example. The corrected cells above were each verified against Go's own tzdata,
which is the same calendar authority the binary embeds, and against the web-sourced transition dates
stated in [ADR-0007](../decisions/0007-per-tenant-timezone.md). The values asserted in the test
fixture (T-07) are the corrected ones.

## 7. Interaction with overrides and plan changes

| Event | Effect on the cycle | Rule |
| --- | --- | --- |
| Plan edit | None until the next boundary | DR-014 |
| `apply-now` | Re-anchor to now, apply the new limit immediately, discard the balance | DR-014, UC-04 |
| Plan assignment change | Re-anchor to now, new limit, discard the balance | DR-013 |
| Override set, `immediate` | Limit changes now; window unchanged; the new limit binds at the next boundary | DR-011 |
| Override set, `next_boundary` | No effect until the next boundary | DR-014 |
| Force rollover | Advance to the current window by the same transition | UC-12 |
| Time-zone change | Future boundaries recomputed; window unchanged | DR-033 |
| Interval change on an override | Applies from the next boundary, because changing the interval changes every future boundary and therefore requires a new anchor | DR-011 |

The last row is a genuine consequence of the pure-function design and worth stating plainly: an
override that changes the **interval** cannot be applied to a running cycle without invalidating
the anchor relationship, so it takes effect at the next boundary or requires an explicit apply.
Changing only the **limit** has no such constraint.

## 8. Failure modes specific to this component

| Failure | Behaviour | Prevention |
| --- | --- | --- |
| Host clock jumps backwards | The computed target index is lower than the stored one; the transition returns `STALE` and nothing changes. The tenant is stuck in a longer cycle until the clock catches up | NTP is a hard requirement (NFR-D8, DR-001). A clock-skew alert is the mitigation we can provide |
| Host clock jumps forwards | The tenant lands in a later window and loses the intervening allowance. This is the same behaviour as an outage, and it is correct: time passed | Alert on skew so it is noticed |
| Zone database updated in the container | Impossible by construction: `time/tzdata` is embedded and the host database is never consulted (NFR-OPS8) | — |
| Unrecognised time zone in a stored tenant | Cannot be written: validated on input (DR-033), and the column default is UTC. A zone that is later removed from the IANA database still resolves from the embedded copy | — |
| `cycle_index` far in the future from a bad write | Every subsequent transition returns `STALE` and the tenant is stuck | The monotonic guard also has a configured sanity ceiling; a stored index beyond it is a data-integrity alert, and the recovery is `force-rollover` with an audit record |
| Very old anchor with `hourly` | `currentIndex` is logarithmic, not linear. A five-year-old hourly tenant needs about 22 iterations | Bounded by the exponential-then-binary search, asserted by a benchmark |

## 9. Review checklist for any change to this component

1. Does the new code still derive every boundary from the anchor, rather than from a previous
   boundary?
2. Does it avoid reading the clock more than once per request?
3. Is the time zone resolved once and cached, rather than per call?
4. Is the arithmetic performed on wall-clock values in the tenant's zone?
5. Does the monotonic guard still refuse a backwards transition?
6. Is a new row needed in the test matrix, and does it assert an exact RFC 3339 value with an
   offset?
7. Does the request path and the worker still call the same function?
8. If the answer to 1, 2 or 5 is no, the change is rejected regardless of how it tests.
