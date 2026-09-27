# AGENTS.md

Operating manual for this repository. It exists because the specification is large enough that an
agent can produce a plausible, wrong answer by reading three files and stopping.

Two rules govern everything below.

1. **The specification is the product, and its questions are now closed.** There was no application
   code until [Gate 0](ROADMAP.md#gate-0--unblock-the-specification) closed on 2026-09-27: the
   four blocking questions in
   [the open-questions register](docs/product/assumptions-and-open-questions.md#2-open-questions)
   and the eviction question beside them. Writing code before that would have been a defect.
   `IP-00` closed that gate and wrote no code; `IP-01` is the first phase that owns files in the
   repository, and it must be `IN PROGRESS` first. The one thing still unanswered, [A-01](docs/product/assumptions-and-open-questions.md#accepted-risks),
   is a dated, owner-accepted release risk; it is not a reason to stall the contract.
2. **Behaviour is stated once.** A rule lives in
   [domain-rules.md](docs/product/domain-rules.md) as a numbered `DR-nnn`. Every other document
   references it. If two documents disagree, `domain-rules.md` wins and the other document is
   wrong. Never restate a rule's substance in another file; reference the identifier.

## Session start

A new session starts with three reads and one command, in this order. Do not begin editing before
all four are done; the command in particular is not optional, because it is what tells you whether a
break is yours.

| # | Do | Why |
| --- | --- | --- |
| 1 | Read [AGENTS.md](AGENTS.md) — this file — in full | It is the manual, not background reading. Reading a subset is how the identifier traps get hit |
| 2 | Read [roadmap-index.md](docs/roadmaps/roadmap-index.md) §2 and §4 | §2 is the current position, which the rest of the repository describes but does not decide. §4 is which phase is open, and a phase that is not open is a phase you may not work on |
| 3 | Read the current phase's own section in its release document | Its objective, scope, references, dependencies and Definition of Done. A phase document is the authority on what that phase owns |
| 4 | Run the check and report the result | `pwsh -File tools/check-docs.ps1 -Strict`. `powershell -File tools/check-docs.ps1` is equivalent and needs no install |

Then read the row of [Reading order by task](#reading-order-by-task) that matches the task, and only
those documents. A prompt that says "understand the manual" without naming the task leaves the
reading order unchosen, and the wrong row is how a plausible wrong answer happens.

## Before you change anything

Read these, in this order. Do not skip to the file you want to edit.

| Step | Read | Why it matters |
| --- | --- | --- |
| 1 | [README.md](README.md) | The product in one page, and the three claims everything must prove |
| 2 | [PROJECT.md](PROJECT.md) | Status, the claims, four reading orders, and what is blocked |
| 3 | [docs/README.md](docs/README.md) | The index, the identifier namespaces, the consistency rules |
| 4 | The specific document | See the reading orders below for which ones matter |

Then run the check, so you know the specification was clean before you touched it:

```powershell
pwsh -File tools/check-docs.ps1
```

`pwsh` is PowerShell 7. The script also runs unchanged on Windows PowerShell 5.1, which is what
ships with the operating system, so `powershell -File tools/check-docs.ps1` is equivalent and needs
no separate install. If it fails before your change, the failure is not yours. Say so, fix it if it
is in scope, and continue.

## Reading order by task

| You are… | Read, in order |
| --- | --- |
| Changing a rule or its behaviour | [domain-rules.md](docs/product/domain-rules.md) → [request-lifecycle.md](docs/architecture/request-lifecycle.md) → [state-machines.md](docs/product/state-machines.md) → [error-catalog.md](docs/product/error-catalog.md) → [testing-strategy.md](docs/architecture/testing-strategy.md) |
| Adding or changing an endpoint | [api-conventions.md](docs/architecture/api-conventions.md) → [mvp-scope.md](docs/product/mvp-scope.md) → [use-cases.md](docs/product/use-cases.md) → [error-catalog.md](docs/product/error-catalog.md) → [PRD.md](PRD.md) |
| Touching the data store or a key | [data-model.md](docs/architecture/data-model.md) → [atomicity-mechanism-options.md](docs/research/atomicity-mechanism-options.md) → [ADR-0002](docs/decisions/0002-lua-scripts-for-atomicity.md) |
| Touching cycles, resets, or time | [cycle-engine.md](docs/architecture/cycle-engine.md) → [ADR-0003](docs/decisions/0003-lazy-monotonic-cycle-rollover.md) → [ADR-0008](docs/decisions/0008-cadence-model-anchored-and-rolling.md) → [reset-cycle-semantics.md](docs/research/reset-cycle-semantics.md) |
| Touching idempotency, retries, or refunds | [ADR-0004](docs/decisions/0004-idempotency-prevention-and-detection.md) → [ADR-0005](docs/decisions/0005-refund-as-first-class-operation.md) → [idempotency-and-retries.md](docs/research/idempotency-and-retries.md) → [use-cases.md](docs/product/use-cases.md) |
| Sizing or deploying | [deployment.md](docs/architecture/deployment.md) → [performance.md](docs/architecture/performance.md) → [non-functional-requirements.md](docs/architecture/non-functional-requirements.md) |
| Changing scope or a release | [mvp-scope.md](docs/product/mvp-scope.md) → [ROADMAP.md](ROADMAP.md) → [ADR-0015](docs/decisions/0015-release-slicing.md) → [PRD.md](PRD.md) |
| Planning or starting implementation work | [roadmap-index.md](docs/roadmaps/roadmap-index.md) → the phase document for the release → [testing-strategy.md](docs/architecture/testing-strategy.md) → the phase's own specification references |
| Reopening a decision | [TECHNICAL-DECISIONS.md](TECHNICAL-DECISIONS.md) → the ADR → the research note it rests on |

## The identifier system

Stable prefixes, defined once, never renumbered or recycled. The full table with the shape of each
prefix is in [docs/README.md](docs/README.md#identifier-namespaces).

| Prefix | Means | Defined in |
| --- | --- | --- |
| `DR-nnn` | Normative rule | [docs/product/domain-rules.md](docs/product/domain-rules.md) |
| `UC-nn` | Use case | [docs/product/use-cases.md](docs/product/use-cases.md) |
| `FS-nn` | Failure scenario | [docs/product/error-catalog.md](docs/product/error-catalog.md) |
| `A-nn` | Assumption, with a validation method | [docs/product/assumptions-and-open-questions.md](docs/product/assumptions-and-open-questions.md) |
| `Q-nn` | Open question, with a default | [docs/product/assumptions-and-open-questions.md](docs/product/assumptions-and-open-questions.md) |
| `J-n` | End-to-end journey | [docs/product/user-journeys.md](docs/product/user-journeys.md) |
| `NFR-XXn` | Non-functional requirement, with a measurement | [docs/architecture/non-functional-requirements.md](docs/architecture/non-functional-requirements.md) |
| `INV-XXn` | State invariant | [docs/product/state-machines.md](docs/product/state-machines.md), [docs/architecture/data-model.md](docs/architecture/data-model.md) |
| `T-nn` | Named correctness test | [docs/architecture/testing-strategy.md](docs/architecture/testing-strategy.md) |
| `ADR-nnnn` | Decision record | [docs/decisions/](docs/decisions/) |
| `IP-nn` | Implementation phase | [docs/roadmaps/roadmap-index.md](docs/roadmaps/roadmap-index.md) |
| `C-n`, `K-n`, `P-n`, `F-n`, `E-n`, `T-n` | State transition, prefixed by entity | [docs/product/state-machines.md](docs/product/state-machines.md) |

Three traps, all of which have already caused a wrong edit:

- **`T-01` is a test; `T-1` is a tenant-state transition.** Same letter, different namespaces,
  different digit count. `check-docs.ps1` will not catch a swap between them.
- **Assumption identifiers are always two digits.** `A-02` resolves; a single-digit
  `A-` plus one digit does not, and a single-digit form once appeared in the register and pointed
  nowhere.
- **An `NFR` may have two digits.** `NFR-S1` through `NFR-S9`, then `NFR-S10`. A regex written for
  one digit silently misses the tenth.

## How to make a change

### Changing or adding a rule

1. Edit [domain-rules.md](docs/product/domain-rules.md). Every rule has the same shape: statement,
   rationale, verification. If you cannot name how the rule is verified, the rule is not finished.
2. New rules take the next number. Never insert a number, never renumber, never reuse one that was
   deleted — a deleted identifier stays deleted so old references stay visibly broken rather than
   silently wrong.
3. Check every document that references the rule's neighbours. A change to cycle rollover, an
   idempotency window, or the balance contract has reach into
   [request-lifecycle.md](docs/architecture/request-lifecycle.md),
   [data-model.md](docs/architecture/data-model.md),
   [consistency-and-recovery.md](docs/architecture/consistency-and-recovery.md),
   [deployment.md](docs/architecture/deployment.md),
   [observability.md](docs/architecture/observability.md) and
   [testing-strategy.md](docs/architecture/testing-strategy.md). A rule that exists in only one file
   is not specified.
4. Add or extend the named test that proves it. A new claim without a new test is a wish.
5. If the change contradicts an accepted decision, stop and write an ADR. Do not edit the decision.

### Adding an error code

1. Add it to [error-catalog.md](docs/product/error-catalog.md) with an HTTP status, a client action
   and a retry verdict.
2. Use the code verbatim in [api-conventions.md](docs/architecture/api-conventions.md) and in the
   [state machines](docs/product/state-machines.md).
3. Confirm `quotacore_errors_total` covers it. The `code` label set is the catalogue by reference,
   and a contract test asserts the two match.
4. Update the counts in [docs/README.md](docs/README.md#inventory) — the checker will tell you if
   you do not.

### Adding an endpoint

1. Confirm it is in [mvp-scope.md](docs/product/mvp-scope.md) for the release you are targeting.
   An endpoint outside the boundary is a scope change, not an implementation detail.
2. Specify the wire format in [api-conventions.md](docs/architecture/api-conventions.md): status
   codes, error shape, idempotency behaviour, `X-Request-Id`, pagination.
3. Add the use case to [use-cases.md](docs/product/use-cases.md) and the step-by-step path to
   [request-lifecycle.md](docs/architecture/request-lifecycle.md), including what happens when the
   data store is unreachable.
4. Add the API key scope in [ADR-0012](docs/decisions/0012-hashed-scoped-api-keys.md).
5. When implementation starts, `api/openapi.yaml` is the source of truth and is generated from
   these documents, not the other way round ([ADR-0010](docs/decisions/0010-go-chi-spec-first-openapi.md)).

### Superseding a decision

Decision records are immutable once accepted. To change one:

1. Write a new ADR with the next number, referencing the old one, stating the trigger that fired.
2. Set the old ADR's status to `Superseded by ADR-nnnn` — a one-line status change is the only
   edit permitted on an accepted record.
3. Update [TECHNICAL-DECISIONS.md](TECHNICAL-DECISIONS.md) and the affected documents.
4. The research note the old decision rested on stays, and gets a dated note saying what changed.

### Recording an unresolved decision

Not everything is a defect. When the specification is genuinely undecided:

- **A fact that may change** goes in [research/](docs/research/) with a date and a source, and a
  "re-check by" line.
- **A choice that has not been made** goes in
  [the open-questions register](docs/product/assumptions-and-open-questions.md#2-open-questions)
  with a default, so an implementer is never blocked by a missing answer.
- **A bet that may be wrong** goes in the [assumptions](docs/product/assumptions-and-open-questions.md#1-assumptions)
  table with a validation method and an owner.
- **A limitation you designed around** goes in
  [SECURITY.md](SECURITY.md#6-known-gaps) or
  [deployment.md](docs/architecture/deployment.md), never in an absence.

Do not invent an answer to make a document look complete. The specification is more valuable
honest about an open question than confidently wrong.

## Writing rules

- British spelling; hyphenate compound adjectives (`cycle-boundary edit`); no Oxford comma in short
  lists; tables over prose for anything enumerable.
- One topic per file, kebab-case filenames, exactly one H1, no trailing whitespace, no tab
  indentation.
- Relative links, and link back to the root document that spawned the document. Link to a heading
  with the GitHub slug: lowercase, spaces to hyphens, punctuation dropped.
- Prefer `ADRs`, not "decision records", in prose, so the reference is findable.
- Date anything that describes a point-in-time state, especially licensing and pricing. An
  undated claim about a licence is a bug waiting for a licence change.
- No unsourced number. If a limit, price, latency target or third-party fact has no citation in
  [research/](docs/research/), either cite it or remove it. A plausible number with no source is worse
  than no number.
- Never use "Threshold" as a product name. The name is Quotacore.

## Editing files safely

The repository is the deliverable, and it was unversioned until 2026-09-27 — long enough for the loss
below to happen with no way back. Git is available now, so a bad edit is recoverable; recovery is not
prevention, and these rules are cheaper than a rewrite. One changelog section was lost to a script
that sliced a file into a new file and overwrote the original before checking the result, and the
cause was a helper function named after a PowerShell built-in alias. These rules exist because that
happened here.

- **Read the whole file before rewriting any of it.** A slice from a line number you have not read
  is a guess about content you have not seen.
- **Prefer a targeted `edit` to a scripted rewrite.** A string replacement that reports how many
  times it matched is safe; a rewrite that reassembles lines is not.
- **When a script must rewrite a file**, write to a temporary copy, then compare the heading list and
  the line count against the original, and only then replace the original. If the two disagree, the
  original is still there to compare against.
- **Never name a helper after a PowerShell alias.** `sl`, `gc`, `cat`, `ls`, `mv`, `rm`, `sc`, `sp`,
  `si`, `echo` and `select` are all aliases, and a function that shadows one turns a harmless-looking
  helper into a `Set-Location` call with the wrong argument.
- **Preserve encoding and non-ASCII punctuation.** These files are UTF-8 and they contain em dashes,
  section signs and `≤`. A script that reads and writes them as anything else corrupts characters that
  no reader will notice until a link or a slug stops matching.
- **After any bulk edit, run the checker before you claim anything.** It is the cheapest possible
  test that the file is still a document.

## Definition of done

A change is finished when all of these are true:

```powershell
pwsh -File tools/check-docs.ps1     # exits 0; powershell -File tools/check-docs.ps1 is equivalent
```

- Every link resolves, path and anchor.
- Every identifier reference resolves to a definition, and nothing is defined twice.
- The [inventory](docs/README.md#inventory) matches the documents.
- Every document is listed in [docs/README.md](docs/README.md).
- Every error code in the catalogue is reachable from some endpoint, and every code an endpoint
  lists is in the catalogue.
- Every rule carries a `*Verified by:*` line naming a method that exists.
- The behaviour change is reflected in every document that states the old behaviour. Search for
  the identifier, not for the prose: the wording differs between files by design.
- Any new claim resolves to a named test; any new number resolves to a measurement.
- Any scope change appears in [mvp-scope.md](docs/product/mvp-scope.md) *and* [ROADMAP.md](ROADMAP.md)
  with the same version.
- If the change resolved an open question, the question is marked answered and the answer points at
  the rule it produced.
- The change is recorded in [CHANGELOG.md](CHANGELOG.md) under `Unreleased`.

The checker verifies structure. It cannot tell you that a rule is true, that a status code is
right, or that a trade-off is sound. Those need a human, and saying so is part of the job.

## What not to do

- **Do not write application code outside a phase.** No `main.go`, no `api/openapi.yaml`, no
  migrations, no Dockerfiles, unless the phase you are working in is `IN PROGRESS` in
  [roadmap-index.md](docs/roadmaps/roadmap-index.md#4-phase-map) and the file belongs to it. The
  gate is no longer a question; it is a phase. If asked for code that no phase owns, say so and
  offer the phase that should own it.
- **Do not edit an accepted ADR's reasoning.** Supersede it.
- **Do not renumber anything.** Not rules, not use cases, not ADRs, not error codes.
- **Do not delete a rule to resolve a contradiction.** Find the contradiction; one of the two
  statements is wrong, and usually the newer one is the wrong one.
- **Do not add a capability to satisfy a "should also" impulse.** Scope changes go through
  [mvp-scope.md](docs/product/mvp-scope.md) and an ADR.
- **Do not add metrics, endpoints, or config keys** that no document requires. Every one of those
  is an obligation on someone else's time.
- **Do not restate a rule's substance in a summary document.** Reference the `DR-nnn`. Summaries
  drift, and a drifted summary is a second source of truth.
- **Do not guess at an external fact.** Check the [research notes](docs/research/) and their
  sources, date what you find, and say when a fact is stale.
- **Do not add a phase for a capability that is not in [mvp-scope.md](docs/product/mvp-scope.md).**
  A phase is derived from approved scope; if the work seems to need one, either the scope is
  incomplete or the phase belongs to a release it was not planned into. Both are scope changes and
  belong in [mvp-scope.md](docs/product/mvp-scope.md) and [ROADMAP.md](ROADMAP.md) with the same
  version, first.
- **Do not write a definition-of-done item that cannot fail.** "Works", "tested" and "fast enough"
  are not items. A named test, a measurement with a threshold, or a schema constraint is; the three
  forms are listed in
  [roadmap-index.md](docs/roadmaps/roadmap-index.md#5-what-makes-a-definition-of-done-item-valid).
- **Do not start a phase before its gating questions are answered,** and do not start one "just to
  save time". The output of a blocked phase is work to throw away, which is more expensive than
  waiting.

## The three claims

Everything in the specification exists to support these. If a change makes one of them harder to
prove, say so before making it.

1. **P99 under 5 ms at 2,000 requests per second.** The data plane never opens a database
   connection on the request path.
2. **A retrying client is charged once.** Idempotency is prevented in the data plane and detected
   in the control plane, with a 24-hour window. Total store loss is the one hole, and a double charge
   caused by it is reversed automatically rather than denied (DR-049).
3. **One `docker compose up` and it works.** No account, no signup, no egress, no external
   dependency.

Each is mapped to named tests in
[testing-strategy.md](docs/architecture/testing-strategy.md#2-correctness-tests-the-ones-that-matter) and to
measurable requirements in
[non-functional-requirements.md](docs/architecture/non-functional-requirements.md).

## Known traps in the current text

Things that are decided, and that a later reader could plausibly "fix" in the wrong direction.

  - **The 24-hour idempotency window is fixed**, not configurable.
    `QUOTACORE_IDEMPOTENCY_TTL` is pinned to 24 hours for the window's life (DR-029). Making it
    tunable would change a documented customer guarantee. This one *is* decided.
  - **A missing balance is a failure, not a re-grant.** Balances are materialised at provisioning and
    at cycle transitions only (DR-045). Materialising on demand would hand a full allowance to
    whoever triggered an eviction. This one *is* decided.
  - **The fast store does not evict.** `maxmemory-policy noeviction` covers the whole key space, and
    the store is sized for the full 24-hour window (DR-048, [ADR-0017](docs/decisions/0017-noeviction-and-duplicate-reversal.md)).
    Capacity exhaustion is a `503` for every tenant rather than a silent eviction. The sized figure
    is a floor until `IP-15` measures it, and this one *is* decided — the measurement refines the
    number, not the policy.
  - **`noeviction` does not close the restart hole.** A total store loss destroys idempotency
    records, and they are not derivable from the ledger, so a replay after that is charged twice and
    then reversed (DR-049, FS-22). Do not "fix" the reversal by dropping the record instead, and do
    not describe the window as surviving a flush.
- **The event ledger is best effort and at-least-once.** `event_id` deduplication at the consumer is
  part of the contract, not an oversight.
- **A cache miss may read the database once.** DR-039 permits a single bounded lookup on a miss.
  The prohibition is on a per-request read of configuration or API keys, not on any database access
  at all.
- **Cycle rollover is computed in the application**, in Go, in UTC, from the anchor. `cycle_index`
  in the data store is a memo for reconciliation, not a second authority.

## Related files

| File | Use |
| --- | --- |
| [docs/README.md](docs/README.md) | The index, the inventory, the identifier namespaces, the consistency rules |
| [docs/roadmaps/roadmap-index.md](docs/roadmaps/roadmap-index.md) | The execution plan: 28 phases, the dependency chain, and what makes a definition-of-done item valid. Read it before starting any implementation work |
| [ROADMAP.md](ROADMAP.md) | Release slicing only — what ships in which release, and the Gate 0 blockers |
| [CLAUDE.md](CLAUDE.md) | The same entry point, for harnesses that look for that filename |
| [CHANGELOG.md](CHANGELOG.md) | The history of the specification |
| [tools/check-docs.ps1](tools/check-docs.ps1) | Links, anchors, identifiers, inventory, and the nine semantic checks that catch a contradiction between two documents. The only command you need |
