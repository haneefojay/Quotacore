# User Journeys

Ten end-to-end narratives. Each one is a sequence of moments a real person experiences, and each
carries an explicit failure mode: what goes wrong, what the product does, and what the customer
must do. Journeys are written from the customer's point of view, so the failure modes are the
parts that actually determine whether the product is trusted.

Each journey states the moment the customer either becomes confident or gives up.

---

## J-1 — An afternoon, from zero to enforcing (P-3, solo developer)

**The moment:** the first `429 quota_exceeded` returned by a real request, when the customer
expected to have plenty of headroom.

**Arc**

1. **Discovery.** The customer reads the README. The quickstart is one `docker compose up`, no
   signup, no cloud account. The deciding factor is that nothing asks for an account.
2. **First start.** Compose brings up the service, Postgres and Valkey. The service prints the
   bootstrap admin key once, at `warn`, with a note that it will not be shown again. The customer
   copies it. This is the first trust test: the key is shown once, which is mildly alarming, and
   the log says so plainly.
3. **Model the offer.** One CLI command creates the feature, one creates the plan, one adds the
   entitlement. The commands are verbose but obvious, and the plan is printed back with the exact
   cycle window the first tenant will receive.
4. **Provision.** One tenant is created with an `external_id` that is not an email address, a
   plan, and `UTC`. The response shows `cycle_start` and `cycle_end` immediately, which is the
   first moment of confidence: the customer can see the boundary rather than trusting it.
5. **Wire the call.** The customer's handler posts to `/v1/consume` with a feature key and an
   idempotency key, and branches on the result. The idempotency key is the friction point. The
   error message names the problem, the remedy, and the fact that the SDK will generate it
   automatically in v0.5. The customer reads it, decides it is reasonable, and generates a key
   from their own request identifier.
6. **Verify.** A loop of ten requests with a limit of five produces exactly five successes and
   five `429`s, and the customer's own count agrees with the balance. This is the moment the
   customer believes the product.
7. **Ship.** The handler ships. The customer has not written a single line of quota logic beyond
   the call and the branch.

**Failure mode to expect.** The customer's first instinct on seeing `429` is that something is
broken, because nothing in their application is wrong. The error envelope answers it: it names
the balance, the limit, the requested amount, and the exact instant the allowance returns. An
error that includes `cycle_end` is a self-service answer; an error that only says "quota exceeded"
generates a support ticket.

**What we would measure.** Time from first `docker compose up` to a first enforced request.
Provisioning-to-enforcement latency, because a tenant that exists but does not work is the worst
first impression available.

---

## J-2 — Protecting an AI endpoint (P-1, AI product engineer)

**The moment:** the discovery that a runaway agent loop can no longer generate a four-figure
overnight bill, without adding a new service to debug.

**Arc**

1. **Setup.** One metered feature, `llm.tokens.output`, unit `tokens`, interval `monthly`. The
   plan entitlement is a limit in tokens, which is a unit the customer's downstream provider
   already bills in. The mapping is one-to-one and takes under a minute.
2. **The call site.** The protected operation is inside the request handler, after the customer
   has computed the actual cost from the provider's usage report. The customer calls `consume`
   with the real amount, not an estimate, which removes the entire estimate-versus-actual class
   of bug.
3. **Cost discovery.** The customer first wires a `check` before the call, so the UX can show
   "you have 4,000 tokens left". Then they read that `check` is advisory and move the branch to
   the `consume` result. This transition is the most important teaching moment in the product, and
   it is why the error envelope for `429` includes the balance.
4. **Variable cost.** A long completion can cost more than the estimate. The customer calls
   `consume` with the final amount, which either succeeds in full or is denied in full. There is
   no partial deduction and no "estimated" versus "actual" reconciliation problem, because
   nothing is estimated.
5. **Overage policy.** The customer decides what a denial means: a 402 to the end user, a
   degraded response, or a queue. This is a business decision Quotacore deliberately does not
   make, and the documentation says so.
6. **Trust.** The customer watches `quotacore_consume_denied_total` alongside their own provider
   invoice and sees them line up. That correlation, once observed, is what makes the product
   sticky.

**Failure mode to expect.** A concurrent burst from one user's agent loop, all of which pass
`check` and then all of which call `consume`. Only the ones that fit are allowed. The customer's
own logging shows more `consume` calls than successes, which looks like a bug until they read
FS-01. The fix is to move the branch onto the `consume` result, which they had already been told
to do.

**What we would measure.** Share of integrations that branch on `consume` rather than gating on
`check`, and the ratio of `check` to `consume` calls, as a proxy for how well the advisory
semantics were understood.

---

## J-3 — Replacing hard-coded plan branches (P-2, backend engineer)

