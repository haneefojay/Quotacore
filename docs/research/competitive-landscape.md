# Research: Competitive Landscape

**Date:** 2026-09-27 · **Status:** complete · **Affects:** [foundation](../product/foundation-and-personas.md), [mvp-scope](../product/mvp-scope.md), [ADR-0015](../decisions/0015-release-slicing.md)

**Read this first, and read it as a constraint on the product rather than as a source of
encouragement.** The finding is that the space is populated, that the incumbents are well funded,
and that the honest wedge is narrow. Pricing and feature claims below change frequently and were
recorded as of the research date; treat them as directional, not as a basis for a commitment.

## Question

Who is solving this problem today, how well, and where is there a gap a small, self-hosted,
Apache-2.0 component can occupy without being squeezed from both sides?

## Why it mattered

Because the most likely failure mode for this project is not a technical one. It is building
something correct that nobody has a reason to switch to, or something correct that a well-funded
platform absorbs as a feature. Deciding the position early is worth more than any amount of
performance work.

## Method

Categorised candidates by what they actually do on the enforcement path, read their public
documentation on entitlements, metering and idempotency, and evaluated each against the specific
needs recorded in the [foundation](../product/foundation-and-personas.md) — atomicity, retry
safety, self-hosting, licence compatibility, and single-command deployment.

## Findings

### The landscape, by category

| Category | Representative players | What they do well | Where they leave a gap |
| --- | --- | --- | --- |
| **Usage and entitlement platforms** | Stigg, Lago, OpenMeter, Orb, Metronome, Polar, and others | Broad coverage: metering, entitlements, credits, wallets, invoicing integrations. Mature on the commercial side | Enforcement is one feature among many. The hot-path decision is often advisory, and the accuracy of *blocking* is not their headline metric |
| **Billing platforms with an entitlements surface** | Stripe (Entitlements API), and others | Distribution, invoicing, and an existing trust relationship. Entitlements is a deliberate carve-out from billing for exactly the latency reason this project exists | You are inside their platform. The usage-based metering path is a documented recommendation rather than the product |
| **API gateways and rate limiters** | Envoy, Kong, NGINX, Cloudflare, and others | Excellent at infrastructure rate limiting: requests per second, per IP, per route | Not a business allowance. They do not model calendar cycles, plan limits, per-customer entitlements, or credits, and adding them would be an awkward fit |
| **Feature flag and configuration services** | LaunchDarkly, Unleash, GrowthBook, and others | Mature targeting, gradual rollout, experimentation | Different question entirely. A flag answers "should this code path run", not "is this customer entitled to this capability". They share vocabulary and nothing else |
| **In-house implementations** | Everywhere | Perfectly fitted to one company | The status quo. A counter column, a Redis `INCR` guarded by an `if`, or a cron reset. This is what the product is displacing, and it is where most of the demand comes from |
| **This project** | Quotacore | Atomic enforcement, retry safety, self-hosted, permissive licence, one command | Not a billing system, not a gateway, not a flag service, and deliberately incomplete as a platform |

### The honest assessment

**The problem is real and validated, and the space is not empty.** Any pitch implying otherwise
would fail on contact with an informed prospect, and would waste the credibility the product needs
to earn. The category has funding, has multiple credible implementations, and has been picked up
by at least one platform with essentially unlimited distribution.

Two things are nevertheless true, and they are the whole of the opportunity:

1. **Entitlement *checks* have been consistently treated as secondary to entitlement *management*.**
   Platforms sell plans, dashboards and invoicing, and the request-time check is a feature. The
   hard part of a check — atomic decrement, retry safety, correct calendar reset — is documented as
   configuration in most of them rather than demonstrated as a tested property.
2. **The self-hosted, no-account, permissively-licensed segment is largely unserved by the
   platforms.** They are hosted services. Their open-source editions exist, but they are large
   systems with a learning curve, and the enforcement path is still the same secondary feature.

That combination is the gap: **a component that is only the enforcement path, is correct about it,
runs on the customer's host, and can be vendored into a proprietary product.**

### Where the incumbents genuinely beat this design

Stated at length on purpose. A competitive analysis that only lists advantages is marketing.

| Incumbent advantage | Why it matters | Response |
| --- | --- | --- |
| Invoicing, payments, dunning, revenue recognition | A real product with real customers | **Do not compete.** Out of scope permanently, and the customer's existing provider already does it |
| Feature breadth | One vendor instead of five | Accept. Breadth is not the differentiator and chasing it is how a focused component becomes a platform |
| Managed hosting and a support SLA | Removes operational work | Partly addressed by the single-command deployment; the SLA gap is a real limitation and is not pretended away |
| Ecosystem and integrations | Sales and marketing advantage | Not addressable by engineering |
| Dashboard and reporting UX | A visible, tangible feature | Addressed at v0.4, later than the enforcement correctness, which is the correct order |
| Brand trust and existing contracts | The hardest thing to displace | Only addressable over time, by being the component they cannot remove from their own stack |

The honest conclusion: **this product does not win by being a better platform. It wins by being a
better component, and by being installable in a way that a platform is not.** If it is evaluated as
a platform, it loses. That is a positioning constraint, not a product gap, and it should shape
every scope decision — including the refusal to add features that would make it look more like a
platform.

### What the competitors' failures teach

