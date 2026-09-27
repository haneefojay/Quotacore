# Assumptions and Open Questions

The register of everything this specification is betting on, and everything it has not decided.
Two tables, deliberately kept separate:

- **Assumptions** are things believed to be true. Each has a verification method and an impact if
  it turns out to be false. An assumption with no verification method is a wish.
- **Open questions** are things not yet decided. Each has a proposed default, a decision deadline,
  and a marker for whether it blocks implementation.

An open question with a proposed default is safe: the team can build while the question is
outstanding, because the default is written down and the code path is isolated.

Status values: `assumed` · `verifying` · `confirmed` · `falsified` · `open` · `deferred`

---

## 1. Assumptions

### Commercial and market

| ID | Assumption | Status | How it is verified | If false |
| --- | --- | --- | --- | --- |
| A-01 | Metered quota enforcement, as distinct from boolean entitlements and infrastructure rate limiting, is a durable standalone purchase | assumed | Five design-partner conversations specifically about the *enforcement* job, not about billing | The product must reposition toward a component of a larger stack, or add the missing adjacent surface first |
| A-02 | Target users accept a network hop to a sidecar on the protected path | assumed | p99 measured in the first three integrations; any requirement for in-process evaluation is a signal | The deployment model changes to a library or an in-process engine, which is a different product |
| A-03 | Requiring an idempotency key is acceptable friction | assumed | Observe `missing_idempotency_key` rate during the MVP pilot | Fallback is SDK-generated keys in v0.5. **Not** making the key optional, because then the default behaviour is wrong (J-7) |
| A-04 | Customers accept running one Postgres and one Redis-compatible store | assumed | Count deployment refusals in the pilot | The embedded single-binary alternative in ADR-0011 becomes worth building |
| A-05 | A self-hosted, no-account product is preferred over a hosted tier by at least the initial cohort | assumed | Interviews with evaluators who reached the licensing or self-hosting step | A hosted offering may be needed, which introduces availability commitments the current design does not make |
| A-06 | Apache-2.0 is acceptable for a component that will be vendored into someone else's product | assumed | A `go-licenses` scan of the shipped image showing no copyleft obligation, plus written confirmation from the first two design partners' legal reviewers | A change of licence would require re-consenting every user |
| A-07 | The competitive space is crowded enough that "nobody has built this" is not a viable pitch | confirmed | [competitive-landscape](../research/competitive-landscape.md) | — |

### Technical

| ID | Assumption | Status | How it is verified | If false |
| --- | --- | --- | --- | --- |
| A-08 | A single Lua script execution can hold everything required for correctness: idempotency, rollover, ceiling check, deduction | confirmed | The script exists and the contention test passes | Correctness must move to a compare-and-set loop with an explicit lock, which changes the latency model and the failure modes |
| A-09 | `go-redis/v9` supports `EVALSHA` on Redis, Valkey and KeyDB without a compatibility shim | verifying | Integration test against each engine before v0.1 exits the pilot | A shim, or dropping support for an engine, documented explicitly |
| A-10 | A bounded in-process snapshot plus pub/sub invalidation keeps configuration propagation within a stated bound | assumed | Measure observed propagation delay; assert the documented bound | The bound must be tightened with a version check on the enforcement path, which reintroduces a read |
| A-11 | Redis Cluster hash tags give per-tenant atomicity without cross-slot transactions | confirmed | Key layout in ADR-0002 | Multi-slot transaction handling, which is a significant complexity increase |
| A-12 | An append-only event ledger is a sufficient recovery mechanism for a full allowance loss | assumed | Restore drill: wipe Redis entirely, rebuild balances from events, reconcile | A write-through path to Postgres becomes mandatory on the hot path, which contradicts ADR-0001 |
| A-13 | Postgres 16 is a sufficient minimum version | assumed | Confirm no reliance on features newer than 16 | Raise the minimum and document it |
| A-14 | Go 1.27 is available and the chosen libraries support it | confirmed | `go build` and CI on 1.27 | Pin to the previous minor |
| A-15 | UTC-pinned host clock calculations with per-tenant IANA zones are correct for all supported zones | verifying | Boundary test table, including DST gaps and repeats, and a full sweep over `zoneinfo` | Restrict the supported zone list, which is a visible product limitation |
| A-16 | Best-effort event delivery with a visible drop metric is acceptable to operators | assumed | Ask operators directly during the pilot; show them the metric | Move to synchronous writes with a stated latency cost, which changes the performance target |

### Operational