**The moment:** deleting the last `if plan == "pro"` branch from the codebase.

**Arc**

1. **Recognition.** The customer has four plan names in their schema and eleven branches. Every
   plan change is a migration and a release, and the last one caused an incident. This is the
   strongest pull in the whole product, and it has nothing to do with AI.
2. **Model.** Features become capabilities. The plan becomes a set of entitlements. The customer
   notices that their own `feature_flags` table is doing half of this already, badly, and that
   the two concerns have been conflated in their schema.
3. **Parallel run.** The customer enforces in both places for a while: their old code, and
   Quotacore. The comparison is not automatic, because the two systems cannot be made to agree
   exactly, and pretending otherwise would waste their time. The useful comparison is coarse:
   is a customer near their limit, yes or no.
4. **Delete the branches.** The customer's plan change becomes a row in a plan, applied at the
   next boundary. No migration. No release. The customer feels this directly, and it is the
   moment the product becomes infrastructure rather than a tool.
5. **Allocation caps.** Seats and projects are modelled as features with limit and interval,
   consumed on create and refunded on delete. The customer discovers that their delete path must
   refund, and the documentation's `refund` use case exists for exactly this.

**Failure mode to expect.** The customer's own data drifts from the counted allocation, because
their delete paths miss refunds. `GET /v1/admin/tenants/{id}/events` makes the drift visible as a
sequence of unmatched `consumed` events, and the fix is in their code, not ours. This is the
boundary of the product: Quotacore counts what it is told, and does not know what a "project" is.

**What we would measure.** Number of plan names remaining in a customer's schema after
integration. It is the clearest single indicator that the product succeeded.

---

## J-4 — Changing a price (P-2, operator)

**The moment:** raising a limit for every customer on a plan, and it taking effect without a
deploy.

**Arc**

1. **The edit.** The limit for one feature is changed on the plan. The API reports two
   consequences explicitly: new assignments get the new limit now, existing tenants get it at
   their next boundary. The response names the number of affected tenants and the earliest
   affected `cycle_end`.
2. **The reassurance.** No current balance changes. The operator can verify with one
   `GET /v1/balance` before and after.
3. **The urgent case.** A customer needs more capacity today, so `apply-now` is called with a
   reason. Every assigned tenant is re-anchored, the new limit applies immediately, and the
   audit log records before, after, actor, and reason.
4. **The one-customer case.** A single enterprise customer needs a different number, so a tenant
   override is set. It survives subsequent plan edits, because an override is a contract term
   and a plan is an offer. This precedence rule is the reason the override mechanism exists.
5. **The mistake.** The limit was raised on the wrong plan, or for the wrong feature. Because no
   balance moved, the edit is reverted with no customer impact, and the audit log shows exactly
   when the change was visible.

**Failure mode to expect.** An operator assuming `PATCH /plans/{id}` affects customers
immediately, and being pleasantly surprised. The reverse surprise, which is the dangerous one, is
an operator assuming `apply-now` is free to use casually. It re-anchors every tenant, which
discards unused allowance, so a well-meaning emergency action can quietly reset every customer's
cycle. The API therefore requires a reason, audits it separately, and the documentation says
plainly that a grant, not a forced apply, is the right tool for "give them more room".

**What we would measure.** Ratio of `apply-now` calls to `grant` calls. A high ratio means
operators are using a destructive tool for an additive intent, and the error messages should be
improved.

---

## J-5 — A customer disputes a charge (P-4, support)

**The moment:** answering "you billed me twice" from a CLI, in under a minute, with certainty.

**Arc**

1. **The complaint.** A customer says they were charged twice for the same operation, or that
   they were blocked while they had credits remaining.
2. **The lookup.** The operator lists the tenant's events, filtered by feature and time. Each
   event shows the instant, the signed delta, the resulting balance, the cycle, the source, and
   the request identifier.
3. **The arithmetic.** The deltas sum to the balance. The operator can see, in one screen,
   whether the total is right. This is the moment the product earns its keep, because it converts
   a database query and an engineer into a CLI command.
4. **The double charge.** The operator has the customer's `Idempotency-Key` — it is in the event's
   request metadata, because the API asked them to record it. Looking it up returns the recorded
   outcome: applied once, `replayed: false`, with the stored response. If the customer did send
   two distinct keys, that is visible too, and the API documented why that is the customer's
   responsibility.
5. **The correction.** A refund is applied with a new key. The balance is restored, an event is
   written, and the customer has an answer.

**Failure mode to expect.** A gap in the event history, caused by FS-10. The operator sees the
gap, sees `quotacore_event_sink_dropped_total` is non-zero, and can honestly say the record is
incomplete for that period. The alternative, which is much worse, is a customer who is right and
cannot be shown to be wrong. A visible gap is recoverable; a silent one ends the relationship.

