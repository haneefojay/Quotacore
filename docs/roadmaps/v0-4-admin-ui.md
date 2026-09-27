# v0.4 — the embedded operator interface

Quotacore · 2026-09-27

Two phases, `IP-24` and `IP-25`. The interface the [mvp-scope.md](../product/mvp-scope.md) §2
description calls read-heavy first, served by the same binary as the API and authenticated with
admin-scoped keys.

Read [roadmap-index.md](roadmap-index.md) first.

## Phase map

| ID | Phase | Depends on | Status |
| --- | --- | --- | --- |
| `IP-24` | Embedded admin shell | `IP-16` | `BLOCKED` |
| `IP-25` | Admin views and guarded writes | `IP-24` | `BLOCKED` |

## What this release must not become

A browser interface in front of a service that is meant to be embedded in someone else's
infrastructure, sold as a component, run on a customer's host with no egress. Three constraints
follow, and they are the reason this release is late rather than early.

1. **It is embedded, not hosted.** One single-page application, served by the same binary, with no
   separate origin, no CDN and no third-party asset. NFR-S3's re-scoping does not extend to loading a
   font or a script from a public host.
2. **It is a client, not a privileged path.** Every action the interface takes is an action an
   `admin`-scoped key could take over the API. There is no session the browser holds that means
   anything the API would not accept. Otherwise the interface becomes a second authorisation
   surface, and it is the one nobody threat-models.
3. **The read paths are the product.** An operator's anxiety is "what will this change do to my
   tenants", and that is answered by reading, not by writing. [Q-14](../product/assumptions-and-open-questions.md)
   already decided that a preview endpoint is cheaper than a good instinct about bulk operations.

---

## IP-24 — Embedded admin shell

**Status** `BLOCKED`, gated on `IP-16`.

**Objective.** The application loads, from the service's own origin, and authenticates with an
admin-scoped key, and nothing else about it exists yet.

**Scope.**
- Build and embed: the assets compiled into the binary, served from one path, with a content hash
  and a cache policy.
- Authentication: a key entered by the operator, held in memory only, and never written to storage
  the browser can read at rest.
- The request layer, so that every call carries `X-Request-Id` and the same retry policy the SDKs
  will use.
- The read-only shell: navigation, layout, and error presentation.

**Specification references.**
- [mvp-scope.md](../product/mvp-scope.md) §2, v0.4
- [api-conventions.md](../architecture/api-conventions.md), which the interface obeys as a client
- [ADR-0012](../decisions/0012-hashed-scoped-api-keys.md)
- [ADR-0010](../decisions/0010-go-chi-spec-first-openapi.md), the contract the client is generated
  against
- NFR-S3, NFR-S4, NFR-S10, NFR-C5

**Dependencies.** Blocked by `IP-16`. Blocks `IP-25`.

**Definition of Done.**

1. The application is served from the service's own origin, and a test asserts that loading any page
   makes no request to any other host — with egress blocked, the application still works.
2. No third-party asset is referenced, including fonts, analytics and error reporting. Asserted by a
   build check over the bundle, not by a review.
3. A key is held in memory for the session, is never written to `localStorage`,
   `sessionStorage` or a cookie, and is not recoverable from the browser's storage after a reload
   (DR-043, NFR-S1). A test asserts the storage is empty.
4. Every request the interface makes carries `X-Request-Id`, and it appears in the error the operator
   sees, so a support conversation starts with a traceable value (NFR-S10).
5. The interface is not a second authorisation surface: every route it uses is a documented API
   route, and the set of routes it uses is asserted to be a subset of the contract's routes.
6. A `runtime`-scoped key is refused in the interface, and the refusal names the scope required
   (NFR-S4).
7. The bundle's size is measured and recorded, and the build fails if it exceeds the stated budget.

**Exit criteria.** An operator can open the interface, authenticate, and see an empty shell, with no
request leaving the host.

**Deferred.** Every view. This phase is the plumbing, deliberately, so the views are built against
something testable.

---

## IP-25 — Admin views and guarded writes

**Status** `BLOCKED`, gated on `IP-24`.

**Objective.** The six views, and write actions that show their exact effect before they commit.

**Scope.**
- Tenant list and detail, plan editor with cycle-boundary preview, feature list, event history, audit
  history, and the live balance inspector.
- Write actions: create, edit, archive, grant, set, force-rollover, suspend, resume, apply-now, key
  issuance.
- The confirmation step, which shows the effect including the `cycle_end` at which it first applies.
- Pagination and filtering, matching the API's.

**Specification references.**
- [mvp-scope.md](../product/mvp-scope.md) §2, v0.4
- [use-cases.md](../product/use-cases.md), [state-machines.md](../product/state-machines.md), the
  operations and their allowed transitions
- [DR-013](../product/domain-rules.md), [DR-014](../product/domain-rules.md),
  [DR-041](../product/domain-rules.md)
- [INV-P2](../product/state-machines.md), [INV-P3](../product/state-machines.md)
- [error-catalog.md](../product/error-catalog.md), every code a view can surface
- [Q-04](../product/assumptions-and-open-questions.md) and
  [Q-14](../product/assumptions-and-open-questions.md)

**Dependencies.** Blocked by `IP-24`. Blocks nothing.

**Definition of Done.**

1. All six views in [mvp-scope.md](../product/mvp-scope.md) §2 exist, and each is exercised by a test
   against a seeded stack rather than by a screenshot.
2. **Every write confirms first, and the confirmation states the effect, including the `cycle_end` at
   which the change first applies.** A test asserts the confirmation for a plan edit names that
   instant, and that the value shown is the value the API returns.
3. The plan editor's boundary preview calls the impact route from `Q-14` rather than computing the
   answer in the browser. A test asserts the interface displays the API's number and no other.
4. A bulk `apply-now` above the threshold from `Q-04` presents the confirmation token flow, and
   cannot be completed in one click (INV-P2).
5. An operation with no legal transition is not offered, and a `409 resource_version_conflict` from
   a concurrent edit is presented as a conflict with a way to reload, not as a failure
   (INV-P3).
6. Every error code a view can surface has a message that names a remedy, and the catalogue's client
   action is what the view does.
7. The live balance inspector is read-only, and a test asserts it issues no mutating request.
8. Every action the interface can take is in the CLI's mapping table from `IP-14`. If the interface
   can do something the CLI cannot, one of them is missing something, and the test says which.

**Exit criteria.** An operator can do the whole job from the browser, safely, and can explain before
clicking what a change will do.

**Deferred.** Multi-tenancy of the interface itself, saved views, and bulk CSV import.
