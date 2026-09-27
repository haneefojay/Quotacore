# ADR-0013 — Single-tenant instance, flat tenants, opaque identifiers, no PII

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [data-model](../architecture/data-model.md), [SECURITY.md](../../SECURITY.md)

## Context

A quota engine can be deployed in two shapes. Either one instance serves many customers who
share a database, or each customer runs their own instance. The choice determines the data
model, the authorization model, the threat model, and the backup story.

The product's distribution model is self-hosted: a single container image that a customer runs
in their own infrastructure. A hosted offering is a future possibility and is explicitly out of
scope for the MVP (see [mvp-scope](../product/mvp-scope.md)).

## Options considered

**A — Single-tenant instance, flat tenant list (chosen).** One instance serves one customer. A
"tenant" is a row: the user's account, workspace, customer, or device, whichever the integrating
application uses as its unit of quota. The tenant is identified by an opaque external
identifier that the parent application supplies.

**B — Single instance with an organisation hierarchy.** Tenants grouped under organisations,
with organisation-wide allowance shared down to members. This is what "Enterprise customers
have custom limits" sounds like.
- Pros: models a real enterprise requirement, and org-level budgets are how enterprise buyers
  think.
- Cons: introduces a second level of allocation. A shared pool needs a rule for what happens when
  a member consumes more than their share, what happens to a member's balance when the org
  allowance is reduced, and how partial resets propagate down a hierarchy. Every one of those is
  a genuine product decision that has to be designed, specified, and tested. The previous
  project history excluded hierarchies from the MVP for the same reason, and that judgement was
  right.

**C — Multi-tenant SaaS.** Many customers in one instance.
- Pros: one deployment serves many customers; a hosted business becomes possible without new
  architecture.
- Cons: per-customer isolation, key scoping, noisy-neighbour quotas, per-customer rate limits,
  and a materially different threat model. It is a different product with a different security
  posture, and adopting it during the MVP would compromise both.

## Decision

**Option A.** A single-tenant instance with a flat list of tenants.

- **A tenant is whatever the parent application declares it to be.** Quotacore does not model
  users, workspaces, organisations, devices or teams. It models exactly one concept: a quota
  holder identified by an external identifier.
- **`tenant.external_id` is supplied by the customer and is opaque to Quotacore.** It is
  validated as a bounded, restricted character set and otherwise treated as an opaque token:
  `[A-Za-z0-9._:-]{1,128}`. That set deliberately excludes `@`, so an email address is
  syntactically invalid, and it is enforced at the API boundary rather than requested politely.
- **The "max 3 projects" case is not a hierarchy.** It is a single metered feature on a single
  tenant, consumed when a project is created and refunded when it is deleted (ADR-0005). A
  customer who wants one allowance shared across a whole organisation provisions one tenant for
  the organisation and calls it from the appropriate place in their own authorisation logic. The
  decision of *which user* draws on a shared allowance stays in the customer's application,
  where their own domain model lives.
- **No personally identifiable information is stored.** Tenant names, emails, addresses,
  payment references, plan names a customer might have chosen as a free-text field, and
  anything else identifying a natural person are all out of scope. `tenant.metadata` is a
  small JSON object for operational correlation, explicitly documented as not-PII, and it is
  not indexed, not searchable by content, and not included in log lines by default.
- **The consequence is stated as a product property, not a side effect:** if Quotacore is
  compromised, the attacker gets opaque identifiers, integer balances, plan configuration and
  usage volumes. They do not get a customer list. For a component that sits in the path of
  every protected request, that materially reduces the severity of a breach and simplifies the
  privacy obligations Quotacore takes on.

## Rationale

The customer already has a domain model. Quotacore's job is to enforce a number, and the moment
it starts modelling who the users are it has duplicated the customer's authorisation logic in a
component that is deliberately kept ignorant of their domain. Flat tenants with opaque
identifiers is the smallest model that solves the actual problem.

This also makes the security review tractable. There is one privilege boundary to reason about —
the customer's application and its operator — rather than a customer-to-customer isolation
model. That is a large part of why the MVP can ship without a hosted-offering security
programme.

Declining the organisation hierarchy is the most consequential product decision in this ADR. It
means a customer with genuinely hierarchical needs keeps that logic in their own application,
where they already have it.

## Consequences

**Positive**
- The data model is small enough to reason about completely, and there is no inheritance or
  allocation semantics to get wrong.
- No cross-tenant isolation to implement, test or audit, because there are no other customers'
  tenants in the database.
- A breach of a Quotacore instance discloses no customer identity.
- The tenant concept maps onto whatever the customer already has, so integration does not
  require restructuring their data model.
- The future hosted offering remains possible; it would be a new deployment mode with its own
  isolation model, added on top of this schema.

**Negative**
- Customers with organisation-wide budgets must implement the split themselves. This is a real
  adoption cost and must be documented with a worked example rather than left to discovery.
- Per-tenant metrics cannot be aggregated across customers, which matters for a future hosted
  offering but not for self-hosted.
- A customer who provisions a tenant per user instead of per organisation will get N times the
  keyspace. Capacity planning in [deployment](../architecture/deployment.md) covers this, and
  the tenant count limit makes it a configuration error rather than an incident.
- The hosted, multi-tenant version is a larger piece of work than the schema retrofit looks
  from here. Accepted.

## Revisit when

- A hosted offering is committed to, which requires an isolation and key-scoping model and
  probably a schema change.
- A majority of enterprise prospects require native hierarchical limits, at which point option B
  becomes unavoidable and should be designed as its own release.