| Observed pattern in the field | The lesson taken |
| --- | --- |
| Denial semantics that are hard to distinguish from transient errors | A typed, permanent code for an exhausted allowance, distinct from a rate limit, distinct from a service error ([DR-040](../product/domain-rules.md)) |
| "Check" endpoints that read as a gate, with the race left to the customer | `check` is documented as advisory in three places, and the failure mode of ignoring it is described concretely (J-2) |
| Idempotency presented as an optional best practice | Required, not optional. The cases a server cannot protect against are disclosed rather than glossed (FS-03) |
| Reset implemented as a scheduled job | Lazy, monotonic, atomic, with the failure mode of the alternative written down (FS-05) |
| Licences that changed under customers | A BSD-3-Clause default image, and a dated licence section ([licensing research](licensing-redis-vs-valkey.md)) |
| No incident-visible failure mode for the enforcement path | Every error code has a metric, an alert and a severity, and two of them are treated as money ([observability](../architecture/observability.md#5-alerts)) |
| Missing allocation-cap support, forcing customers to hand-roll counters | Allocation caps as a first-class use case, with the delete-path refund requirement documented (UC-10) |
| Enterprise plans requiring a code change | Per-tenant overrides with a stated precedence rule (UC-13, J-8) |

Each of these is a place where a small, focused component can be genuinely better, and every one
of them is a correctness property rather than a feature.

### Pricing and packaging

Deliberately not analysed in depth. The conclusions that matter are structural, not numerical:

- A self-hosted component has no marginal cost of service, so there is nothing to meter and no
  reason to have tiers based on volume. Charging for a self-hosted component by usage would require
  phoning home, which contradicts the trust story.
- The viable models are an open-source core with paid support, or a paid convenience tier. Both are
  commercial questions, not product questions, and neither changes the architecture.
- **Any model that requires the service to make an outbound call is disqualifying for this
  product's positioning** (NFR-S3). A metering component that reports usage of itself would be
  holding the most sensitive kind of data about the least willing customer.

## What was rejected, and why

| Rejected | Reason |
| --- | --- |
| Competing as a metering and billing platform | The field is well funded and the incumbents have distribution. The product would lose on breadth regardless of quality |
| Competing as a feature-flag service | A different question, already well served, and reaching into it would dilute the product |
| Competing as an API gateway | Infrastructure rate limiting is a solved, adjacent problem, and it would require sliding-window counters the MVP deliberately excludes (ADR-0008) |
| Adding usage-based pricing to the service itself | Requires egress, and the trust cost exceeds the revenue by a wide margin |
| A feature-gap roadmap driven by competitor comparison | Every competitor has more features. Chasing them converts a component into a worse platform |

## Decision impact

- The [foundation](../product/foundation-and-personas.md) states the competitive table honestly,
  including that the space is populated, and narrows the claim to the two things that are unique
  rather than comparative: **retry safety required rather than optional**, and **no external
  dependency at all**.
- [mvp-scope.md](../product/mvp-scope.md) section 4a records invoicing, payments, feature flags,
  gateway rate limiting and a customer-facing portal as permanently out of scope, with reasons.
- The scope discipline in [ADR-0015](../decisions/0015-release-slicing.md) is justified by
  positioning: a component that stays a component is installable, and a platform is not.
- [J-10](../product/user-journeys.md) is written as an adoption journey for a sceptical evaluator,
  which is the persona that matters most.
- Assumption A-01 in the [assumptions register](../product/assumptions-and-open-questions.md) —
  that metered enforcement is a durable standalone purchase — is the load-bearing commercial
  assumption, and it is the one this research most directly informs.

## Confidence and what would change this

**Medium confidence, and lower than for the technical research.** Competitive positions decay, and
this one will decay faster than the architecture. Specifically:

- **Re-verify before any release.** Feature claims, especially "does the incumbent's check endpoint
  guarantee atomic decrement", are the kind of thing that changes silently in a competitor's
  changelog. A claim in this document that has not been re-checked within two releases should be
  treated as unverified.
- **The most likely change is a platform absorbing this.** A well-resourced competitor making
  atomic enforcement a documented, tested guarantee would remove the wedge. The response would be
  to compete on the things a platform cannot adopt: self-hosting, licence, and component
  composability.
- **The least likely change is the platforms becoming worse.** The failure modes catalogued above
  are structural — billing platforms are incentivised to prioritise billing — and are unlikely to be
  fixed by the platforms themselves.

**A note on the limits of this research.** Everything here is from public documentation. No
competitor was interviewed, no benchmark was run against a competitor's product, and no pricing
was negotiated. The categories, the failure-mode catalogue and the structural conclusions are
robust; the specific feature claims are a snapshot and should be treated as a starting point for a
conversation rather than as a reference.

## Sources

- Public product documentation for Stigg, Lago, OpenMeter, Orb, Metronome, Polar, LaunchDarkly,
  Unleash and GrowthBook, read for their entitlements, metering, idempotency and cycle-reset
  semantics.
- Stripe's published guidance on entitlements, idempotency keys, and metering, including the
  documented distinction between billing and enforcement paths.
- Envoy, Kong and NGINX rate-limiting documentation, for the gateway comparison.
- Redis and Valkey licensing, via [licensing-redis-vs-valkey.md](licensing-redis-vs-valkey.md).
- General published discussion of the usage-based billing tooling market, for the category
  structure and the funding landscape.