| ID | Assumption | Status | How it is verified | If false |
| --- | --- | --- | --- | --- |
| A-17 | Operators can run NTP and keep the host clock correct | assumed | Document it as a hard requirement; alert on clock skew | Detection of skew becomes the mitigation, since we cannot correct the host clock |
| A-18 | A single region is acceptable | assumed | Ask about data-residency requirements during evaluation | Regional data planes with a control plane per region, a large change |
| A-19 | Backup and restore of Postgres plus the data store is a customer responsibility, with the procedure documented | assumed | Restore drill performed by someone who did not write the procedure | A managed backup story is needed, which is an operations product |
| A-20 | Six reset intervals cover the MVP cohort | assumed | Count interval requests during the pilot | Add intervals in a minor release; the cycle engine is already table-driven |

### Usability

| ID | Assumption | Status | How it is verified | If false |
| --- | --- | --- | --- | --- |
| A-21 | Including `cycle_end` in every balance and denial response is enough to make quota denials self-explanatory | assumed | Count support questions of the form "why was I blocked?" | Add a `GET /v1/balance` deep link or a richer error body |
| A-22 | A CLI is an acceptable operator interface for the MVP, and a web UI is not required to adopt | assumed | Five pilot operators using only the CLI | UI moves earlier, at the cost of the webhook release |
| A-23 | The CLI is discoverable enough to configure a plan from documentation alone | verifying | Time-on-task for a first-time operator, unassisted | Interactive prompts or a `quickstart` command |

### Accepted risks

An assumption may be carried into a release with its status unchanged, provided somebody with
authority to accept the consequence says so in writing and the acceptance is dated. An undated
acceptance is indistinguishable from an oversight, and an oversight is the most common way a bet
becomes a fact nobody chose.

