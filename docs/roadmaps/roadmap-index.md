# Roadmap Index — implementation phases

Quotacore · 2026-09-27

The execution plan. [ROADMAP.md](../../ROADMAP.md) decides **what** ships in which release and
[mvp-scope.md](../product/mvp-scope.md) decides **what a release contains**; this directory decides
**in what order the work is done** and **what proves a piece of work is finished**. Nothing here
adds scope. A capability that is not in [mvp-scope.md](../product/mvp-scope.md) has no phase, and a
phase that is not in [mvp-scope.md](../product/mvp-scope.md) is a defect.

## 1. The documents

| Document | Contents |
| --- | --- |
| [v0-1-enforcement-path.md](v0-1-enforcement-path.md) | `IP-00`–`IP-16`: the specification gate, then the MVP |
| [v0-2-entitlements-and-rolling-windows.md](v0-2-entitlements-and-rolling-windows.md) | `IP-17`–`IP-20`: boolean entitlements, rolling windows, per-tenant keys |
| [v0-3-webhooks.md](v0-3-webhooks.md) | `IP-21`–`IP-23`: the only egress the product will ever have |
| [v0-4-admin-ui.md](v0-4-admin-ui.md) | `IP-24`–`IP-25`: the embedded operator interface |
| [v0-5-sdks.md](v0-5-sdks.md) | `IP-26`–`IP-27`: generated client libraries |

## 2. Current position

