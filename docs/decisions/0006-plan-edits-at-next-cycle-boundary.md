# ADR-0006 — Plan edits take effect at the next cycle boundary by default

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [domain rules](../product/domain-rules.md), [overview](../architecture/overview.md), [use cases](../product/use-cases.md)

## Context

A plan's limit is the number every tenant on that plan is entitled to. When an operator edits
it — Pro goes from 10,000 to 20,000 tokens per month, or 20,000 down to 15,000 — the tenants
already assigned to Pro are mid-cycle. Their cycle started under the old limit, and their
balance is a number derived from the old limit.

There are three defensible behaviours, and they are not equivalent from the customer's point of
view.

## Options considered

**A — Apply immediately to every assigned tenant.**
- Pros: what the operator typed is what is true now. One mental model, no second code path.
- Cons: the operator has just changed the *offer*, not the *contracts*. A price rise applied
  mid-cycle instantly cuts off customers who have already paid for the period. A price cut
  applied mid-cycle instantly grants free quota to the entire installed base, on the operator's
  say-so, with no proration and no record of who received how much. The blast radius of a
  single `PATCH` is the entire customer list.

**B — Apply at each tenant's next cycle boundary (chosen).** Existing tenants keep their current
limit until their own boundary; at the boundary they reset to whatever the plan says *then*.
New tenants assigned to the plan get the new limit immediately, because they have no cycle in
progress.
- Pros: no customer is cut off or credited mid-cycle by an unrelated edit. A tenant only ever
  sees a new limit at the moment its allowance is restored, which is the only moment a limit
  change is meaningful to it.
- Cons: a limit edit can take up to one full cycle to reach existing tenants. An operator who
  urgently needs a cut applied will find the change has no visible effect, and must know to
  press the other button.

**C — Preserve consumed usage and recompute the balance.** Used 8,000 of 10,000, new limit
20,000, remaining becomes 12,000.
- Pros: the fairest outcome, and the only one that behaves correctly for a mid-cycle price rise.
- Cons: requires knowing consumption *this cycle* as a separate quantity, not just a balance.
  The data plane currently stores a balance and an allowance; it does not store a usage total.
  Adding one is a data-model change with its own migration and its own failure mode, and the
  fairness argument applies to a bounded set of cases. Deferred — see "Revisit when".

## Decision

**Option B, with an explicit, audited escape hatch.**

- Editing a plan's entitlement **bumps the plan version** and changes the limit that applies to
  *new* assignments and to the *next* boundary of existing ones. The reset path in ADR-0003
  reads the limit at the moment the window is computed, so the edit lands exactly on the
  boundary with no extra machinery.
- `POST /v1/admin/plans/{plan_id}/apply` performs an **apply-now**: it re-anchors and re-limits
  every currently assigned tenant immediately. This is a deliberately loud, audited,
  explicit action with a required `reason`, not a side effect of editing.
- **Plan assignment changes are a different operation and a different rule.** Moving a tenant
  from Free to Pro is a hard reset: the new plan's limits apply immediately and the tenant's
  anchor moves to now. Prorated blending of a partial upgrade is deferred. This distinction —
  *editing a plan* is a change to the offer, *assigning a plan* is a change to a contract — is
  the whole reason the two operations need separate rules.

## Rationale

The operator's intent when editing a plan is almost always "from now on, this plan includes
more", not "retroactively change what every current subscriber has been granted". Option B
matches that intent and is the only one of the three where a plan edit cannot, by itself,
change any customer's current entitlement. Option A makes a single API call a
mass-entitlement event.

Option C is deferred rather than rejected on the merits. It is the right long-term answer for
fairness, but it is strictly more machinery for a case the MVP does not have to serve, and
ADR-0003's window-computation model is deliberately kept free of a "usage so far" input.

Making apply-now explicit and audited converts the risky behaviour from a default into a
deliberate action with an actor and a reason attached, which is the correct place for it.

## Consequences

**Positive**
- A plan edit can never cut off a paying customer mid-cycle or hand out free quota to the
  installed base by accident.
- No new data is needed; the boundary already recomputes the limit.
- The dangerous behaviour still exists, but is reachable only through an audited endpoint.

**Negative**
- Limit changes propagate slowly — up to one cycle. Operators must be told, in the API docs and
  the admin tooling, or they will file a bug report that the change "did not work".
- Two distinct admin operations with different semantics (assign versus apply) must be clearly
  distinguished in the API surface, the CLI, and the documentation. A confusing UI here causes
  real support load.
- The apply-now operation fans out across every assigned tenant. It must be bounded, chunked and
  observable, and it is the one admin operation that can take meaningful wall-clock time on a
  large instance. Specified as a chunked job with progress, not a synchronous loop.

## Revisit when

- Prorated upgrades become a requirement, which is the trigger to store per-cycle usage
  explicitly and revisit option C.
- Plans gain effective-dating, at which point "next boundary" becomes a query against a schedule
  rather than a single current version.