| Assumption | Risk carried | Accepted by | Accepted on | Re-check by | Becomes a blocker if |
| --- | --- | --- | --- | --- | --- |
| [A-01](#1-assumptions) | The whole product rests on a market claim that is untested. If metered enforcement is not a durable standalone purchase, the positioning is wrong and the engineering is still correct | Project owner | 2026-09-27 | 2026-12-31, or after the first three of the five conversations in [a-01-design-partner-validation.md](../research/a-01-design-partner-validation.md) | Fewer than three of the first five design partners describe enforcement as a problem they currently solve, in their own words and without prompting |

Accepting A-01 does not make the bet more likely to be true. It makes it visible, owned and dated, so
that the decision to keep building on it was made rather than discovered.

---

## 2. Open questions

`Blocks` means implementation of the affected component cannot start. Everything else has a
proposed default, so work continues.

**All four former blockers are now answered**, and two of them turned out to be already decided
rather than merely defaulted. That distinction is worth keeping, because a question that was
answered by an existing rule needed no new decision and no new risk.

| Former blocker | How it was resolved | Where the answer lives |
| --- | --- | --- |
| [Q-01](#2-open-questions) | **Already decided.** A grant is cycle-scoped and a plan change starts a new cycle, so `bonus` is zero in it. `UC-06` additionally states that an *override* does survive, so the grant/override split was a deliberate choice rather than an oversight | [DR-013](domain-rules.md), [DR-020](domain-rules.md), [INV-C5](state-machines.md), [UC-06](use-cases.md) |
| [Q-02](#2-open-questions) | **Already decided.** The previous balance is discarded, not carried over, and proration is deferred. Stated twice, in the rule and in the use case | [DR-013](domain-rules.md), [UC-06](use-cases.md) |
| [Q-04](#2-open-questions) | New decision. `apply-now` above a configurable threshold returns `409 confirmation_required` and applies nothing; the token is single-use and bound to the plan version | [DR-046](domain-rules.md), [FS-19](error-catalog.md), [T-12](../architecture/testing-strategy.md) |
| [Q-14](#2-open-questions) | New decision. A read-only impact route reports the affected count and the earliest affected `cycle_end` | [UC-19](use-cases.md), [FS-19](error-catalog.md), [10.2](../architecture/api-conventions.md) |

**[Q-21](#2-open-questions) was a fifth blocker in substance, though it was filed as a v0.1-release
question.** It is now answered by [ADR-0017](../decisions/0017-noeviction-and-duplicate-reversal.md),
and the resolution changed more than the register: the second claim is now worded as "charged once,
and a double charge caused by store loss is reversed", because the 24-hour window cannot survive
total store loss and the specification says so rather than implying otherwise.

**Eleven remain open and none of them block.** Each has a proposed default that a reasonable person
would accept on sight, and the risk of shipping the default and correcting it later is smaller than
the cost of stalling the first endpoint. Five of the eleven had to be settled before the OpenAPI
document is frozen, and they are answered: [Q-05](#2-open-questions),
[Q-06](#2-open-questions), [Q-07](#2-open-questions), [Q-10](#2-open-questions) and
[Q-16](#2-open-questions). Four of those five were also already decided elsewhere, in the request
schema, the endpoint matrix or an invariant; only Q-16 needed new text.

| ID | Question | Proposed default | Deadline | Blocks | Status |
| --- | --- | --- | --- | --- | --- |
| Q-01 | Does an admin `grant` survive a plan change? | **No.** A grant is cycle-scoped and a plan change starts a new cycle (DR-013, DR-020). Customers told "we gave you 10,000 extra" mean it for the current cycle | Before the admin API is frozen | `POST /admin/tenants/{id}/grant` | answered — DR-013, DR-020 |
| Q-02 | Is the balance after a plan change the new limit, or the new limit plus remaining old balance? | **New limit only.** Proration and carry-over are billing semantics and are explicitly out of scope | Before the admin API is frozen | UC-06 | answered — DR-013, UC-06 |
| Q-03 | Should a tenant override be removable by the customer, or only by an operator? | **Operator only** in the MVP. Customer-facing control requires authentication and an identity model the product does not have | v0.4 | UC-13 | open |
| Q-04 | Does `apply-now` require a confirmation token when more than N tenants are affected? | **Yes**, for more than 50, return a `409 confirmation_required` with a token. Re-anchoring many tenants at once is destructive and a bulk accident is plausible | Before v0.1 release | `POST /admin/plans/{id}/apply` | answered — DR-046, T-12 |
| Q-05 | Should the MVP support multiple features per request, for example a bundle of token and request costs? | **No.** One feature per call, and a bundle is two calls. Bundling breaks the per-feature event model and the idempotency fingerprint | Before v0.1 release | `POST /v1/consume` | answered — api-conventions 7.1 |
| Q-06 | Should `check` be a `POST` or a `GET`? | **`POST`**, because it carries an amount and a window, and `GET` with a body is not portable. Naming it `check` rather than `quote` because it does not reserve anything | Before the OpenAPI is frozen | `POST /v1/check` | answered — api-conventions 7.2 |
| Q-07 | Is an idempotency key required on `check`? | **No.** `check` mutates nothing, so a duplicate is harmless (DR-024) | Before the OpenAPI is frozen | `POST /v1/check` | answered — api-conventions 7.2 |
| Q-08 | What is the exact snapshot-propagation bound to document? | **1 second, p99**, measured, with the observed distribution published | Before v0.1 release | A-10, [api-conventions](../architecture/api-conventions.md) | open |
| Q-09 | Do event and audit retention have different defaults? | **Yes.** Events 90 days, audit 400 days. Audit is the contractual record; events are operational history | Before the first migration ships | `events-retention` config | open |
| Q-10 | Should `GET /v1/balance` return every feature for a tenant, or require a feature key? | **Optional `feature_key`.** Without it, return the full set; with it, one entry. The full set is a small bounded number per plan | Before the OpenAPI is frozen | `GET /v1/balance` | answered — api-conventions 7.3 |
| Q-11 | Do we accept a numeric `window` for rolling, or a string enum? | **String enum** (`rolling_1h`), because a numeric duration invites unit mistakes and a 32-bit overflow | v0.2 | UC-17 | deferred |
| Q-12 | What is the official support commitment? | **Community only**, explicitly. A support SLA would be a business change, not a technical one | Before the README claims anything | — | open |
| Q-13 | Should the reset worker be enabled by default? | **Yes**, at a 60 s interval, because it makes `cycle_end` accurate in the admin views even when a tenant is idle. Correctness does not depend on it | Before v0.1 release | worker config | open |
| Q-14 | Do we need an admin API to *preview* the effect of a plan edit before committing it? | **Yes**, and cheaply: `GET /v1/admin/plans/{id}/impact` returning affected tenant count and the earliest affected `cycle_end`. This is the operator's main anxiety and the API should answer it | Before v0.1 release | `POST /admin/plans/{id}/apply` | answered — UC-19, 10.2 |
| Q-15 | Is a per-tenant rate limit needed to protect the service from one noisy customer? | **Yes**, per API key, with `rate_limited` distinct from `quota_exceeded` (DR-040). Default generous; configurable | Before v0.1 release | `rate_limited` | open |
| Q-16 | What is the behaviour when a tenant is deleted while requests are in flight? | **Deny after the delete commits.** In-flight requests that already entered the script before the delete may complete; the deletion is not retroactive and the audit log shows both | Before the OpenAPI is frozen | `DELETE /admin/tenants/{id}` | answered — DR-047 |
| Q-17 | Should quota reservations be built? | **No.** `consume` the estimate, `refund` the difference. Reservations are a real feature and a genuine source of complexity | Post-v0.1 | — | deferred |
| Q-18 | Do we need a bulk tenant import for onboarding a large customer? | **No** in the MVP; CSV import only if a pilot customer cannot onboard without it | Post-v0.1 | — | deferred |
| Q-19 | Should the data plane be able to run without the control plane entirely, for a hard-isolation deployment? | **No.** The two planes are one product; splitting them creates a stale-configuration failure mode nobody asked for | Not scheduled | — | open |
| Q-20 | What is the migration story for the event ledger if a customer has a compliance retention requirement? | **Documented export.** A documented query plus a documented deletion job. A full export pipeline is a v0.5+ candidate, not an MVP feature | Before v0.1 release | `events-retention` config | open |
| Q-21 | May the data store evict an idempotency record inside the 24-hour window, and still claim "a retrying client is charged once"? | **No, the window is a contract, and the store is sized to keep it.** `maxmemory-policy noeviction` on the whole fast-store keyspace, sized for the 24-hour window at the sustained-throughput figure in NFR-T1. Capacity exhaustion is a `503`, not a silent eviction. What `noeviction` cannot survive is *total* store loss, and that is answered by an automatic reversal of the duplicate once it is detected | Before v0.1 release | DR-029, NFR-T1, NFR-T9, [deployment](../architecture/deployment.md#5-sizing) | answered — ADR-0017 |
---

## 3. How this register is maintained

1. Any assumption that is falsified during implementation gets its status changed and a new
   assumption added for whatever replaced it. The original text is not edited, because the record
   of what was believed and when is itself evidence.
2. An open question is resolved by writing the decision into the relevant document and
   cross-referencing the question ID there, so a reader can see that a question was considered
   rather than overlooked.
3. A question marked `deferred` is not a pending decision. It is a deliberate non-decision, with a
   release in which it may be revisited.
4. Any new assumption added during implementation must state its verification method. An
   unverifiable assumption is removed rather than kept, because an unverifiable assumption is
   indistinguishable from a guess and is treated as a fact by everyone who reads it.
5. A question marked `answered` names the rule, route or ADR that answers it. The `Proposed default`
   column is left showing what the default was *before* the question was closed, so that a reader can
   see whether the decision matched the default. Where it did not, the cell is rewritten and the
   difference is recorded below.
6. `confirmed` means the verification method has been *run* and returned the stated result. It does
   not mean the method is a good one, or that it has been scheduled. A method that describes an
   activity nobody has performed is a plan, and a plan is `verifying` at best.

### Corrections

| Date | Entry | Correction |
| --- | --- | --- |
| 2026-09-27 | A-06, A-22 | Both were marked `confirmed` while their verification methods were five conversations and five pilot sessions that have not happened. Reverted to `assumed`. `confirmed` is a claim about evidence, not about intent |
| 2026-09-27 | A-06 method | Its verification method was A-01's method — five design-partner conversations about the *enforcement* job. That measures the market, not the licence, so the assumption was being verified by evidence about a different question. Replaced with a licence scan of the shipped image and written confirmation from the first two design partners' legal reviewers, which is what a licence question actually needs |
| 2026-09-27 | A-01 | Left `assumed`, and carried into v0.1 as a dated, owner-accepted risk rather than a code-start blocker. See [Accepted risks](#accepted-risks) |
| 2026-09-27 | Q-01, Q-02, Q-05, Q-06, Q-07, Q-10 | Closed as already decided. Each was answered by an existing rule, request schema or invariant, and the question only recorded that nobody had looked. The cross-references are now in the deciding documents |
| 2026-09-27 | Q-04, Q-14, Q-16 | Closed with new text. Each produced a rule, a use case or a route, and each is verifiable by a named test |
| 2026-09-27 | Q-21 | Closed by ADR-0017. The proposed default offered a choice between the window and the throughput claim; the decision took the window and raised the store's memory floor, and it also closed the store-loss hole the question did not mention |
| 2026-09-27 | Q-21 citation | The question cited NFR-C1, which is the Apache-2.0 compliance requirement. The throughput figure is NFR-T1. Corrected here and wherever else it appeared |
