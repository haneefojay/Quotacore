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
- `IP-03`, the cycle engine and the boundary matrix, closed with its evidence recorded in the phase
  section of [v0-1-enforcement-path.md](docs/roadmaps/v0-1-enforcement-path.md). The engine is
  `internal/cycle`, a pure function from an anchor and an interval to the current window, embedded
  with the Go time-zone database (`import _ "time/tzdata"` in the engine and the binary, satisfying
  NFR-OPS8 without egress). Eight files, closed by a test each: `TestBoundaryMatrix` (all 20 rows of
  the [cycle-engine.md](docs/architecture/cycle-engine.md) §6 table to the exact RFC 3339 instant),
  `TestDaylightSavingPair`, `TestNeverHasNoBoundary`, `TestProjectedWindowEqualsComputedWindow`
  (six fixed DST probes plus 4,000 seeded draws over five zones), `TestCycleIndexIsMonotonic` and
  `TestClockJumpBackwards` (T-05), `TestIdleAcrossBoundaries` (T-06), `TestZoneDatabaseIsPresent`,
  and `TestSearchIsNotLinearByConstruction` with two benchmarks (five-years: hourly 348 ns/op,
  daily 18.2 µs/op, weekly 15.1 µs/op, monthly 7.3 µs/op, yearly 4.4 µs/op; `Add` 7.2 ns/op, no
  allocations). `TestNoProductBehaviour` in `internal/api` now also pins the engine's arithmetic
  vocabulary — `currentIndex`, `addMonths`, `addDays`, `addYears` — inside `internal/cycle`.
- `IP-04`, the data-plane skeleton, the keyspace and the snapshot cache, closed with its evidence
  recorded in the phase section of
  [v0-1-enforcement-path.md](docs/roadmaps/v0-1-enforcement-path.md). It owns `internal/store` (key
  builders for every address [data-model.md](docs/architecture/data-model.md) section 3.1 names —
  hash-tagged so a tenant's keys share a slot, an expiry rule of window end plus 24 hours, the
  `qc:cfg` version reads, and the invalidation publish/subscribe with a `{"version":"N"}` payload —
  plus two source checks that hold the data plane off control-plane imports and off `KEYS`/`SCAN`
  anywhere in `internal/`), `internal/snapshot` (an LRU with a byte budget and interned plans, a
  token-bucketed miss path that loads a tenant exactly once per miss, decision order
  archived-over-suspended-over-in-plan per DR-015, and subscriber and reconcile loops that discard
  stale-version messages, count the discard, and keep the previous snapshot when a refresh fails),
  and `internal/observability` (seven collectors, exactly the seven named in observability.md,
  registered per registry). `internal/db` gains the pool-wait sampler that feeds
  `quotacore_dbpool_wait_seconds`, and `cmd/quotacore` wires pool, snapshot and metrics to the
  readiness terms and the six new configuration keys of deployment.md section 4. A datastore round
  trip and the NFR-D3 evidence run in CI against the real valkey; NFR-T7 is closed by
  `TestSnapshotMemoryWithinNFRT7`.
- `IP-05`, the atomic scripts, closed with its evidence recorded in the phase section of
  [v0-1-enforcement-path.md](docs/roadmaps/v0-1-enforcement-path.md). It owns `internal/script`:
  three embedded Lua bodies — `consume`, `refund`, `check` — and the Go runner that addresses them
  by digest. The bodies are `//go:embed` constants, so the bytes the store executes are the bytes
  the binary was built from (NFR-S8, ADR-0002); `Load` compares the store's digest with the one Go
  computed, so a proxy that rewrote a body is a start-up error rather than a silent difference; the
  runner sends `EVALSHA` and never `EVAL`, reloads all three and retries exactly once on
  `NOSCRIPT`, and performs no cycle arithmetic at all — `internal/cycle` is the only calendar
  authority (DR-004, INV-C2). All three answer with eight strings, and the reply is the state the key
  holds *after* the write, so no second round trip is needed to describe a decision. A test asserts
  the three shared transition blocks are byte-for-byte identical, so there is no second
  implementation of rollover to drift (INV-C2), a second asserts the work is not proportional to
  the tenant with 200 sibling hashes, and a third asserts that no request field is ever assembled
  into script text.
  **Proven against a real valkey, not a fake.** 200 concurrent decrements of a balance of 100
  succeed exactly 100 times and leave the balance at 0, never negative (T-01, DR-017); 50 callers
  that all discover a closed boundary re-grant it once between them rather than 50 times (DR-045);
  a target behind the store is answered `stale` and rolls nothing (T-05); a denial changes no
  balance, no cycle state and no expiry (T-08); the ceiling holds and a refund that would break it
  is refused whole rather than clamped (T-04, DR-020); a missing or unusable hash is answered
  `state_missing` and never initialised from request arguments (DR-045); `PTTL` is window end plus
  24 hours for a finite window and `-1` for a `never` one (INV-X6); and a command sent over a raw
  socket is applied whole or discarded whole across ten alternating attempts, with the hash never
  half-written (T-09). Three defects were found by those tests and fixed: an undefined `KEY` global,
  a reply one unit behind the key, and a stale caller skipping the expiry write.
