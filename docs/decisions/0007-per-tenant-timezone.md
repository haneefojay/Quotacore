# ADR-0007 — Per-tenant IANA time zone, with UTC as the default

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [cycle-engine](../architecture/cycle-engine.md), [domain-rules](../product/domain-rules.md)

## Context

The previous project history stated the rule as: *"All reset calculations MUST be executed in
strictly UTC. Localising billing logic causes fatal drift."* That is a real principle — mixing
implicit local time into cycle arithmetic produces off-by-one-day errors that are impossible to
reproduce in a test — but it conflates two different things:

1. **Where the computation runs.** The service must not depend on the host's local time zone.
   That is non-negotiable and is honoured here: the process pins its own working zone to UTC
   and only ever converts explicitly.
2. **Whose calendar defines the boundary.** A Berlin customer whose monthly allowance resets
   "on the 1st" means 1st in Berlin. Under a UTC-only rule they get 02:00 local on the 1st in
   summer and 01:00 local in winter, and support tickets arrive from customers who were
   promised midnight.

The alternative is to push the problem to the customer: the parent application computes the
anchor instant itself and tells Quotacore a UTC timestamp. That works, and every customer will
get it subtly wrong, differently, and in a way that only manifests twice a year.

## Options considered

**A — UTC only, host pinned to UTC.** The anchor is an instant; boundaries are UTC calendar
arithmetic.
- Pros: zero time-zone code, no DST cases, trivially reproducible. The previous proposal.
- Cons: a promise of "monthly" becomes "monthly at an arbitrary local hour", and that hour
  changes twice a year. For the primary use case — a customer-facing "your tokens reset
  midnight" message — this is wrong at the edges.

**B — Per-tenant IANA time zone, default UTC (chosen).** The tenant carries a time zone;
  boundaries are computed as calendar arithmetic *in that zone*.
- Pros: matches the customer-facing promise; the calendar logic is written once, correctly, in
  one place; `time/tzdata` is embedded in the binary so behaviour does not depend on the host's
  zone database.
- Cons: daylight-saving transitions must be handled correctly in three cases (a boundary that
  does not exist, a boundary that occurs twice, and a day that is 23 or 25 hours long); zone
  data is versioned and must be kept current for historical correctness.

**C — Fixed UTC offsets.** Store "+01:00".
- Cons: zones change, and no offset models daylight saving. Rejected.

**D — Per-tenant calendar locale** (for example, a "billing month" that starts on the 5th).
- Real requirement in some markets, but it is a schedule abstraction layered on top of time
  zones, and it is not needed for the MVP. Deferred; the pure cycle function is the right place
  to add it later because it has no I/O.

## Decision

**Option B, with four rules that make it tractable.**

1. Every tenant stores an IANA zone name (`tenant.timezone`), defaulting to `UTC`. Validated on
   write against the embedded zone database; an unknown zone is a `400`, never a silent fallback
   to UTC, because a silent fallback produces exactly the drift the rule was meant to prevent.
2. **The process working zone is UTC.** All arithmetic is performed by constructing explicit
   wall-clock times in the tenant's zone, never by adding durations to a localised instant.
3. **The boundary is a wall-clock time, not a duration.** "Monthly" means the same wall-clock
   time on the anchor's day-of-month, in the tenant's zone, clamped to the last day of shorter
   months. Adding 30 days is not "monthly" anywhere and is banned.
4. **Two intervals are durations, not calendar dates**, because customers mean a fixed period
   when they say so: `hourly` is exactly 3600 seconds, `weekly` is seven calendar days at the
   anchor's wall-clock time. This is stated in [domain-rules](../product/domain-rules.md) so the
   asymmetry is not mistaken for an oversight.

DST is handled by construction rather than by special cases, because Go's `time.Date` performs
the normalisation: a wall-clock time that does not exist on a spring-forward day resolves
forward, and one that occurs twice on a fall-back day resolves to the first occurrence. Both
behaviours are asserted in the test matrix in [cycle-engine](../architecture/cycle-engine.md)
rather than left to review.

## Consequences

**Positive**
- "Resets at midnight on the 1st" is literally true for the customer, in their own zone.
- Calendar edge cases are solved once, centrally, and are unit-testable without a datastore.
- UTC remains the default, so a customer who never sets a zone gets the simplest behaviour.
- The pure `cycleWindow` function is independent of I/O and of the data plane, which makes the
  hardest logic in the product exhaustively testable.

**Negative**
- Daylight saving is a genuine source of bugs and requires a deliberate test matrix: at minimum
  Europe/Berlin, America/New_York, Australia/Lord_Howe (30-minute DST shift) and a fixed-offset
  zone, each across both transition directions.
- The embedded `time/tzdata` snapshot determines historical and future boundary calculations.
  A zone-rule change by a government therefore changes past cycle boundaries if the binary is
  rebuilt with newer data. This is a real, documented, and generally acceptable property;
  recording the zone-data version in `/v1/admin/status` makes it diagnosable.
- The time zone is part of a tenant's configuration, so changing it mid-cycle changes future
  boundaries without re-anchoring. Documented as intended: the anchor instant and wall-clock
  time are preserved, the zone is not.

## Revisit when

- A business-calendar requirement (custom billing months, holidays) appears, which is option D.
- Tenants are large enough that a single cycle window must span heterogeneous regional rules,
  which would push calendar ownership out of the tenant record.