**What we would measure.** Time to resolve a dispute, and the fraction of disputes resolved
without engineering involvement.

---

## J-6 — The dependency is down (P-2, on-call)

**The moment:** an alert fires at 03:00, and the first question — "is my revenue path safe?" —
has a one-line answer.

**Arc**

1. **The alert.** The customer's monitoring reports `503` from the enforcement path.
2. **The first question.** Is the service allowing free usage? No. Every failure is fail-closed,
   with no server-side fail-open switch, and there is no configuration to look for. A
   `quotacore_consume_total{outcome="rejected"}` and `quotacore_errors_total{code="service_unavailable"}`
   confirm it is rejections, not silent allows.
3. **The second question.** Is it our database or theirs? `/readyz` is false, and
   `GET /v1/admin/status` reports which dependency is unhealthy. The two failure modes are
   distinguishable: `503 control_plane_unavailable` means Postgres is down and only uncached
   tenants are affected, while `503 service_unavailable` means enforcement itself is down.
4. **The customer's decision.** The customer's own code chooses. Because the response is fast and
   typed, they can implement a deliberate policy — queue, degrade, or fail closed — instead of an
   accidental one inherited from a hang.
5. **The recovery.** Nothing needs to be reconciled. The request path repairs any tenant it
   touches, and the worker is optional. There is no backlog to drain, no job to re-run, and no
   customer to notify about a missed reset, because a missed boundary is not a customer-visible
   event (FS-05).

**Failure mode to expect.** Partial rather than total outage: a subset of tenants affected
because they were not in the snapshot cache. `quotacore_config_cache_miss_total` rises
first, before any `503`. That is the leading indicator, and it is documented as one.

**What we would measure.** Detection-to-mitigation time, and whether the customer's recovery
required any Quotacore-side action at all. The correct answer to the last question is "no".

---

## J-7 — A retry storm (P-1, under load)

**The moment:** a load balancer timeout, a 60-second burst of retries, and a bill that is still
correct.

**Arc**

1. **The incident.** The customer's service slows down. Their load balancer times out at 500 ms.
   Quotacore's p99 is single-digit milliseconds, so this is not Quotacore's latency — it is
   network, or their own application's contention.
2. **The storm.** Every timed-out request is retried by the client's HTTP library, several times.
   The first retry is rejected outright, `400 missing_idempotency_key`, because the customer's
   HTTP library generates a fresh key per attempt and nobody has read that section yet. The
   customer reads the 400, realises the key must be stable per logical operation rather than per
   attempt, and that realisation is the fix.
3. **The outcome.** Nothing is double-charged, and the customer learns why before it matters. Had
   the client instead generated a *fresh* key for every attempt, every attempt would be a distinct
   logical operation and the customer would be charged for each one — that is FS-03, the one place
   where the product's correctness depends on the client following an explicit instruction, and it
   is disclosed in the README, in the error catalogue and in the `consume` use case rather than
   hidden.
4. **The fix.** The customer derives the key from their own durable operation identifier, so every
   retry of the same logical operation reuses it. One line of code. After that, the storm is
   harmless: 50 attempts, one deduction, one record, 49 replays each marked `replayed: true`.
5. **The SDK shortcut.** In v0.5 this is not a line of code at all, because the SDK manages key
   lifetime across retries. This is the strongest argument for the SDK release, and it is why
   DR-026 was not relaxed to make the key optional: an optional key would make the default
   behaviour wrong.

**Failure mode to expect.** A customer who reads `Idempotency-Key` as optional because the
endpoint works without it in testing. The counter-argument in the documentation is that a test
never times out, so a test never demonstrates the problem. A single `timeout`-injection test in
their CI settles the question permanently, and the integration test recipe provides one.

**What we would measure.** Ratio of `replayed: true` responses to total consumes. High and rising
is healthy, not a problem: it means clients are retrying correctly.

---

## J-8 — An enterprise override (P-2, sales-driven)

**The moment:** signing a customer whose limits are "like Pro, but 10× the tokens".

**Arc**

1. **The request.** A deal closes with custom numbers and a hard requirement that it not require a
   code change.
2. **The override.** A tenant override is set for the token feature with a limit of ten times the
   plan's. It applies from the next cycle, and the operator can apply it immediately to the
   tenant.
3. **The isolation.** Other customers on Pro are unaffected. The override is per tenant, so there
   is no "Pro+ tier" to maintain, no schema change, and no new plan name to remember.
4. **The renewal.** In a year the base plan's limit is raised for everyone. The override is
   unaffected, because the customer's number is a negotiated contract term and was never derived
   from the plan. The operator's decision was correct at the time and stays correct.