- `internal/api/shape_test.go` now permits `internal/script` and pins `EVALSHA`/`EvalSha` to it as
  script-scoped vocabulary, while the calendar and ledger symbols stay forbidden everywhere and
  `DeductBalance`, `CheckAndDeduct`, `balanceRemaining` and `idempotencyKey` stay forbidden outright.
  `TestNoProductBehaviour` still holds: a consume is one `EVALSHA` and no route reaches it until
  `IP-07`.
- `Runner.Client`, so start-up loads the three scripts through the same connection the runner will
  use, and `cmd/quotacore` loads them once at start-up (ADR-0002's "loaded at process start"). A
  failed load is a warning rather than a refusal to start, because the runner self-heals on the
  first `NOSCRIPT` and refusing to start would turn a transient store outage into an outage of the
  process.

### Changed

- **`IP-02` is `COMPLETE`, started and closed on 2026-09-28, with all seven Definition-of-Done
  items met.** The contract exists, is served, and is generated from the specification rather than
  ahead of it.
  **The contract.** `api/openapi.yaml` is 5,037 lines covering 39 operations, 67 named schemas and
  all 34 catalogue codes, with the response envelope, `X-Request-Id` on every response, the
  `Idempotency-Key` header, cursor pagination, `additionalProperties: false` on every request body,
  and a request and response example on every operation.
  **The generated types.** ogen 1.24.0 with `disable_all` emits types, JSON codecs and validators
  and nothing else: no router, no client, no telemetry layer, and no `501` handler, because a
  generated stub answering `501` for a v0.1 endpoint would be a second wrong meaning for
  `not_implemented` (`TestOgenEmitsNoRoutes`).
  **Served, not just written.** `GET /openapi.json` returns the embedded document converted from
  YAML to JSON once at start-up and cached, with object key order preserved so the bytes are stable
  and diffable; a request path that parsed a 5,000-line document would be a request path spending
  time on something that cannot change while the process runs. `GET /docs` returns a committed
  219,356-byte HTML page generated from the same file by `tools/gendocs`, self-contained with inline
  CSS, no JavaScript and nothing fetched, because a reference that needs the network is a reference
  that is blank on the air-gapped deployment this product is designed for. Both are `no-store` and
  both are unauthenticated. The served document is asserted to be the committed bytes and the
  document the binary was built from, so the contract cannot drift from the binary unnoticed.
  **Eight claims that were promises became tests.** [api-conventions.md](docs/architecture/api-conventions.md#13-contract-tests)
  section 13 promised eight enforced contract checks and named only five. The catalogue, matrix,
  example, `X-Request-Id` and `additionalProperties` checks existed; the other three did not.
  `TestEverySchemaHasADescription` and `TestEverySchemaFieldHasATypeAndADescription` enforce item 3,
  `TestNoRefIsUnresolvedAndNoComponentIsDead` enforces item 7, and
  `TestUnexpressibleConstraintsNameTheirMechanism` enforces item 8 — the override constraint JSON
  Schema cannot express and the data store enforces as `tenant_overrides_not_empty`. A specification
  that claims a check nothing performs is a wish, and the cost of finding out was **47 fields and 21
  schemas that had no description at all**: 47 additions to the contract, which is why every
  response field now reads as a specification rather than as a type listing.
  **Security is stated the way the implementation enforces it.** `bearerAuth` is declared once
  globally; every operation's `security` array is empty, because a scope in the array means *exactly*
  that scope and would exclude an `admin` key from the data plane, which section 5 requires it to
  work on. The minimum each route needs is `x-required-scope: runtime` or `admin`, a named vendor
  extension, because OpenAPI 3.1 has no vocabulary for "at least this scope" and inventing one would
  make the document non-conformant. Operational routes state `security: []` explicitly rather than
  inheriting a global requirement and opting out of it.
  **No product behaviour.** An enforcement call still returns `404`, because `IP-07` owns the routes.
  `TestNoProductBehaviour` and `TestOgenEmitsNoRoutes` both fail if that ever stops being true, so
  the claim is asserted by a test rather than by prose.
  **The seventh item was added on the day it was needed.** `mvp-scope.md` and
  [ADR-0010](docs/decisions/0010-go-chi-spec-first-openapi.md) both require a generated `/docs` page
  and no phase owned it, so it was either unowned or silently dropped. It is now `IP-02` item 7, and
  it can fail: the page is generated by `make docs` and CI fails if the committed page differs from
  the generated one.
  Status is in [roadmap-index.md](docs/roadmaps/roadmap-index.md#4-phase-map) and
  [v0-1-enforcement-path.md](docs/roadmaps/v0-1-enforcement-path.md#ip-02--the-openapi-contract), and
  the evidence for each item is a table in the phase section rather than a sentence about it.
- **`api-conventions.md` gained [section 5.1](docs/architecture/api-conventions.md#51-how-the-contract-states-security)
  and [section 14](docs/architecture/api-conventions.md#14-the-served-contract).** Section 5.1 states
  how the contract expresses security, including why the requirement arrays are empty and why
  `x-required-scope` is a vendor extension. Section 14 states the served contract: what `/openapi.json`
  and `/docs` return, why the JSON is generated at start-up rather than parsed per request, why the
  page is a committed artefact rather than a runtime template, and why both routes are `GET` only.
  Section 14 was added as a new section rather than inserted, because renumbering is forbidden and a
  section 14 after section 13 is cheaper than a broken link.

- **`IP-01` is `COMPLETE`, and one of its six Definition-of-Done items was amended before it was
  met.** All six items are now satisfied, four of them by evidence recorded in the phase's own
  section. The first two were already met: a `go.mod` for `github.com/quotacore/quotacore`, a
  `Makefile` whose targets are the documented build steps, `internal/api` holding the `/healthz` and
  `/readyz` probes with unit tests, a development Compose file that brings up the fast store and
  Postgres and nothing else with the fast store on `noeviction` as decided, and a CI workflow that
  runs the specification check as a build-failing gate. Four more are now met and asserted rather
  than asserted-about.
  **The service refuses to start** on an unacknowledged `QUOTACORE_BOOTSTRAP_ADMIN_KEY` or on a
  shortened `QUOTACORE_IDEMPOTENCY_TTL`, starts and reports not-ready when no tenant exists, and is
  proven by running the built binary on the Compose network: `/healthz` `200`, `/readyz` `503`,
  `/v1/balance` `404`, and a drain to exit code `0` read from the container rather than inferred
  from a successful `docker stop`. **The module graph is pinned and the licence gate is real**: three
  direct requirements (`chi` v5.3.2, `pgx` v5.11.0, `goose` v3.28.0), `go.sum` committed, `go mod
  tidy` idempotent, a CycloneDX 1.6 SBOM generated by the build, and a scan that fails closed on a
  copyleft dependency without an `ADR-nnnn`. **No product behaviour**, by two tests that both fail
  when broken: a router test requiring the route set to be exactly the two probes, and a source-level
  check requiring the package set to be exactly `cmd/quotacore`, `internal/api` and `internal/db`
  with no cycle, balance, deduction or script arithmetic anywhere in the module.
  **The amendment.** Item 4 asked for an SBOM over the Go module graph *and the base image*. No image
  exists in `IP-01`, whose own scope excludes one, because `IP-15` owns the multi-stage image, so no
  work inside the phase could produce the second half. On the owner's decision the base-image half
  moved to `IP-15`'s new item 11, which names NFR-C3 and NFR-C4, and `IP-01`'s item 4 now covers the
  Go module graph and the runtime path. The obligation has a successor rather than a deletion, which
  is the part that matters: an SBOM that omits the base image describes a binary the operator cannot
  run, and quietly dropping the other half would have removed a real requirement from the plan.
  Status is in [roadmap-index.md](docs/roadmaps/roadmap-index.md#4-phase-map) and
  [v0-1-enforcement-path.md](docs/roadmaps/v0-1-enforcement-path.md).
- **Two defects in `IP-01`'s own code were found by running it, and one bug in its CI was found by
  reading how CI behaves.** The readiness log closure mutated its remembered set on every probe with no
  lock, so two concurrent `/readyz` requests raced on it, and it never logged the first observation,
  which is the transition an operator needs. Both are fixed, and the test that guards the lock fails
  in 9 runs out of 10 when the lock is removed; its first version passed 16 of 20 times and asserted
  on the Go scheduler rather than on the code. Separately, the integration test was gated on `CI`
  being set, which would have failed the build job on every pull request: hosted runners set `CI` for
  their own reasons, and that job has no database. The gate is now the explicit
  `QUOTACORE_REQUIRE_INTEGRATION`, so a green pipeline still cannot have skipped the test.
- **Two claims about the toolchain that were recorded as facts were transient network failures.** The
  phase document said the module proxy was unreachable and that the four dependencies the scope names
  were absent from `go.mod`, and the changelog carried the first of those forward. The proxy
  answered normally when re-checked, the dependencies are in and pinned, and `go mod tidy` completes
  and is idempotent. The second error was naming `go-redis` as a requirement of this scope: it is not
  named there, the data-store client belongs to the data-plane phases, and depending on it now would
  add a requirement the build does not use. CI now runs `go mod tidy` and fails if it changes
  `go.mod` or `go.sum`, which is the authority on this rather than a note in a document.
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
- **`IP-00` became `COMPLETE` and `IP-01` became the only unblocked phase, both on 2026-09-27.** At
  that point `IP-01` was `NOT STARTED`; the later entry above closes it. Every Definition-of-Done item
  was met or deliberately re-scoped, and the other 26 phases are `BLOCKED` on their dependencies
  rather than on a question. Status changes are in
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
- **`make ogen-check` now compares regenerated bytes instead of asking git whether the working tree
  is clean.** The previous implementation regenerated in place and failed if
  `git status --porcelain` reported anything, which cannot tell "the generated code is stale" from
  "the generated code is correct but not yet committed". It therefore failed for anyone in the
  middle of editing the contract, running `make generate` and committing both, and could only pass
  in CI — the one place it is genuinely needed. The target now generates into `.ogen-check/`,
  copies the committed files alongside, and diffs the two trees, which answers the same way on a
  laptop and in CI and also catches a file added or removed by the generator rather than only
  edited. `.ogen-check/` is in `.gitignore`, and is removed whether the check passes or fails.
  Proven in both directions: a one-line description change in `api/openapi.yaml` fails the check
  with a unified diff pointing at the changed comment, and restoring the contract passes it.
  `make ci` is unchanged as a set of steps and now runs green on Windows as well as Linux.

- **`IP-04` is `COMPLETE`, started and closed on 2026-09-28, with all six Definition-of-Done items
  met.** The phase section of
  [v0-1-enforcement-path.md](docs/roadmaps/v0-1-enforcement-path.md) carries the evidence table.
  DoD item 1 carries a dated amendment: the end-to-end chaos row needs a product route to observe,
  so it moves to `IP-07` (which pins `503 control_plane_unavailable`), while this phase proves the
  mechanism by construction — source checks hold the data plane off the control plane and off
  cursor scans, a miss is one bounded read behind a rate limiter, and
  `quotacore_dbpool_wait_seconds` samples the pool off the request path. Two conflicts inside the
  specification were resolved in favour of their authoritative documents: the cache-miss counter is
  named once (`quotacore_config_cache_miss_total`, in observability.md), and the key layout follows
  data-model.md exactly — ADR-0002's example spellings described the same addresses, and the
  accepted decision record is untouched because its subject is the script mechanism, not the key
  bytes. One defect was found in the phase's own test fixtures: the snapshot tests first loaded a
  tenant at a version their own fake disagreed with — a test-written-from-the-same-source trap that
  only the prune assertion exposed — and the fixture now stores the version `qc:cfg:tenants`
  announced at load time, which is what the prune-to semantics require.

- **`IP-05` is `COMPLETE`, started and closed on 2026-09-29, with all ten Definition-of-Done items
  met, three of them amended.** The phase section of
  [v0-1-enforcement-path.md](docs/roadmaps/v0-1-enforcement-path.md) carries the evidence table.
  **The arithmetic now exists and is atomic.** Three embedded Lua bodies make the decision and the
  mutation one indivisible step against a single hash, with the cycle transition shared byte-for-byte
  across all three so there is no second rollover implementation to drift. 200 concurrent
  decrements of a balance of 100 succeed exactly 100 times and leave the balance at 0 (T-01);
  50 callers that all discover a closed boundary re-grant it once (DR-045); a caller behind the
  store rolls nothing (T-05); a denial changes nothing (T-08); the ceiling holds and an over-ceiling
  refund is refused whole rather than clamped (T-04, DR-020); a missing hash is `state_missing` and
  never an initialisation (DR-045); and `PTTL` is window end plus 24 hours, or `-1` for `never`
  (INV-X6). `T-09` is proven in the half this phase can reach — a command sent over a raw socket is
  applied whole or discarded whole, never half-written — and `T-10` in the half that does not need a
  process, a runner that has never seen a key behaving as one that has just started. The
  process-level forms of both are `IP-07`'s, where there is a process to kill.
  **Three amendments, each recorded in the phase section with its reason.** DoD item 4 and item 5
  were narrowed to the store-level halves for the reason above, so the obligation has a named
  successor rather than a deletion. DoD item 6 said a missing hash is `503 service_unavailable`; no
  HTTP route exists until `IP-07`, so the scripts answer the condition as the decision
  `state_missing` and the `503` and the counter increment are `IP-07`'s, with `IP-07` already
  naming this exact condition in step 2 of the request lifecycle.
  **Three defects the real store found, and a specification statement that was wrong.** The first
  version of the scripts referenced `KEY` as a global rather than reading `KEYS[1]`, which is an
  error only a real server reports; the reply carried the balance read at the top of the script, so
  every `applied` answer was one unit behind the key; and a caller answered `stale` returned before
  the expiry write, so a key materialised without an expiry kept none as long as every caller was
  behind it. Each is fixed and each is now covered by a test, the last by a new item 10. Separately,
  `observability.md` described `quotacore_balance_key_missing_total` as counting a hash that "had to
  be initialised", which contradicted DR-045 and was never true of the design: the description now
  says a request is failed closed with `503` rather than initialised. The metric keeps its name and
  its P0 severity.
  **The `never` encoding is now stated.** `data-model.md` section 3.1 records that all six hash
  fields are always written, that a `never` entitlement stores `window_end` as the empty string
  rather than a sentinel timestamp, and why: `HMGET` answers an absent field and an empty one
  identically, so a missing `window_end` cannot be told from a `never` one that was never written.
  The control-plane writer in `IP-09` therefore writes six fields or none.

### Fixed

- Rows 4–7 of the boundary table in [cycle-engine.md](docs/architecture/cycle-engine.md) §6 were
  wrong, and an implementer reading them would have built the wrong calendar engine while the tests
  written from the same document passed. The daily and weekly rows around the EU spring forward
  named times an hour and a weekday too early, and the two monthly rows for a January-31 anchor
  named the open and closed ends a day off on both sides. Corrected at the instant the Go time-zone
  database computes, with a dated note recording the change, and the `T-07` fixture now asserts the
  corrected values. Found by `IP-03`.
- The committed `go.mod`/`go.sum` were not tidy: `go build ./...` failed on a clean checkout with
  "updates to `go.mod` needed", and the CI gate that checks tidiness was red before `IP-03` touched
  the tree. The generated `api` package imports `ogen-go/ogen`, `go-faster/errors` and
  `go-faster/jx`, none of which the committed `go.mod` required. `go mod tidy` repairs it, adding
  only requirements the committed `go.sum` already satisfied; `internal/cycle` imports nothing but
  the standard library.
- The `Files in the repository` inventory count included build output, so the number in
  [docs/README.md](docs/README.md) was unreproducible the moment anything in the repository produced
  an artefact. The count now honours the anchored directory patterns in `.gitignore` — `/bin/` and
  `/dist/` — which that file already declared and which nothing needed until `IP-01` added a build and
  an SBOM. The exclusion applies to that one count only: a Markdown document cannot be hidden from
  the link, identifier or inventory-document checks by adding a line to `.gitignore`, which is the way
  this kind of exclusion usually turns into a weakened gate.
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

