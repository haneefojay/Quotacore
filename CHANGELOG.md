# Changelog

The specification is versioned separately from the product. Nothing here describes shipped software:
the first release of Quotacore has not been built, and [PROJECT.md](PROJECT.md) explains why.

Dates are ISO-8601. Entries follow [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
specification uses [semantic versioning](https://semver.org/spec/v2.0.0.html) on documents rather
than on code. A change that alters existing behaviour is recorded as `Changed` even when no code
exists, because a future implementer needs to know which statements are new.

## [Unreleased]

### Added

- `DR-046` — a `409 confirmation_required` with a single-use, plan-version-bound token above
  `QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD`, and `apply-now` applies nothing while the token is
  outstanding. T-12 proves it.
- `DR-047` — tenant deletion is soft, is not retroactive, and does not cancel an in-flight
  deduction; the remedy for an unwanted charge is `POST /v1/refund`. T-15 proves it. This answers
  `Q-16`.
- `DR-048` and `DR-049` — the no-eviction policy and the automatic reversal of a duplicate detected
  after total store loss. T-13 and T-14 prove them.
- `GET /v1/admin/plans/{id}/impact`, a read-only preview of a plan edit's blast radius, with
  `UC-19` and its matrix row. This answers `Q-14`, and it is a v0.1 capability in both
  [mvp-scope.md](docs/product/mvp-scope.md) and [ROADMAP.md](ROADMAP.md).
- `confirmation_required` (409) in [error-catalog.md](docs/product/error-catalog.md), with a client
  action and a retry verdict, and `FS-19` to `FS-22` for the confirmation, impact-preview,
  delete-in-flight and store-loss cases.
- `NFR-T9` and `NFR-T10`, which make the retained idempotency working set and the behaviour of the
  window across a process restart into measurements rather than prose.
- [ADR-0017](docs/decisions/0017-noeviction-and-duplicate-reversal.md), answering `Q-21`, with the
  `noeviction` option, the automatic-reversal option, and the two claims that trade against each
  other named as such.
- [a-01-design-partner-validation.md](docs/research/a-01-design-partner-validation.md): the
  interview script for `A-01`, with pass criteria registered **before** the answers exist, a
  falsification rule, and the sources for the two operator-workflow configuration defaults, which are
  labelled as judgements rather than measurements.
- A `P-3a` to `P-3c` confirmation arm on the plan state machine, an `I-5`/`I-6` deletion arm on the
  tenant state machine, and four new invariants.
- `QUOTACORE_IDEMPOTENCY_TTL` is now declared in the deployment configuration table, as a key that is
  pinned and refused at startup rather than tunable. The rules and the phases referred to it before
  it was listed.
- The execution plan, in [docs/roadmaps/](docs/roadmaps/roadmap-index.md): 28 implementation phases,
  `IP-00` to `IP-27`, across five release documents. Each phase names its objective, scope,
  specification references by identifier, dependencies in both directions, and a Definition of Done
  whose items are a named test, a measurement with a threshold, or a schema constraint — the only
  three forms that can fail. The index adds a status legend, a phase-complete checkpoint, a
  cross-phase requirements table, the dependency chain with the work that can run in parallel, and
  the three headline claims with the one phase and one measurement that owns each.
- The `IP-nn` identifier namespace, in [docs/README.md](docs/README.md#identifier-namespaces) and
  [AGENTS.md](AGENTS.md). It is deliberately not shaped like a state transition, so a phase
  identifier can never be mistaken for an edge in [state-machines.md](docs/product/state-machines.md).
- [ROADMAP.md](ROADMAP.md) now states, once, that it is release slicing and that the order of the
  work inside a release lives in the phase documents, with a table of which phases belong to which
  release. The roadmap and the phases are different things, and a phase changing does not change a
  release boundary.
- Nine semantic checks in [tools/check-docs.ps1](tools/check-docs.ps1), each fatal, catching
  contradictions that a single document cannot reveal: the error catalogue against its endpoint
  matrix in both directions; a count claimed in prose against the recomputed count; a DDL enum the
  wire contract names against the values the contract states; every rule against its stated
  verification method; a roadmap release against the scope boundary; a research note against its
  date and source; a supersession against the record it names; a question status against the
  register's vocabulary; and the absence of application code while a blocking question is open.
  Four advisory checks report judgement calls, and `-Strict` promotes them for a release. The
  number-sourcing check treats a cited `NFR-` identifier as a source, because a requirement is
  where a limit is recorded, and it no longer reports a finding when nothing was excluded from the
  inventory.
- A `*Verified by:*` line on all 45 rules in [domain-rules.md](docs/product/domain-rules.md), each
  naming a `T-nn` test, an `NFR-*` measurement, a named artefact in
  [testing-strategy.md](docs/architecture/testing-strategy.md), or the constraint in
  [data-model.md](docs/architecture/data-model.md). The rules previously stated no verification
  method at all, which both [AGENTS.md](AGENTS.md) and [docs/README.md](docs/README.md) require.

### Changed

- **The second claim is now worded honestly.** It was "a retrying client is charged once", which is
  false in one case and was not previously acknowledged anywhere: total store loss destroys
  idempotency records, they are not derivable from the ledger, and a replay after a flush is charged
  twice. The claim is now "a retrying client is charged once, and a double charge caused by store
  loss is reversed". Wording appears in [AGENTS.md](AGENTS.md), [README.md](README.md) and
  [PROJECT.md](PROJECT.md); the behaviour is [DR-049](docs/product/domain-rules.md), the failure
  scenario is FS-22, and the decision is
  [ADR-0017](docs/decisions/0017-noeviction-and-duplicate-reversal.md).
- **The data store no longer evicts, and is sized for the window.** `maxmemory-policy allkeys-lru`
  was specified in [deployment.md](docs/architecture/deployment.md) while the 24-hour idempotency
  window was stated as a guarantee. Those cannot both be true: an evicted idempotency record is a
  lost replay, which is a double charge, not a degraded cache. The fast store now runs `noeviction`
  for the whole key space (DR-048), the projected data-store memory moves from 4 GiB to a 24 GiB
  floor with 32 GiB+ recommended, and capacity exhaustion is a `503` for every tenant rather than a
  silent eviction. The sized figure is a projection and is measured by `IP-15`; the policy is not
  provisional and the measurement does not reopen it.
- **A confirmed status now means the evidence exists.** `A-06` and `A-22` were `confirmed` while
  their verification methods were five conversations and five pilot sessions that have not happened.
  Both are `assumed`. `A-06`'s method was also A-01's method, which measures the market rather than
  the licence, and is replaced with a licence scan of the shipped image plus written confirmation
  from the first two design partners' legal reviewers. The register states what `confirmed` means and
  keeps a dated corrections log.
- **`A-01` is a dated, owner-accepted release risk, not a code-start blocker.** The register gains an
  [accepted-risks](docs/product/assumptions-and-open-questions.md#accepted-risks) table naming who
  accepted it, when, when it is re-checked, and the condition that would convert it into a blocker.
  It moves off [Gate 0](ROADMAP.md#gate-0--unblock-the-specification) and becomes a v0.1 release gate
  with a 2026-12-31 re-check, on the grounds that a gate nobody in the repository can open is a gate
  that teaches everyone to ignore it.
- **[AGENTS.md](AGENTS.md) now gates on a phase rather than on a question.** The prohibition on
  writing application code was a question-shaped rule, and questions got answered. It is now the
  phase map: no code outside a phase that is `IN PROGRESS` and owns the file.
- Two capability documents disagreed about how often an operator may confirm a destructive bulk
  operation, and the three documents that stated the old count of documents, error codes, rules and
  tests were all stale. The counts are recomputed by the checker and now agree.
- `A-01` is now stated as an accepted risk in [PROJECT.md](PROJECT.md), [README.md](README.md) and
  [roadmap-index.md](docs/roadmaps/roadmap-index.md), and the second claim carries the store-loss
  qualifier in the three claim tables that state it.
- **`IP-00` is `COMPLETE` and `IP-01` is `NOT STARTED`.** Every Definition-of-Done item is met or
  deliberately re-scoped, so `IP-01` became the only unblocked phase on 2026-09-27 and the other 26
  are `BLOCKED` on their dependencies rather than on a question. Status changes are in
  [roadmap-index.md](docs/roadmaps/roadmap-index.md#4-phase-map) and
  [v0-1-enforcement-path.md](docs/roadmaps/v0-1-enforcement-path.md).
- **[AGENTS.md](AGENTS.md) gained a "Session start" section and an "Editing files safely" section.**
  The first makes the three-reads-and-one-check opening a rule instead of a habit, and the second
  records how a changelog section was lost on 2026-09-27: a script sliced a file and overwrote the
  original before checking the result, and the cause was a helper function named after a PowerShell
  built-in alias.
- **The specification is under version control, on `main`, from 2026-09-27.** It was unversioned
  while it was written, which is how the `[spec.1]` loss above went unrecoverable. `.gitignore` lost
  its Node and Python sections, which no document in this repository needs; the OS, editor, `.env`
  and Go sections are the ones that do. `.gitattributes` pins LF so a rewrite cannot turn into a
  whole-file diff on a machine with `core.autocrlf` set. No CI workflow is added here, because
  [ADR-0010](docs/decisions/0010-go-chi-spec-first-openapi.md) and `IP-01` own `.github/`, and
  adding one now would be code outside a phase.

### Fixed

- `Q-21` cited `NFR-C1`, which is the Apache-2.0 compliance requirement, for the sustained
  throughput figure. The throughput figure is NFR-T1. Corrected in the register.
- Two research notes argued from `allkeys-lru` and are not corrected, because their conclusions still
  hold and one is now better founded. Both carry a dated update saying what changed:
  [idempotency-and-retries.md](docs/research/idempotency-and-retries.md) treated a lost replay as
  acceptable, which it is not, and
  [reset-cycle-semantics.md](docs/research/reset-cycle-semantics.md) and
  [atomicity-mechanism-options.md](docs/research/atomicity-mechanism-options.md) required an
  "uninitialised" script path for a missing key, which total store loss still needs.
- Per-tenant runtime API keys were a v0.2 capability in [ROADMAP.md](ROADMAP.md) with no row in
  [mvp-scope.md](docs/product/mvp-scope.md), so the two documents disagreed about v0.2's contents.
  The scope row is added, matching the release the roadmap had already committed to, and the phase
  that delivers it is `IP-20`. No capability changed version; one document was missing it.
- `payload_too_large` (413) was documented in [error-catalog.md](docs/product/error-catalog.md) but
  reachable from no endpoint, so a real response — an over-cap `metadata` field — had no documented
  home. It now appears on every endpoint that accepts a request body, and the check enforces that
  no code is ever orphaned again.
- The endpoint filter values for `event_type` and `source` are now stated in
  [api-conventions.md](docs/architecture/api-conventions.md). A client could not know what to send.
- "56 documents" was claimed in [README.md](README.md), [PROJECT.md](PROJECT.md) and
  [ROADMAP.md](ROADMAP.md) where the specification holds 55.
- [go-stack-and-dependency-selection.md](docs/research/go-stack-and-dependency-selection.md) argued
  about sub-millisecond processing against an invented "10,000 req/s burst", which is exactly the
  unsourced number the writing rules forbid. The figure now refers to the sustained 2,000 `consume`/s
  the specification actually states in NFR-T1, and the argument is unchanged.
- `tools/check-docs.ps1` now runs on Windows PowerShell 5.1 as well as PowerShell 7, so the
  documented command works on a machine with nothing installed. Two defects prevented it: a
  `$PSScriptRoot` reference in a parameter default, which 5.1 leaves empty, and a three-argument
  `Regex.Match` that 5.1 binds to the `RegexOptions` overload and therefore searches from offset
  zero.
- The rule-verification check accepted a rule whose statement named a test that does not exist. It
  *[truncated: the rest of this entry was lost with the `[spec.1]` body on 2026-09-27; restore it from
  an external copy if you have one.]*

## [spec.1] - 2026-09-27

**This section's history was lost on 2026-09-27 and has not been reconstructed.**

The body of the `[spec.1]` entries below was destroyed by a scripting error while this changelog was
being edited, and no copy exists in this repository. Nothing has been written in its place, because
an invented changelog entry is worse than an absent one: it would date a decision to a day it was not
made and would attribute a rationale to somebody who did not give it. If a copy exists elsewhere —
an editor's local history, a remote, an archive — paste it here and the file is complete again. What
is not in doubt is that the section exists, that it is dated 2026-09-27, and that it records the
first pass of the specification, before the roadmaps, before Gate 0 closed, and before ADR-0017.

