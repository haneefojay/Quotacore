# Research: Validating A-01, and Two Operator-Workflow Figures

**Date:** 2026-09-27 · **Status:** open · **Decision:** none; this note supplies the instrument
for [A-01](../product/assumptions-and-open-questions.md#1-assumptions) and the sources for two
configuration defaults

## Question

Is metered quota enforcement, as distinct from a boolean entitlement and from infrastructure rate
limiting, a durable standalone purchase? And, separately, where did the two operator-workflow
defaults introduced with `DR-046` come from?

## Why it mattered

[A-01](../product/assumptions-and-open-questions.md#1-assumptions) is load-bearing for the whole
product. If enforcement is not a thing people buy on its own, the register's own stated consequence
is to reposition as a component of a larger stack or add the missing adjacent surface, and that is a
re-architecture rather than a patch. It is also the one assumption in the register that no amount of
implementation can settle, because it is a claim about buyers rather than about code.

The note also carries the sources for `QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD` and
`QUOTACORE_CONFIRMATION_TTL`. Those are numbers the specification now depends on, and
[AGENTS.md](../../AGENTS.md) forbids an unsourced number, so they are sourced here rather than
asserted in the configuration table.

## Status: not yet validated

The five conversations have not happened. A-01 is recorded as an owner-accepted risk rather than as
`confirmed`, and the register carries the date and the release gate. Nothing in this repository can
substitute for them: the method is a number of conversations with a qualitative property — *about
the enforcement job, not about billing* — and only the people being asked can supply that property.

The failure mode this note exists to prevent is quiet. It would be easy to mark A-01 `confirmed`
because five conversations would be easy to *claim*, and the cost of the claim would not surface
until the product was built and unsold.

## Method: the five conversations

Each conversation is with a team that currently enforces per-feature quotas, by some means, and
that has felt the pain. Twenty minutes, same five prompts, so the answers are comparable.

1. Walk me through the last time you had to stop one feature of your product being used while
   leaving the rest of it running. What did you build?
2. What did you try first, and why did it not work? *(The interesting answers are the ones where a
   boolean flag or an infrastructure rate limit was tried and rejected.)*
3. When you say "per feature", what is the unit you meter — a request, a token, a row, a byte, a
   second of compute? How do you know the number?
4. What happens today when a customer disputes a count? Who reconciles it?
5. If this existed and worked, what would you have stopped building last year? Would you pay for
   it separately, or would you still expect it bundled?

Prompt 5 is the closest thing to a direct test of A-01, and it is the one most likely to produce a
polite non-answer. A non-answer is a data point, not a failure, and is recorded as such.

## Pass criteria, registered before the answers exist

A conversation **validates** A-01 only if all three hold:

| Criterion | Test | Why it is not negotiable |
| --- | --- | --- |
| The problem is theirs, unprompted | They describe a per-feature metering problem before prompt 2 names any alternative | A problem the interviewer supplied is not a problem they would pay to solve |
| Neither alternative solves it | They have already tried a boolean entitlement or a rate limiter, and can say why each failed | "Metered quota enforcement" and "turn the feature off" must be different purchases, or the two claims are the same claim |
| The purchase is separable | They would buy it separately rather than expecting it bundled | A-01 says *standalone*; a preference for bundling is evidence against it, not a softer yes |

A conversation **falsifies** A-01 if the participant's real problem is the adjacent surface instead:
a metering front end, an invoice, a dashboard, or the enforcement decision itself rather than the
enforcement primitive.

The aggregate rule: **three of five validate, and none falsify, and A-01 moves to `confirmed`.**
Anything less and A-01 stays `assumed`, and the register's repositioning consequence applies. A
single falsification is enough to trigger it regardless of the other four, because the falsifying
participant is describing a market that exists, and the register names it.

Recording is per conversation: date, participant type, which criteria held, in a table appended to
this note. A summary verdict without the table is not evidence.

## Re-check by

When the first three conversations are complete, or by 2027-03-31, whichever is sooner. If the
result is falsification, do not re-run the exercise: take the repositioning decision instead.

## The two operator-workflow defaults

Both are **judgements, not measurements**, and are labelled as such in
[deployment.md](../architecture/deployment.md#5-sizing) and in the rule that depends on them. They
are recorded here so that the distinction between a sourced number and an invented one is visible
rather than assumed.

| Key | Default | Basis | What would change it |
| --- | --- | --- | --- |
| `QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD` | `50` | The `apply-now` script re-anchors every assigned tenant, so cost scales with the assigned count. Fifty is low enough that the pause an operator would notice has not yet begun, and high enough that a plan of a realistic enterprise size is not routinely two-step. Judgement, not measurement | Observed `apply-now` duration against assigned-tenant count, from `IP-10` telemetry. If the 50-tenant apply takes longer than an operator's patience, lower it |
| `QUOTACORE_CONFIRMATION_TTL` | `900s` | Long enough for an operator to read the impact and re-issue the call, short enough that abandoned tokens do not accumulate in a store that cannot evict (DR-048). Judgement, not measurement | If operators report expiry, raise it. It is bounded by memory only because the store is `noeviction`, which is exactly why the bound must be small |

Neither figure is load-bearing for correctness. `DR-046`'s correctness rests on the guard engaging
at all, not on where the line sits. That is the reason they are configuration keys rather than
constants, and the reason a wrong value is a nuisance rather than a defect.

The TTL is bounded by something outside this note. `DR-048` makes the fast store
`noeviction` ([ADR-0017](../decisions/0017-noeviction-and-duplicate-reversal.md)), so abandoned
confirmation tokens are the one namespace in the store that nothing will ever reclaim. A key with
no expiry would therefore be a slow leak in a store that is sized for a different purpose, and
`noeviction` turns a leak into a `503` rather than into a silent eviction. That is the whole reason
the default is small.
