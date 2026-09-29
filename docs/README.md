# Documentation

Every document in this repository, what it settles, and who should read it. 111 files: 63 Markdown
documents — 10 root, 9 product, 12 architecture, 17 decisions, 8 research, 6 roadmaps and this
index — plus 48 that are not documents: `LICENSE`, the two checkers, one allowlist and the
documentation generator under `tools/`, `.gitignore`, `.gitattributes`, the two files under
`.github/`, the module, Compose and Makefile files `IP-01` owns, the twenty files `IP-02` owns
under `api/` — the contract, its generator config, the generated types, codecs and validators, the
JSON and HTML renderers, the committed reference page, and the tests that hold them to the document —
and the fifteen files `IP-04` owns under `internal/` — the keyspace, the snapshot cache, the
observability package and the pool observer, with their tests.
Two tools hold the work to its own rules:
[`tools/check-docs.ps1`](../tools/check-docs.ps1) for the specification and
[`tools/gendocs`](../tools/gendocs/main.go) for the served contract.

Not sure where to start? [PROJECT.md](../PROJECT.md#reading-order) has four reading orders. This
file is the reference. If you are an agent or a contributor making changes, read
[AGENTS.md](../AGENTS.md) first.

## Root documents

| Document | What it settles | Read it if |
| --- | --- | --- |
| [../README.md](../README.md) | What the product is, in one call and one claim table | You landed here first |
| [../PROJECT.md](../PROJECT.md) | Orientation, status, the three claims, four reading orders | You are new to the project |
| [../PRD.md](../PRD.md) | Requirements, personas, metrics, and the scope boundary with reasons | You are deciding whether to adopt or to build |
| [../ARCHITECTURE.md](../ARCHITECTURE.md) | System shape, components, data model, runtime flow | You are building or reviewing the system |
| [../TECHNICAL-DECISIONS.md](../TECHNICAL-DECISIONS.md) | All 17 decisions, their rationale and revisit triggers | You want to know *why*, or you want to reopen something |
| [../SECURITY.md](../SECURITY.md) | Threat model, controls, known gaps, incident response | You are assessing risk, or deciding whether to trust it with revenue-bearing state |
| [../ROADMAP.md](../ROADMAP.md) | v0.1 → v0.5, gates, and what is deliberately late | You want to know what ships when, and what will not |
| [../AGENTS.md](../AGENTS.md) | How to work in this repository: reading order, change recipes, definition of done | You are about to change anything here |
| [../CLAUDE.md](../CLAUDE.md) | The entry point for Claude Code and other agent harnesses | Your harness looks for this file |
| [../CHANGELOG.md](../CHANGELOG.md) | What changed in the specification, and when | You want the history rather than the state |
| [../LICENSE](../LICENSE) | Apache-2.0, canonical text | You are checking the terms |

## Product — what must be true, and for whom

Read in order; the documents build on each other. Start with
[foundation](product/foundation-and-personas.md).

| Document | What it settles | Read it if |
| --- | --- | --- |
| [foundation-and-personas.md](product/foundation-and-personas.md) | The product in one page, four personas, and the permanent non-goals | You need orientation or an argument for the approach |
| [domain-rules.md](product/domain-rules.md) | **49 numbered rules (`DR-001`–`DR-049`)** — the normative behaviour of the whole system | You are implementing, testing, or reviewing behaviour |
| [mvp-scope.md](product/mvp-scope.md) | The hard v0.1 boundary, per-release exit criteria, and what is deferred | You are planning work or defending a cut |
| [use-cases.md](product/use-cases.md) | 19 use cases (`UC-01`–`UC-19`) with preconditions, triggers, flows and alternatives | You are writing tests, or designing an interface |
| [state-machines.md](product/state-machines.md) | Every state, its transitions, guards and invariants | You are implementing transitions or reasoning about an edge case |
| [error-catalog.md](product/error-catalog.md) | 34 error codes and 22 failure scenarios (`FS-01`–`FS-22`), with a client action for each | You are handling errors, or writing an SDK |
| [user-journeys.md](product/user-journeys.md) | 10 end-to-end journeys (`J-1`–`J-10`), including the sceptical evaluator | You are validating that the product solves a real problem |
| [assumptions-and-open-questions.md](product/assumptions-and-open-questions.md) | 23 assumptions (`A-01`–`A-23`) with validation methods, a dated accepted-risk table and a corrections log, and 21 questions: 10 answered, 3 deferred, 8 open | You are about to build on an unverified assumption |
| [glossary.md](product/glossary.md) | Every term, with the ones that are easy to confuse marked | Any time a word is ambiguous |

## Architecture — how it is built and proven

Read in the numbered order in [../ARCHITECTURE.md](../ARCHITECTURE.md#10-detailed-docs); it is the
build order and the review order.

| Document | What it settles | Read it if |
| --- | --- | --- |
| [overview.md](architecture/overview.md) | Component map, request paths, dependency rules | You are orienting in the system |
| [non-functional-requirements.md](architecture/non-functional-requirements.md) | Every number (`NFR-*`), and how each is measured | You are sizing, testing, or arguing a requirement |
| [data-model.md](architecture/data-model.md) | PostgreSQL DDL, the data-store keyspace, TTLs and ownership | You are writing migrations or scripts |
| [cycle-engine.md](architecture/cycle-engine.md) | The boundary algorithm, the addition rules, and a worked test matrix | You are implementing the calendar logic — the highest-risk component |
| [request-lifecycle.md](architecture/request-lifecycle.md) | The hot path step by step, with the failure mode at each step | You are implementing the API or debugging latency |
| [api-conventions.md](architecture/api-conventions.md) | Wire format, idempotency, errors, versioning, key construction | You are writing a client, a server, or a generator |
| [consistency-and-recovery.md](architecture/consistency-and-recovery.md) | Each failure mode, its behaviour, and how to recover | You are writing runbooks or an on-call rotation |
| [security-model.md](architecture/security-model.md) | Trust boundaries and controls, in depth | You are reviewing [../SECURITY.md](../SECURITY.md) and want the detail |
| [observability.md](architecture/observability.md) | Metrics, cardinality discipline, logs, and the alert table | You are instrumenting or on call |
| [deployment.md](architecture/deployment.md) | Images, configuration, upgrades, backup, restore, resource sizing | You are deploying or sizing a host |
| [performance.md](architecture/performance.md) | Budgets per stage, and the measurement plan | You are tuning or setting a capacity target |
| [testing-strategy.md](architecture/testing-strategy.md) | 15 named correctness tests, 7 further suites, and the three that prove the claims | You are writing a test plan or a CI pipeline |

## Decisions — why each choice was made

Full records in [decisions/](decisions/), indexed with rationale in
[TECHNICAL-DECISIONS.md](../TECHNICAL-DECISIONS.md). All 17 are accepted; none is superseded.

| ADR | Decision | Load-bearing? |
| --- | --- | --- |
| [0001](decisions/0001-control-plane-data-plane-split.md) | Separate the control plane from the data plane | Yes |
| [0002](decisions/0002-lua-scripts-for-atomicity.md) | Server-side Lua scripts are the atomicity primitive | Yes |
| [0003](decisions/0003-lazy-monotonic-cycle-rollover.md) | Cycle rollover is lazy, monotonic, and computed in the application | Yes |
| [0004](decisions/0004-idempotency-prevention-and-detection.md) | Idempotency: prevention in the data plane, detection in the control plane | Yes |
| [0005](decisions/0005-refund-as-first-class-operation.md) | Refund is a first-class operation with a hard invariant | Yes |
| [0006](decisions/0006-plan-edits-at-next-cycle-boundary.md) | Plan edits take effect at the next cycle boundary by default | |
| [0007](decisions/0007-per-tenant-timezone.md) | Per-tenant IANA time zone, with UTC as the default | |
| [0008](decisions/0008-cadence-model-anchored-and-rolling.md) | One cadence model: anchored calendar cycles, rolling windows separate | |
| [0009](decisions/0009-redis-protocol-valkey-default.md) | Speak the Redis protocol, ship Valkey by default | |
| [0010](decisions/0010-go-chi-spec-first-openapi.md) | Go, `net/http` with chi, and a spec-first OpenAPI contract | |
| [0011](decisions/0011-postgres-only-control-plane.md) | The control plane is PostgreSQL only, with embedded up-only migrations | |
| [0012](decisions/0012-hashed-scoped-api-keys.md) | Hashed, scoped, rotatable API keys, with an environment bootstrap key | |
| [0013](decisions/0013-single-tenant-flat-model-no-pii.md) | Single-tenant instance, flat tenants, opaque identifiers, no PII | |
| [0014](decisions/0014-apache-2-0-license.md) | Apache License 2.0 | |
| [0015](decisions/0015-release-slicing.md) | A hard MVP boundary, with the rest sequenced behind it | |
| [0016](decisions/0016-usage-event-ledger.md) | The usage event log is a signed-delta ledger, and it is the recovery source | Yes |
| [0017](decisions/0017-noeviction-and-duplicate-reversal.md) | No eviction inside the 24-hour window, and an automatic reversal for what no eviction cannot prevent | Yes |

## Research — what was investigated before deciding

Evidence, with dates and sources, so a future reader can tell a re-check from a fact.

| Document | What it establishes | Read it if |
| --- | --- | --- |
| [atomicity-mechanism-options.md](research/atomicity-mechanism-options.md) | Why Lua rather than `MULTI/EXEC`, a lock, or optimistic concurrency | You are questioning the atomicity primitive |
| [reset-cycle-semantics.md](research/reset-cycle-semantics.md) | Calendar versus relative versus sliding, and the edge cases that separate them | You are implementing the cycle engine |
| [licensing-redis-vs-valkey.md](research/licensing-redis-vs-valkey.md) | The Redis licence change, why Valkey is the default, and when to re-check | You are choosing an image or shipping commercially |
| [metering-ledger-patterns.md](research/metering-ledger-patterns.md) | Signed deltas, exactly-once illusion, the four-way reconciliation | You are designing the ledger or the replica |
| [competitive-landscape.md](research/competitive-landscape.md) | What Stripe Entitlements, Lago, OpenMeter, and open-source attempts do and omit | You are positioning, or being asked "how is this different" |
| [go-stack-and-dependency-selection.md](research/go-stack-and-dependency-selection.md) | Go 1.27, chi, pgx, `go-redis`, embedded migrations, OpenAPI tooling | You are setting up the repository |
| [idempotency-and-retries.md](research/idempotency-and-retries.md) | Keys, fingerprints, TTLs, and the exact-once illusion, with the `iPhone` case | You are implementing retries or a client SDK |
| [a-01-design-partner-validation.md](research/a-01-design-partner-validation.md) | The interview script that closes or kills A-01, and the sources for the two configuration defaults | You are about to build on an untested market claim, or you are asking where a default number came from |

## Roadmaps — in what order the work is done

Execution plan, in [roadmaps/](roadmaps/). [ROADMAP.md](../ROADMAP.md) decides what ships in which
release and [mvp-scope.md](product/mvp-scope.md) decides what a release contains; these decide the
order of the work and what proves a piece of it finished. [Gate 0](../ROADMAP.md#gate-0--unblock-the-specification)
is closed: `IP-00` to `IP-04` are `COMPLETE`; the other 23 wait on their dependencies, with `IP-05`
(the four atomic scripts) the next permitted.

| Document | What it settles | Read it if |
| --- | --- | --- |
| [roadmaps/roadmap-index.md](roadmaps/roadmap-index.md) | The status legend, the phase map, the dependency chain, and what makes a definition-of-done item valid | You are starting any piece of work and need to know what order it goes in |
| [roadmaps/v0-1-enforcement-path.md](roadmaps/v0-1-enforcement-path.md) | 17 phases, `IP-00`-`IP-16`, from the specification gate to the three claims | You are building the MVP |
| [roadmaps/v0-2-entitlements-and-rolling-windows.md](roadmaps/v0-2-entitlements-and-rolling-windows.md) | 4 phases, `IP-17`-`IP-20`: boolean entitlements, rolling windows, per-tenant keys | You are building v0.2 |
| [roadmaps/v0-3-webhooks.md](roadmaps/v0-3-webhooks.md) | 3 phases, `IP-21`-`IP-23`, and what the first egress changes | You are building v0.3 |
| [roadmaps/v0-4-admin-ui.md](roadmaps/v0-4-admin-ui.md) | 2 phases, `IP-24`-`IP-25`, and why the interface is a client rather than a privileged path | You are building v0.4 |
| [roadmaps/v0-5-sdks.md](roadmaps/v0-5-sdks.md) | 2 phases, `IP-26`-`IP-27`, and the retry pattern the API requires | You are building v0.5 |

## Inventory

These numbers are not maintained by hand. `tools/check-docs.ps1` recomputes each one from the
documents themselves and fails if this table disagrees, so a stale count cannot survive a review.

| Item | Count | Defined in |
| --- | --- | --- |
| Rules | 49 | [product/domain-rules.md](product/domain-rules.md) |
| Use cases | 19 | [product/use-cases.md](product/use-cases.md) |
| Error codes | 34 | [product/error-catalog.md](product/error-catalog.md) |
| Failure scenarios | 22 | [product/error-catalog.md](product/error-catalog.md) |
| Assumptions | 23 | [product/assumptions-and-open-questions.md](product/assumptions-and-open-questions.md) |
| Open questions | 21 | [product/assumptions-and-open-questions.md](product/assumptions-and-open-questions.md) |
| Journeys | 10 | [product/user-journeys.md](product/user-journeys.md) |
| Non-functional requirements | 63 | [architecture/non-functional-requirements.md](architecture/non-functional-requirements.md) |
| Invariants | 43 | [product/state-machines.md](product/state-machines.md), [architecture/data-model.md](architecture/data-model.md) |
| Named tests | 15 | [architecture/testing-strategy.md](architecture/testing-strategy.md) |
| Decisions | 17 | [decisions/](decisions/) |
| Markdown documents | 63 | repository root |
| Files in the repository | 134 | repository root, excluding the paths `.gitignore` marks as build output (15 files added by `IP-04`: the keyspace, the snapshot cache, the observability package and the pool observer, with their tests) |

## Identifier namespaces

Every reference in the specification uses one of these prefixes. They are stable, never reused, and
never renumbered. `tools/check-docs.ps1` fails if a reference points at an identifier that does not
exist.

| Prefix | Means | Defined in | Shape |
| --- | --- | --- | --- |
| `DR-nnn` | A normative rule | [product/domain-rules.md](product/domain-rules.md) | `DR-001` |
| `UC-nn` | A use case | [product/use-cases.md](product/use-cases.md) | `UC-01` |
| `FS-nn` | A failure scenario | [product/error-catalog.md](product/error-catalog.md) | `FS-01` |
| `A-nn` | An assumption | [product/assumptions-and-open-questions.md](product/assumptions-and-open-questions.md) | `A-01` |
| `Q-nn` | An open question | [product/assumptions-and-open-questions.md](product/assumptions-and-open-questions.md) | `Q-01` |
| `J-n` | A user journey | [product/user-journeys.md](product/user-journeys.md) | `J-1` |
| `NFR-XXn` | A non-functional requirement | [architecture/non-functional-requirements.md](architecture/non-functional-requirements.md) | `NFR-A1`, `NFR-S10` |
| `INV-XXn` | A state invariant | [product/state-machines.md](product/state-machines.md), [architecture/data-model.md](architecture/data-model.md) | `INV-C1`, `INV-X8` |
| `T-nn` | A named correctness test | [architecture/testing-strategy.md](architecture/testing-strategy.md) | `T-01` |
| `ADR-nnnn` | An architecture decision record | [decisions/](decisions/) | `ADR-0001` |
| `IP-nn` | An implementation phase | [roadmaps/roadmap-index.md](roadmaps/roadmap-index.md) | `IP-00` |
| `C-n`, `K-n`, `P-n`, `F-n`, `E-n`, `T-n` | A state transition, prefixed by its entity | [product/state-machines.md](product/state-machines.md) | `C-2`, `K-3`, `T-1` |

Note the deliberate asymmetries, because they have bitten before: a **two-digit** `T-01` is a test, a
**single-digit** `T-1` is a tenant-state transition. They are different namespaces that share a
letter. Do not "fix" one into the other. An `IP-nn` is a phase and nothing else, and it is
deliberately *not* shaped like a state transition, so a phase identifier can never be mistaken for
an edge.

## Conventions

- One topic per file, kebab-case filenames, one H1.
- Decision records are immutable once accepted. Supersede, never edit.
- Link with relative paths. Link back to the root document that spawned the document.
- Date anything that describes a point-in-time state, especially licensing and pricing.
- Tables over prose for anything enumerable: codes, rules, metrics, fields, limits.
- British spelling, hyphenated compound adjectives (`cycle-boundary edit`), no Oxford comma in
  short lists.
- State a limit, a number or a deadline only where a source supports it, and cite the source in
  [research/](research/). An unsourced number is a defect, not a placeholder.

## Consistency rules

These are the rules the specification is held to. Each one is checked by
[`tools/check-docs.ps1`](../tools/check-docs.ps1); the numbered rules below are the reason the
checks exist, not a description of them.

1. **No orphans.** Every document is listed in the index above, every link resolves — path *and*
   anchor — and every identifier reference resolves to a definition.
2. **Behaviour is stated once.** A rule lives in [domain-rules.md](product/domain-rules.md) with a
   stable `DR-nnn` identifier. Other documents reference it. If two documents disagree, the rule
   document wins and the other is wrong.
3. **One error vocabulary.** 34 codes, defined once in
   [error-catalog.md](product/error-catalog.md). The catalogue is the only list: the
   `code` label set of `quotacore_errors_total` is the catalogue by reference rather than a second
   copy, and [api-conventions.md](architecture/api-conventions.md) defines the envelope rather than
   restating the codes. The catalogue's own endpoint matrix maps every code to the endpoints that
   can return it, and both directions of that mapping are checked, so a code no endpoint can return
   and a code the matrix names but the catalogue does not define are both defects. An undocumented
   code does not exist.
4. **Every number has a measurement.** A requirement without a method for verifying it is an
   aspiration. `NFR-*` items name their measurement, and the inventory above is recomputed rather
   than asserted.
5. **Every claim says how it is verified.** Every rule in
   [domain-rules.md](product/domain-rules.md) carries a `*Verified by:*` line naming a method that
   exists: a named test `T-nn`, an `NFR-*` measurement, a named artefact in
   [testing-strategy.md](architecture/testing-strategy.md), or the constraint in
   [data-model.md](architecture/data-model.md) where the schema is itself the proof. A rule with no
   such line is unfinished, and the checker says so.
6. **Release boundaries are identical everywhere.** If a capability appears in the roadmap, it
   appears in [mvp-scope.md](product/mvp-scope.md) with the same version, and is absent from the v0.1
   list.
7. **Identifiers are stable.** `DR-`, `UC-`, `FS-`, `A-`, `Q-`, `J-`, `NFR-`, `INV-`, `T-`, `ADR-`
   prefixes are never reused or renumbered. Retiring one is a deletion, not a recycle.
8. **Sharp edges are documented, not omitted.** A design limitation belongs in
   [SECURITY.md](../SECURITY.md#6-known-gaps), [deployment.md](architecture/deployment.md) or the
   relevant `A-`/`Q-` entry — not in an absence.

## Audit status

The specification is internally consistent as of 2026-09-27. `tools/check-docs.ps1` passes: every
link and anchor resolves, every identifier reference resolves, no identifier is defined twice, the
inventory above matches the documents, and the semantic checks find no contradiction between
documents — the error catalogue and its endpoint matrix agree in both directions, every rule states
how it is verified, every release in the roadmap is named in the scope boundary, every research note
is dated and sourced, every supersession points at a record that agrees, every question status comes
from the register's vocabulary, and no application code exists while a blocking question is open.
What the check cannot judge — whether a rule is *true*, whether a design trade-off is *right* — is a
review question, and is tracked in
[the open-questions register](product/assumptions-and-open-questions.md#2-open-questions) and in
[ROADMAP.md](../ROADMAP.md#gate-0--unblock-the-specification). Those are deliberately different
lists: an audit finding is a defect, an open question is a decision that has not been made yet.