**No application code exists yet, and that is now a choice rather than a prohibition.** The
specification was the deliverable of the previous phase, [Gate 0](../../ROADMAP.md#gate-0--unblock-the-specification)
closed on 2026-09-27, and the contract questions that gated implementation are answered:
[Q-01](../../docs/product/assumptions-and-open-questions.md#2-open-questions) and
[Q-02](../../docs/product/assumptions-and-open-questions.md#2-open-questions) turned out to be
already decided, [Q-04](../../docs/product/assumptions-and-open-questions.md#2-open-questions) and
[Q-14](../../docs/product/assumptions-and-open-questions.md#2-open-questions) produced new rules and a
new route, and [Q-21](../../docs/product/assumptions-and-open-questions.md#2-open-questions) produced
[ADR-0017](../../docs/decisions/0017-noeviction-and-duplicate-reversal.md). `IP-00` is therefore
`COMPLETE`. The current phase is `IP-01`, the toolchain and CI foundation, started on 2026-09-27,
and `IP-02` follows it with the OpenAPI document that is the output of the questions `IP-00`
closed. `IP-01` closed on 2026-09-27 with all six Definition-of-Done items met, after item 4 was
amended by its owner: the item had asked for an SBOM over the Go module graph *and the base image*,
no image exists in this phase, and the base-image half moved to `IP-15`'s item 11, which now names
NFR-C3 and NFR-C4. `IP-02` and `IP-03` are therefore unblocked and both `NOT STARTED`; exactly one
phase may be `IN PROGRESS` at a time, and the next one to start is the owner's to choose.
`A-01` is a dated, owner-accepted v0.1 **release** risk, not a code-start blocker, and that is a
deliberate decision recorded in
[the register](../../docs/product/assumptions-and-open-questions.md#accepted-risks).

| Item | State |
| --- | --- |
| Specification | 63 documents, 49 rules, 17 accepted decisions, 15 named correctness tests |
| Questions blocking v0.1 | None. 8 open and 3 deferred, none of them gating; the five that needed an answer before the contract was frozen are answered |
| Phases | 28, `IP-00`–`IP-27`. `IP-00` and `IP-01` are `COMPLETE`; `IP-02` and `IP-03` are unblocked and `NOT STARTED`; the other 24 are `BLOCKED` on an earlier phase |
| Code written | `IP-01`: 19 Go, Compose, Makefile, module and CI files. No product behaviour, asserted by a test rather than claimed. `IP-00` closed the contract and wrote none; `IP-01` was the first phase to own files in the repository |
| `IP-01` open DoD items | None. Item 4 was amended before it was met: the Go module graph stays here and the base image moved to `IP-15` item 11, so the obligation has a successor rather than a deletion |

## 3. How to use this roadmap

Every phase carries a stable identifier that is never renumbered or reused, so a cross-reference
from a decision record, an issue or a commit message stays valid for the life of the project.

### Rules

1. **A phase is not complete until its Definition of Done is fully satisfied.** It compiling is not
   it being done. A DoD item that cannot fail is not a DoD item; see §5.
2. **A phase does not start while a gating decision or question is open.** Starting early means
   building on a guess, and a guess in an enforcement path is a wrong answer to a money question.
3. **Scope is derived from the specification, never from convenience.** Phases divide approved scope
   into deliverable units; they do not reduce it. Anything genuinely deferred is named under that
   phase's _Deferred items_, and a deferral is a decision, not an omission.
4. **Testing belongs to the phase that writes the code.** No phase defers its tests to a later
   phase. The named tests in [testing-strategy.md](../architecture/testing-strategy.md) are the
   minimum for the phase that owns the behaviour, not a suite for the end of the project.
5. **The three headline claims are proved, not asserted.** Each has an owner phase and a named
   measurement; see §7.

### Status legend

| Marker | Meaning |
| --- | --- |
| `NOT STARTED` | Unblocked by its dependencies, and no work begun |
| `BLOCKED` | A gating decision, question or dependency is unresolved; the blocking item is named on the phase |
| `IN PROGRESS` | Actively being built |
| `IN REVIEW` | DoD claimed complete and under review against its own items |
| `COMPLETE` | Every DoD item satisfied and evidenced, and the phase's exit criteria met |

**`IP-00` and `IP-01` are `COMPLETE`, and `IP-02` is the next phase.** `IP-00` was the phase that
closed the contract questions, and it closed them on 2026-09-27. `IP-01` was the first phase allowed
to own code files, and it closed on 2026-09-27 with all six DoD items met. Every later phase is
`BLOCKED` on an earlier phase, not on a question, and exactly one phase may be `IN PROGRESS` at a
time, so the next phase to start is set deliberately rather than by whoever looks next.

### Phase-complete checkpoint

A phase is finished when **all** of the following hold, in addition to its own DoD. The phase after
it depends on this definition being honoured rather than assumed.

| # | Criterion |
| --- | --- |
| 1 | Every DoD item is satisfied and the evidence is recorded in the phase's own section — a test name, a measurement, or a file, never "it works" |
| 2 | Every behaviour the phase introduced is reflected in every document that states the old behaviour, searched by identifier rather than by prose |
| 3 | Every error code the phase can return is in the catalogue, and every catalogue code the phase can return appears in its matrix row |
| 4 | `tools/check-docs.ps1` exits 0 |
| 5 | The change is recorded in [CHANGELOG.md](../../CHANGELOG.md) under `Unreleased` |
| 6 | No `TODO`, placeholder or stub remains in the code the phase owns |
| 7 | Any open question the phase was gated on is answered, and the answer points at the rule it produced |

## 4. Phase map

Status is `BLOCKED` until every phase in the "Depends on" chain is `COMPLETE`; `NOT STARTED` means
unblocked and untouched. "Gating" lists the decisions and
questions that must be settled first; a decision is read, not re-litigated. A question listed as
gating that has since been answered stays listed, because the phase still has to implement its
answer.

| ID | Phase | Gating | Status |
| --- | --- | --- | --- |
| `IP-00` | Close the blocking questions | — | `COMPLETE` — closed 2026-09-27 |
| `IP-01` | Repository, toolchain and CI foundation | ADR-0009, ADR-0010, ADR-0011, ADR-0014 | `COMPLETE` — closed 2026-09-27, 6 of 6 DoD items met, item 4 amended to the Go module graph with its base-image half moved to `IP-15` |
| `IP-02` | The OpenAPI contract | ADR-0010 | `NOT STARTED` — unblocked by `IP-01` |
| `IP-03` | Cycle engine and boundary matrix | ADR-0003, ADR-0007, ADR-0008 | `NOT STARTED` — unblocked by `IP-01` |
| `IP-04` | Data-plane skeleton, keyspace and snapshot cache | ADR-0001, ADR-0011 | `BLOCKED` on `IP-02`, `IP-03` |
| `IP-05` | The four atomic scripts | ADR-0002, ADR-0005 | `BLOCKED` on `IP-03`, `IP-04` |
| `IP-06` | Idempotency prevention in the data plane | ADR-0004 | `BLOCKED` on `IP-05` |
| `IP-07` | Data-plane endpoints | ADR-0001, ADR-0012 | `BLOCKED` on `IP-05`, `IP-06` |
| `IP-08` | Control-plane schema and migrations | ADR-0011, ADR-0013 | `BLOCKED` on `IP-02` |
| `IP-09` | Control-plane API and API keys | ADR-0012, ADR-0013 | `BLOCKED` on `IP-08` |
| `IP-10` | Plan lifecycle and cycle-boundary semantics | ADR-0006, `Q-01`, `Q-02`, `Q-04`, `Q-14` | `BLOCKED` on `IP-09` |
| `IP-11` | Idempotency detection and reconciliation | ADR-0004, ADR-0016, ADR-0017 | `BLOCKED` on `IP-07`, `IP-12` |
| `IP-12` | Usage ledger, writer and archiver | ADR-0016 | `BLOCKED` on `IP-08` |
| `IP-13` | Observability | — | `BLOCKED` on `IP-07` |
| `IP-14` | The `quotacore` CLI | — | `BLOCKED` on `IP-09`, `IP-12` |
| `IP-15` | Reference deployment and restore rehearsal | ADR-0009, `Q-21`, ADR-0017 | `BLOCKED` on `IP-13`, `IP-14` |
| `IP-16` | Full suite, the three claims, v0.1 exit | — | `BLOCKED` on `IP-10`, `IP-11`, `IP-15` |
| `IP-17` | Boolean entitlements | — | `BLOCKED` on `IP-16` |
| `IP-18` | `set` and `grant` as first-class operations | — | `BLOCKED` on `IP-17` |
| `IP-19` | Rolling windows | — | `BLOCKED` on `IP-05` |
| `IP-20` | Per-tenant runtime keys and cap reporting | — | `BLOCKED` on `IP-18`, `IP-19` |
| `IP-21` | Webhook configuration and signing | — | `BLOCKED` on `IP-16` |
| `IP-22` | Delivery, retries and dead letters | — | `BLOCKED` on `IP-21` |
| `IP-23` | Egress protections and delivery metrics | — | `BLOCKED` on `IP-22` |
| `IP-24` | Embedded admin shell | — | `BLOCKED` on `IP-16` |
| `IP-25` | Admin views and guarded writes | — | `BLOCKED` on `IP-24` |
| `IP-26` | TypeScript SDK | — | `BLOCKED` on `IP-16` |
| `IP-27` | Python SDK | — | `BLOCKED` on `IP-16` |

## 5. What makes a Definition of Done item valid

A DoD item is a claim a machine could refute. Three forms are accepted, and nothing else is:

| Form | Example | Why it is valid |
| --- | --- | --- |
| A named test that must pass | `T-01` passes under `-race` | The test either passes or it does not |
| A measurement with a threshold | p99 at or below the figure in NFR-L2 at the load in NFR-T1 | A number either meets the bound or it does not |
| A schema or contract constraint | A second simultaneous plan is unrepresentable, because `tenants.plan_id` is a single column with a foreign key | The DDL either permits it or it does not |

Rejected forms, and what is wrong with them:

| Rejected | Problem |
| --- | --- |
| "The endpoint works" | No failure condition is named |
| "Tests are written" | A test that asserts nothing fails this |
| "Performance is acceptable" | No threshold, no load shape |
| "Documented" | A reader cannot tell whether the document is correct |
| "Handled gracefully" | Names no behaviour a test could observe |

## 6. Dependency chain

One table, because a diagram and a table are two sources of truth and this repository has been
burned by that before. Each row is an edge; a phase is unblocked when every row naming it is
satisfied.

| From | To | Why |
| --- | --- | --- |
| `IP-00` | `IP-01` | The contract questions had to close before the contract could be written; every other phase is downstream of the foundation |
| `IP-01` | `IP-02`, `IP-03` | A contract and a pure function need a toolchain, not each other |
| `IP-02` | `IP-04`, `IP-08` | The wire contract and the schema are both derived from the specification |
| `IP-03` | `IP-04`, `IP-05` | The keyspace and the scripts both embed cycle arithmetic |
| `IP-04` | `IP-05`, `IP-19` | The scripts run in the keyspace the skeleton defines, and the rolling keys extend that keyspace |
| `IP-05` | `IP-06`, `IP-07`, `IP-19` | Idempotency is written inside the scripts; the endpoints call them; the rolling keys extend them |
| `IP-06` | `IP-07` | The endpoints enforce the header requirement |
| `IP-07` | `IP-11`, `IP-13` | Detection and observability need real requests |
| `IP-08` | `IP-09`, `IP-12` | The control API and the ledger both need tables |
| `IP-09` | `IP-10`, `IP-14` | Plan lifecycle needs the tenant surface; the CLI needs the routes |
| `IP-12` | `IP-11`, `IP-14` | Reconciliation needs the ledger; the CLI's `events` needs it |
| `IP-13` | `IP-15` | Observability is what the deployment is verified with |
| `IP-14` | `IP-15` | The quickstart is verified through the CLI, not by hand |
| `IP-10` | `IP-16` | Plan lifecycle has no other path into the exit phase |
| `IP-11` | `IP-16` | Reconciliation has no other path into the exit phase |
| `IP-15` | `IP-16` | The exit phase runs the suite the deployment makes possible |
| `IP-16` | `IP-17`, `IP-19`, `IP-21`, `IP-24`, `IP-26`, `IP-27` | Every later release starts from a shipped v0.1 |
| `IP-17` | `IP-18` | `set` and `grant` need a boolean type to be distinct from |
| `IP-18`, `IP-19` | `IP-20` | Per-tenant keys need both a stable feature type and a stable key count |
| `IP-21` | `IP-22` | Retries need a subscription to retry |
| `IP-22` | `IP-23` | Egress policy and metrics are about real deliveries |
| `IP-24` | `IP-25` | Views need the shell and its request layer |

`IP-15` needs `IP-13` and `IP-14` separately: observability and the CLI converge there, not in
sequence. `IP-16` needs `IP-10` and `IP-11` alongside `IP-15`, and those two are the only v0.1
phases with no other path into the exit phase — a plan lifecycle with no impact preview, or a
reconciliation that was never run, cannot be called a finished release.

The path runs through `IP-05`, the atomic scripts. Everything before it exists to make the scripts
possible; everything after exists to make the service that runs them trustworthy in production.
That is the same shape as the product's own centre of gravity, and it is deliberate.

### Work that can run in parallel

| Work | Phases | Condition |
| --- | --- | --- |
| Cycle engine and the OpenAPI contract | `IP-03`, `IP-02` | Both need only `IP-01`; neither needs the other |
| Control-plane schema and data-plane scripts | `IP-08`, `IP-05` | Different stores, different languages of concern; both need `IP-03` and `IP-04` at most |
| Ledger and plan lifecycle | `IP-12`, `IP-10` | Both need `IP-08`; neither reads the other's tables |
| Observability | `IP-13` | Starts once the first endpoint exists, and is finished by the last one |
| Everything downstream of v0.1 | `IP-17`–`IP-27` | All require `IP-16`, and none blocks another |

**`IP-05` and `IP-06` must not be split across teams.** `IP-06`'s idempotency record is written
inside `IP-05`'s script, and a partially migrated keyspace is a correctness defect rather than an
incomplete feature.

**`IP-19` may be built in parallel with `IP-17` and `IP-18`.** Rolling windows are a separate
mechanism from calendar cycles ([ADR-0008](../decisions/0008-cadence-model-anchored-and-rolling.md)),
deliberately, and the keyspace is designed for both now and used properly only then. What rolling
windows must not do is modify the calendar engine, which is finished and proved at `IP-03`.

## 7. The three claims and their owners

Each claim in [README.md](../../README.md) has exactly one phase that must demonstrate it, and one
measurement that does the demonstrating. A claim demonstrated in two places is a claim nobody owns.

| Claim | Owner phase | Demonstrated by |
| --- | --- | --- |
| p99 under 5 ms at 2,000 requests per second, with no database connection on the request path | `IP-16` | The warm-latency and sustained-throughput rows of [testing-strategy.md](../architecture/testing-strategy.md#5-load-and-soak), against NFR-L2 and NFR-T1; the "Postgres unreachable" chaos test proves the second half |
| A retrying client is charged once, over a 24-hour window | `IP-06` prevention, `IP-11` detection | `T-02` and `T-03` for prevention; the reconciliation cross-check for detection; NFR-D2 for the window |
| One `docker compose up` and it works, with no account and no external dependency | `IP-15` | [mvp-scope.md](../product/mvp-scope.md) §1, all eight steps, plus the quickstart executed by a script in CI |

## 8. Cross-phase requirements

These apply to **every** phase and are part of every Definition of Done, whichever phase you are
reading.

| Requirement | Source |
| --- | --- |
| No application code is written before `IP-00` completes | [AGENTS.md](../../AGENTS.md), "What not to do" |
| Every response and every log line carries a quoteable `X-Request-Id` | NFR-S10 |
| Error responses use the documented envelope, and every code comes from the catalogue | [api-conventions.md](../architecture/api-conventions.md), [error-catalog.md](../product/error-catalog.md) |
| Scripts are static, shipped in the binary, and never assembled from customer input | NFR-S8 |
| Migrations are embedded, up-only, and applied at start | [ADR-0011](../decisions/0011-postgres-only-control-plane.md) |
| A behaviour change updates every document that states the old behaviour, searched by identifier | [AGENTS.md](../../AGENTS.md), definition of done |
| No identifier is renumbered, and a retired one stays retired | [docs/README.md](../README.md#identifier-namespaces) |
| No number is introduced without a measurement or a citation | [AGENTS.md](../../AGENTS.md), writing rules |
| `tools/check-docs.ps1` exits 0 | [AGENTS.md](../../AGENTS.md), definition of done |
| The change is recorded in [CHANGELOG.md](../../CHANGELOG.md) under `Unreleased` | [AGENTS.md](../../AGENTS.md), definition of done |

## 9. Scope confirmation

| Release | In scope | Phases | Authority |
| --- | --- | --- | --- |
| v0.1 | Metered enforcement with a calendar reset | `IP-00`–`IP-16` | [mvp-scope.md](../product/mvp-scope.md) §2 |
| v0.2 | Boolean entitlements, rolling windows, per-tenant keys | `IP-17`–`IP-20` | [mvp-scope.md](../product/mvp-scope.md) §2 |
| v0.3 | Outbound webhooks | `IP-21`–`IP-23` | [mvp-scope.md](../product/mvp-scope.md) §2 |
| v0.4 | Embedded admin UI | `IP-24`–`IP-25` | [mvp-scope.md](../product/mvp-scope.md) §2 |
| v0.5 | TypeScript and Python SDKs | `IP-26`–`IP-27` | [mvp-scope.md](../product/mvp-scope.md) §2 |

The candidates in [ROADMAP.md](../../ROADMAP.md) §"Beyond" have **no phase**, deliberately. A
capability without a phase is a capability nobody has committed to, which is the correct status for
every item in that table.
