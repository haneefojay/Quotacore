# ADR-0015 — A hard MVP boundary, with the remaining capability sequenced behind it

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [mvp-scope](../product/mvp-scope.md), [ROADMAP.md](../../ROADMAP.md)

## Context

The intended release scope was assembled from several sources over the course of the project:
an original MVP list, a later PRD, a phase plan, and a distribution strategy. Read together they
specified a runtime API, an admin API, a reset engine, a webhook dispatcher, an admin UI, a
CLI, metrics, API documentation, boolean entitlements, rolling windows, and official SDKs for two
languages.

That is not one MVP. It is a product roadmap compressed into a single definition of done, and
the previous documents reflected that: the original MVP list included a webhook dispatcher that
appeared in no later phase, and the definition of done required a UI that the MVP section had
excluded.

The previous plan estimated six weeks for a subset of this. A realistic solo-engineer estimate
for the whole set is roughly three times that, and two of the items — SDKs and webhook delivery —
create permanent maintenance obligations rather than one-time work.

## Options considered

**A — Keep everything in one MVP.** One release, one definition of done, all capability.
- Pros: one document, one launch, no argument about what is next.
- Cons: 12 to 16 weeks before any user feedback, no usable artifact to hand to a design partner
  until the very end, and the first release's correctness story is diluted across three
  different enforcement algorithms, a webhook subsystem, a frontend and two SDK packages.

**B — Hard MVP plus sequenced follow-on releases (chosen).** Cut the *release* boundary, not
  the scope. Every selected capability is fully specified, given a dependency order, and given
  its own definition of done in a later release.

**C — Enforcement core only, nothing else committed.** Defer boolean entitlements, rolling
  windows, webhooks, UI and SDKs indefinitely.
- Pros: the smallest possible first release.
- Cons: discards capabilities that were explicitly selected, and rolling windows in particular
  is a direct promise in the project's own originating problem statement. Rejected as
  unnecessarily destructive; sequencing achieves the same protection without discarding intent.

## Decision

**Option B.** The release sequence is:

| Release | Contents | Rationale for the position in the sequence |
| --- | --- | --- |
| **v0.1 (MVP)** | Runtime API: `consume`, `check`, `refund`; idempotency; anchored cycle engine; control plane and admin API; hashed API keys; audit log; usage event ledger; CLI; Prometheus metrics; structured logs; OpenAPI 3.1 served in-binary; Dockerfile; `docker compose` quick start; generated reference docs | This is the product's hard part: atomicity, idempotency and correct cycle arithmetic. Nothing else is worth building before it is proven. It is a complete, genuinely useful product on its own. |
| **v0.2** | Boolean entitlements; rolling/sliding windows | Additive: new feature kinds, new keys, new scripts, new tests. No change to the cycle engine or to the existing feature kinds. |
| **v0.3** | Outbound webhook dispatcher with signed delivery, retry and dead-lettering | Requires a persistent outbox, a delivery worker, HMAC signing and SSRF controls. Infrastructure that is only valuable once the core is trusted. |
| **v0.4** | Embedded admin UI | Useful for support staff, but every capability it exposes already has an API and a CLI. It is a surface, not a capability. |
| **v0.5** | Official TypeScript and Python SDKs | Depends on the API being stable, which is only true after v0.2. SDKs published against a moving API create permanent maintenance and a bad first impression. |

**The MVP's definition of done is scoped to v0.1 only**, and the roadmap carries an explicit
gating dependency for each later release.

Two capabilities are **cut from the release sequence entirely** rather than deferred, because
they are not the product: invoicing, payment processing and credit-card handling (the customer's
billing provider owns money movement), and customer-facing billing portals. Both are recorded in
[mvp-scope](../product/mvp-scope.md) as explicitly out of scope with reasons.

## Rationale

The purpose of a release boundary is to force a decision about what "usable" means and then hold
it. A boundary that includes a frontend, a webhook subsystem and two SDK packages is not a
boundary; it is an aspiration, and it guarantees that the hardest part of the project — the part
that determines whether the product is correct — is not finished until everything is finished.

Cutting the boundary rather than the scope costs nothing in total effort and buys three things:
a usable artifact in roughly six to seven weeks that can be put in front of design partners; a
definition of done that actually gates; and the ability to discover that the atomicity and cycle
design is wrong before the webhooks, the UI and the SDKs have been built on top of it.

v0.2 goes first among the follow-ons because rolling windows is a direct promise in the
originating problem statement and boolean entitlements is the cheapest of the three. The UI goes
late because it adds a frontend toolchain and a second authentication surface (session plus CSRF)
to a security-sensitive service, and adds no capability the API does not already have.

## Consequences

**Positive**
- v0.1 is genuinely useful and demonstrable, which is the precondition for adoption of anything
  else.
- Each release has a definition of done that can actually fail.
- The risky part of the project is finished and proven before webhooks, a UI or SDKs are built on
  top of it, so a design change costs days instead of months.
- A capability that turns out not to be needed can be dropped without jeopardising the release.

**Negative**
- Five releases to communicate instead of one, and a v0.1 that a customer may find
  insufficient because it has no per-minute rate limiting and no boolean entitlements. The
  marketing consequence is real and must be managed: copy says "quotas", not "rate limits".
- A customer needing per-minute limiting and boolean entitlements must wait for v0.2. Documented
  as a known limitation with the workaround.
- More release engineering overhead: five changelogs, five migration sets, five compatibility
  commitments.
- The per-minute rate limit promised in the originating problem statement is not delivered in
  the MVP. This is the sharpest edge of the decision and is recorded here deliberately.

## Revisit when

- Design partners need multiple capabilities simultaneously to commit to a pilot, which would
  justify collapsing v0.2 and v0.3 into the MVP.
- The API changes materially in v0.2, which would indicate the v0.1 contract was premature and
  that SDKs must wait even longer.
