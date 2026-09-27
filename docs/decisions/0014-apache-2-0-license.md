# ADR-0014 — Apache License 2.0

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [PROJECT.md](../../PROJECT.md), [README.md](../../README.md)

## Context

The project's stated intent is an open-source core with a possible hosted, highly-available
offering later. The licence determines who may build on it, whether the project can be adopted
by companies with restrictive policies, and whether a hosted version is legally possible
alongside the open-source one.

## Options considered

**A — Apache License 2.0 (chosen).**
- Pros: OSI-approved and universally recognised as open source; permissive, so adoption friction
  is minimal; includes an **explicit patent grant and patent-retaliation clause**, which for an
  infrastructure project that might accumulate a patent portfolio around enforcement mechanisms
  is the single most valuable clause; requires a NOTICE file, which is good hygiene; explicitly
  permits operating a hosted service from the code.
- Cons: includes a patent grant that some corporate counsel review closely; the NOTICE
  propagation requirement must be honoured when redistributing.

**B — MIT.** Shortest and most permissive.
- Pros: minimal, universally accepted, zero compliance burden.
- Cons: **no explicit patent grant.** For a company that might build a competing or adjacent
  product, the absence of a patent licence is a real question for their counsel, and it is a
  known reason infrastructure projects choose Apache over MIT.

**C — BSD-3-Clause.** Functionally equivalent to MIT for our purposes.
- Pros: familiar, the historical default for infrastructure.
- Cons: the same absence of an explicit patent grant. Weaker ecosystem signalling than Apache-2.0
  in 2026.

**D — BUSL-1.1 (Business Source License).** Source-available, with a production-use restriction
  that converts to an open licence after a change date.
- Pros: designed precisely for "open source plus a hosted business" — it allows selling the
  service while preventing a competitor from running the unmodified code as a service.
- Cons: **not OSI-approved**, so it is not open source by the strict definition the project's
  positioning depends on. Using it means the project cannot honestly claim to be open source, and
  several package registries and enterprise open-source policies treat BUSL-licensed
  dependencies as unapproved.

**E — AGPL-3.0.** Copyleft with network-use provisions. This is what Lago, the closest
  open-source comparable, uses.
- Pros: guarantees that a hosted derivative is published, which protects against a cloud provider
  running an unmodified copy as a competing service.
- Cons: it forbids exactly what the hosted offering needs. Under AGPL the hosted version must
  either be published under AGPL or be licensed commercially, which means the primary commercial
  artefact would be closed. It is also actively rejected by large numbers of enterprises, which
  directly harms adoption of a component that must be embedded in someone else's infrastructure.

## Decision

**Apache License 2.0**, with a `LICENSE` file containing the canonical text and a `NOTICE` file
maintained from the first release.

## Rationale

Two properties matter for this specific project. First, adoption: Quotacore has to be
acceptable to a company's legal team, and a widely-deployed infrastructure dependency that
triggers an exception request is a dependency that never gets deployed. Apache-2.0 is the
lowest-friction permissive licence with an explicit patent grant, and that combination is
exactly what makes infrastructure projects adopt it.

Second, commercial viability: Apache-2.0 explicitly permits operating a network service from the
code, so a hosted offering is possible without a separate commercial licence for the
unmodified product. BUSL would have been defensible for that, but the cost of being unable to
claim open source status is higher than the benefit of the extra protection.

Note the honest trade-off being made: Apache-2.0 does **not** prevent a cloud provider from
running the OSS build as a competing hosted service. AGPL and BUSL would. That risk is accepted
because the defensibility of this product is stated as engineering credibility and adoption
speed, not as licence-based lock-in — and lock-in is explicitly the wrong strategy for a
component that must be trusted with a revenue path.

## Consequences

**Positive**
- Lowest legal friction of any option, which directly supports the self-hosted adoption motion.
- Patent grant and retaliation clause, reducing an enterprise adoption objection.
- A hosted offering is legally straightforward.
- Compatible with AGPL and GPL projects, so a customer can embed it in a larger codebase without
  a licence conflict.

**Negative**
- A competitor may run the OSS build as a hosted service. Accepted deliberately.
- NOTICE propagation must be maintained across releases. A `make notice` check in CI prevents
  this from being forgotten.
- Apache-2.0 does not require source disclosure for hosted use, so the hosted offering's
  proprietary value is limited to operations, scale and reliability, not to code. That shapes the
  eventual business model toward a managed-service proposition rather than a proprietary-licensing
  one.

## Revisit when

- A hosted offering is committed to and the business model depends on source-available terms, at
  which point BUSL-1.1 is the reconsidered option.
- The project adopts additional dependencies whose licences are incompatible with Apache-2.0.