5. **The renewal risk, stated honestly.** If the operator had instead wanted the override to track
   the plan, this is the wrong mechanism. There is no relative override, no multiplier. The
   documentation says so, in the override section, with the reason.

**Failure mode to expect.** An operator assuming a plan edit will also change overridden tenants.
It will not, and `GET /v1/balance` reporting the override's value is the fastest way to see that
it did not. The resolution order in the tenant-entitlement state machine is documented in one
place and referenced everywhere.

**What we would measure.** Share of tenants carrying an override, per plan. A rising share means
the plan structure no longer matches what is being sold, and the plan is the thing to fix.

---

## J-9 — Rolling a change across a daylight-saving boundary (P-2, in Berlin)

**The moment:** a customer asks why their monthly reset was an hour short, and there is a precise,
checkable answer.

**Arc**

1. **The configuration.** A tenant in `Europe/Berlin` on a monthly plan, anchored on the 1st at
   00:00 local. The cycle ends at local midnight, which is a wall-clock time, not a duration.
2. **March.** On the spring-forward day, 02:00 becomes 03:00. A boundary falling in the gap
   resolves forward to the first valid instant, so that cycle is one hour short. The customer's
   invoice is unaffected; the cycle is simply 719 hours instead of 720.
3. **October.** On the fall-back day, 02:00 occurs twice. The boundary resolves to the first
   occurrence, so that cycle is one hour long.
4. **The explanation.** The boundary table in the cycle-engine documentation states both rules
   with worked examples, and the test matrix asserts them. Support quotes the table. Nobody has to
   reason about time zones during an incident.
5. **The choice the customer can make.** A tenant in a DST zone who wants exactly 720 hours gets
   a `never` interval with a manual grant each month, or a `weekly` interval, or a tenant time
   zone of UTC. The documentation names this trade-off rather than hiding it.

**Failure mode to expect.** An engineer who "fixes" the boundary calculation to add fixed
durations, because that looks more correct. It is not: it drifts, and it breaks month-end
clamping, and it silently converts local-midnight semantics into an accumulating offset. DR-002
and DR-003 exist to make that refactor obviously wrong.

**What we would measure.** Reports of unexpected cycle lengths, mapped to tenants in DST zones. Any
cluster of them means the documentation is unclear, not that the engine is wrong.

---

## J-10 — Migrating in (P-2, evaluating seriously)

**The moment:** the decision to adopt, made on the evidence of the failure behaviour.

**Arc**

1. **Scepticism.** The evaluator has been burned by infrastructure that fails open, or by a
   vendor whose pricing model is the product. The first question is what happens when the
   dependency is down, and the answer is a hard `503` inside a documented budget, with no
   fail-open switch to find. That answer is worth more than it looks, because a fail-open
   dependency in a metering system is a revenue leak with an availability-shaped trigger.
2. **The test.** The evaluator runs the correctness tests before the quickstart, because the
   tests are the pitch: 200 concurrent requests against a balance of 100, exactly 100 succeed.
   A retry storm, one deduction. A missed boundary, one allowance. Nothing here is a claim; it is
   an assertion that either passes or does not.
3. **The audit.** The evaluator reads the threat model and finds the answers to the questions that
   actually matter for a self-hosted component holding billing-relevant state: keys are hashed and
   unrecoverable, no PII is accepted, the ledger is append-only, retention is bounded, and the
   service makes no outbound connection to anything.
4. **The licensing check.** Apache-2.0, permissive, with a patent grant. For a component that
   will be vendored into someone else's product, the absence of copyleft is a requirement, not a
   preference. The Redis-versus-Valkey decision is documented in full, including the licensing
   change in Redis 8, because a metering service's licence must survive a dependency upgrade.
5. **The decision.** Adopt, for the enforcement path, with the customer's own decision recorded
   about what happens on a `503`.

**Failure mode to expect.** The evaluator wants to run the MVP on Windows, where Valkey has no
native build. The documented answer is a container, or a WSL2 environment, or an embedded store.
The system requirements state this up front rather than letting it be discovered on day one.

**What we would measure.** Correctness-test runs in forks and CI, and the rate of reported
correctness defects. Both are the leading indicators of whether the central claims are being
believed and verified, or merely accepted.

---

## Cross-journey invariants

The journeys above are only consistent if these hold everywhere:

1. **No journey requires a fail-open configuration**, because none exists (DR-037).
2. **No journey requires a data-store read on the enforcement path**, because none happens
   (DR-039).
3. **No journey has a silent state change**, because every applied mutation is an event and every
   denial changes nothing (DR-025, DR-042).
4. **No journey ends with the customer guessing**, because every error names a remedy, and every
   balance response names the cycle window.
5. **No journey requires Quotacore to know what the customer's entities are**, because it never
   does (ADR-0013).
