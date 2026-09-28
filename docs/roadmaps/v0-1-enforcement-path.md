# v0.1 — the enforcement path

Quotacore · 2026-09-27

Seventeen phases, `IP-00` to `IP-16`. The first closes the specification, the last demonstrates the
three claims. Everything between them builds the smallest service that can honestly be called
metered quota enforcement: [mvp-scope.md](../product/mvp-scope.md) §2, §3.

Read [roadmap-index.md](roadmap-index.md) first. It defines the status legend, what makes a
Definition of Done item valid, and the checkpoint every phase must pass.

## Phase map

| ID | Phase | Depends on | Status |
| --- | --- | --- | --- |
| `IP-00` | Close the blocking questions | — | `COMPLETE`, closed 2026-09-27 |
| `IP-01` | Repository, toolchain and CI foundation | `IP-00` | `COMPLETE`, closed 2026-09-27 |
| `IP-02` | The OpenAPI contract | `IP-01` | `BLOCKED` |
| `IP-03` | Cycle engine and boundary matrix | `IP-01` | `BLOCKED` |
| `IP-04` | Data-plane skeleton, keyspace and snapshot cache | `IP-02`, `IP-03` | `BLOCKED` |
| `IP-05` | The four atomic scripts | `IP-03`, `IP-04` | `BLOCKED` |
| `IP-06` | Idempotency prevention in the data plane | `IP-05` | `BLOCKED` |
| `IP-07` | Data-plane endpoints | `IP-05`, `IP-06` | `BLOCKED` |
| `IP-08` | Control-plane schema and migrations | `IP-02` | `BLOCKED` |
| `IP-09` | Control-plane API and API keys | `IP-08` | `BLOCKED` |
| `IP-10` | Plan lifecycle and cycle-boundary semantics | `IP-09` | `BLOCKED` |
| `IP-11` | Idempotency detection and reconciliation | `IP-07`, `IP-12` | `BLOCKED` |
| `IP-12` | Usage ledger, writer and archiver | `IP-08` | `BLOCKED` |
| `IP-13` | Observability | `IP-07` | `BLOCKED` |
| `IP-14` | The `quotacore` CLI | `IP-09`, `IP-12` | `BLOCKED` |
| `IP-15` | Reference deployment and restore rehearsal | `IP-13`, `IP-14` | `BLOCKED` |
| `IP-16` | Full suite, the three claims, v0.1 exit | `IP-10`, `IP-11`, `IP-15` | `BLOCKED` |

---

## IP-00 — Close the blocking questions

**Status** `COMPLETE` as of 2026-09-27, with every question closed. It was `BLOCKED` on the validation of
`A-01`, which is five conversations with design partners and is not inside this repository's control.
That dependency moved to a dated, owner-accepted v0.1 release risk, on the grounds that a gate nobody
can open is a gate that teaches everyone to ignore it.

**Objective.** Turn the open items that gate implementation into decided rules, so that no later
phase has to guess a contract.

**Scope.** No application code. This is the specification phase the repository currently exists to
serve, and [AGENTS.md](../../AGENTS.md) forbids application code until it completes.

| Item | Question | Blocking | Must produce | Outcome |
| --- | --- | --- | --- | --- |
| `Q-01` | Does an admin `grant` survive a plan change? | The admin API freeze | A rule stating that a grant is cycle-scoped (DR-013, DR-020) | Already decided. Answered with no new text; `UC-06` already stated the override half |
| `Q-02` | Is the balance after a plan change the new limit, or the new limit plus the remainder? | The admin API freeze | A rule, and a statement in the error catalogue if any code depends on it | Already decided. Answered, and no new code was needed |
| `Q-04` | Does `apply-now` need a confirmation token above N tenants? | v0.1 release | The new `confirmation_required` code, its status, its client action and its retry verdict | New: `confirmation_required`, DR-046, FS-19, T-12 |
| `Q-14` | Is there a plan-edit impact endpoint? | v0.1 release | The route, its response shape and its matrix row | New: `GET /v1/admin/plans/{id}/impact`, UC-19, FS-19 |
| `Q-21` | May the data store evict an idempotency record inside 24 hours? | v0.1 release | A rule **and** an ADR, because the answer trades claim 2 against claim 1 | New: DR-048, DR-049, NFR-T9, NFR-T10, T-13, T-14 and ADR-0017 |
| `A-01` | Is metered enforcement a durable standalone purchase? | Was the first endpoint | Five dated conversations, each about the enforcement job specifically | **Moved out of this phase** and carried as an accepted release risk. The script is in [a-01-design-partner-validation.md](../research/a-01-design-partner-validation.md); the conversations belong to the v0.1 release, not to the specification |

**Specification references.**
- [assumptions-and-open-questions.md](../product/assumptions-and-open-questions.md) §1, §2
- [domain-rules.md](../product/domain-rules.md), for the shape of a finished rule
- [error-catalog.md](../product/error-catalog.md), which now contains `confirmation_required`
- [mvp-scope.md](../product/mvp-scope.md) §2, §3
- [ADR-0017](../decisions/0017-noeviction-and-duplicate-reversal.md), for the `Q-21` decision

**Dependencies.** Blocks every other phase. Nothing blocks it.

**Definition of Done.**

1. ~~`Q-01`, `Q-02`, `Q-04`, `Q-14` and `Q-21` are marked answered, and each answer cites the `DR-nnn`
   it produced.~~ Done. `Q-01` and `Q-02` cite rules that already existed, which is exactly why they
   needed none.
2. ~~`confirmation_required` exists in the catalogue with a status, a client action and a retry
   verdict, and appears in the matrix row for the plan-apply route.~~ Done.
3. ~~The plan-impact route from `Q-14` appears in [api-conventions.md](../architecture/api-conventions.md)
   with its response shape, and in the matrix.~~ Done.
4. ~~`Q-21` produced an ADR whose Consequences section names which claim gives way and under what
   sizing condition.~~ Done by [ADR-0017](../decisions/0017-noeviction-and-duplicate-reversal.md):
   neither claim gives way. The store runs `noeviction` sized for the window, and the residual
   store-loss case is answered by an automatic reversal rather than by weakening the claim.
5. ~~`A-01` has five dated records.~~ **Not a DoD item of this phase, deliberately.** It moved to the
   v0.1 release; see
   [accepted risks](../product/assumptions-and-open-questions.md#accepted-risks). A DoD item that this
   repository cannot close is a DoD item that will eventually be marked done dishonestly.
6. Any answer that contradicts an accepted ADR has a superseding ADR, and the superseded record's
   status line has been changed and nothing else in it. No answer contradicted an existing ADR.
7. `tools/check-docs.ps1` exits 0.

**Exit criteria.** A reader can implement `POST /v1/admin/plans/{id}/apply` without consulting a
human, because every question it depended on has been answered in writing.

**Deferred.** Nothing. This phase is the only thing standing between the specification and code.

---

## IP-01 — Repository, toolchain and CI foundation

**Status** `COMPLETE` as of 2026-09-27, with all six Definition-of-Done items met, one of them
amended on the owner's decision before it was met. Unblocked since 2026-09-27; set `IN PROGRESS`
before writing any file it owns, which is what [AGENTS.md](../../AGENTS.md) requires. The amendment is
item 4, whose base-image half moved to `IP-15` because no work in this phase could produce it, and
that item is written as an amendment rather than tidied away: an SBOM obligation that disappears
without a successor is an obligation that was quietly dropped.

**Objective.** A repository that builds, tests and runs its dependencies from a clean checkout,
with no product behaviour in it yet.

**Scope.**
- Go module with `chi` ([ADR-0010](../decisions/0010-go-chi-spec-first-openapi.md)).
- Embedded, up-only migrations applied at start
  ([ADR-0011](../decisions/0011-postgres-only-control-plane.md)).
- A development Compose file starting the data store and Postgres, and nothing else.
- Continuous integration running `tools/check-docs.ps1`, lint, `go vet` and the unit tests.
- Dependency pinning, a licence scan, and an SBOM produced at build time.

**Specification references.**
- [ADR-0009](../decisions/0009-redis-protocol-valkey-default.md): the default data-store image and
  its licence, and why the licence change is a decision rather than an upgrade
- [ADR-0014](../decisions/0014-apache-2-0-license.md): Apache-2.0, with the patent grant
- [NFR-C3](../architecture/non-functional-requirements.md) and NFR-C4, the SBOM and the
  copyleft constraint
- [NFR-S7](../architecture/non-functional-requirements.md), pinned dependencies
- [NFR-OPS1](../architecture/non-functional-requirements.md), first run
- [NFR-S9](../architecture/non-functional-requirements.md), refusing to start on an unacknowledged
  bootstrap key

**Dependencies.** `IP-00`, which is `COMPLETE`. Blocks `IP-02`, `IP-03`, and therefore everything.

**Build steps.** Item 1 requires that no step is undocumented, so the steps this phase adds are
enumerated here and nowhere else. Every one is a `make` target, and every recipe is a single
command whose exit code is the target's exit code.

| Target | Command | Fails when |
| --- | --- | --- |
| `build` | `go build ./...` | A package does not compile |
| `binary` | `CGO_ENABLED=0 go build -o bin/quotacore ./cmd/quotacore` | The binary does not link |
| `test` | `go test ./...` | A test fails |
| `race` | `CGO_ENABLED=1 go test -race ./...` | A test fails, or the race detector reports one. Separate from `test` because `-race` needs cgo, and a machine with no C compiler can still run every other gate |
| `vet` | `go vet ./...` | `vet` reports a finding |
| `lint` | `gofmt -l .`, then `go vet ./...` | A file is unformatted, or `vet` reports a finding |
| `check-docs` | `tools/check-docs.ps1 -Strict`, under `pwsh` or `powershell` | The specification is inconsistent, or neither shell is installed |
| `license-check` | `tools/license-scan.ps1`, under `pwsh` or `powershell` | The runtime path holds a copyleft dependency with no decision record, or a licence that cannot be classified |
| `sbom` | `tools/license-scan.ps1 -SbomPath dist/sbom.cdx.json` | As `license-check`, and the SBOM is not written |
| `ci` | `check-docs`, `lint`, `test`, `license-check` | Any of the four fails |

`make` is not installed on every host, and the reference platform is Linux
([deployment.md](../architecture/deployment.md) §9), so each target's command is also the way to run
it by hand. Nothing above is a step a contributor has to discover.

`test` reaches the database when `QUOTACORE_DATABASE_URL` names a reachable instance, and the
integration test in `internal/db` **fails rather than skips when `QUOTACORE_REQUIRE_INTEGRATION` is
set without one**, so a green pipeline cannot be a pipeline that quietly never talked to Postgres.
The gate is that variable and not `CI`, and the reason is worth recording because the first version
had it wrong: every hosted CI runner sets `CI` for its own reasons, including the build job that has
no database and must not be failed by a test it cannot run. A gate keyed on `CI` would have turned
every pull request red for a reason that has nothing to do with the change under review. The
integration job sets the variable explicitly, so the property that matters is unchanged.

**Definition of Done.**

1. ~~A clean clone builds and the unit tests pass on the reference platform, with no step that is not
   in a document.~~ Done on 2026-09-27. `go build ./...`, `go vet ./...` and `go test ./...` all
   exit 0 from a clean checkout, and `gofmt -l .` is empty, against the module
   `github.com/quotacore/quotacore` on Go 1.27.1. The only package is `internal/api`, holding the
   two health probes and nothing else, because the layout is fixed by
   [ARCHITECTURE.md](../../ARCHITECTURE.md) §3 and a health probe is the one route handler this
   phase is allowed. The steps are the table above.
2. ~~`docker compose up` on a clean checkout reaches a healthy state for the data store and Postgres,
   with no account, no signup and no egress ([mvp-scope.md](../product/mvp-scope.md) §1).~~ Done on
   2026-09-27. `docker compose up -d --wait` exits 0 from a checkout with no volume present, and
   both services report `Healthy`: Valkey 8.1.10 answering `PONG`, PostgreSQL 16.15 answering
   `pg_isready`. `CONFIG GET maxmemory-policy` returns `noeviction`, so the decided policy is in the
   file rather than in a comment, and `password_encryption` is `scram-sha-256`. The file starts two
   containers and nothing else, and no step precedes it.
3. ~~The CI job runs `tools/check-docs.ps1` and fails the build if it does not exit 0.~~ Done on
   2026-09-27. `.github/workflows/ci.yml` has a `docs` job whose only step after the checkout is
   `make check-docs`, which is the `Makefile` target that runs the script, so the command has one
   definition. No step sets `continue-on-error`, and nothing in the file can turn a non-zero exit
   into a zero one, so the exit code reaches the build. Every action is pinned to a commit SHA
   with the tag beside it. The claim was falsified rather than asserted: on a throwaway copy of the
   repository the unmodified tree exits 0, and with one undefined rule identifier appended to
   [observability.md](../architecture/observability.md) the same command exits 1 and names the
   identifier it could not resolve. The identifier is not written out here, because a reference to
   a rule that does not exist is itself a failure and this repository does not make exceptions for
   prose. Two limits, stated because a green tick would hide them. The workflow has never run,
   because nothing has been committed or pushed, so its behaviour on a GitHub runner is unverified;
   and `make` is absent on the machine this was written on, so the script was invoked directly
   rather than through the target the runner uses.
4. ~~The build produces an SBOM covering the Go module graph and the base image, and a licence scan
   fails the build on a new copyleft dependency in the runtime path without a decision record.~~
   **Amended on 2026-09-27, on the owner's decision, and the amendment is part of the record.** As
   written this item asked for two things, one of which no work inside `IP-01` could produce: an
   SBOM cannot cover a base image that does not exist here, because the scope above does not include
   an image and `IP-15` owns "the multi-stage image, the Compose file, and the quickstart". The
   base-image half was moved to `IP-15`'s item 11, which now carries NFR-C3 and NFR-C4 by name, and
   the item as it stands here is the Go module graph and the runtime path — the two things this phase
   actually owns. The item was not closed by deleting the half that was inconvenient: the
   obligation still exists, it sits with the phase that can meet it, and an SBOM that omits the base
   image describes a binary the operator cannot run, which is the one thing an SBOM is for.
   **Done.** `make sbom` writes a CycloneDX 1.6 document listing all 11 modules linked into the
   binary, each with a resolved SPDX licence identifier and the licence file it was resolved from, and
   `make license-check` gates the build on NFR-C4. The runtime path is read from `go version -m` on
   the built binary rather than from the module graph, because that is exactly the set of modules
   linked into the product: a module that is only a test dependency of a dependency is not in the
   product, and a scan that read the graph instead would gate on code that never ships. The gate was
   falsified in all three directions on a throwaway copy of the repository with a local
   copyleft-licensed module wired into the real binary: it exits 1 and names the dependency when
   there is no decision record, exits 0 when `tools/license-allowlist.txt` cites an `ADR-nnnn` for
   it, and exits 1 when an allowlist entry cites anything that is not a decision record. The
   classifier matches on the text of the grant rather than the SPDX name, because Go's BSD-3 text
   never says "BSD 3-Clause", and an unclassifiable licence fails closed rather than passing as
   unknown.
5. ~~The service starts, reports not-ready because no tenant exists, and refuses to start rather than
   accepting a default bootstrap key.~~ Done on 2026-09-27. The binary was built `CGO_ENABLED=0`
   and run on the compose network, because that is the only way to reach Postgres by the service
   name the documented connection strings use. It starts, applies the embedded migrations, and
   answers: `GET /healthz` is `200` with an empty body, `GET /readyz` is `503` with an empty body,
   and `GET /v1/balance` is `404` because no product route exists. The not-ready reason is logged
   and names `at least one tenant is known` alongside the two conditions this phase has not wired,
   so the 503 is a statement about state rather than a hard-coded answer. The refusal was
   demonstrated by running the same binary with
   `QUOTACORE_BOOTSTRAP_ADMIN_KEY=qc_admin_abc.def` and no acknowledgement: the process exits 1 and
   names the acknowledgement variable. The same run with the acknowledgement set starts, and
   `QUOTACORE_SHUTDOWN_GRACE` drains and stops it cleanly with exit 0, confirmed from the container's
   own `State.ExitCode` rather than inferred from a successful `docker stop`, which is a weaker claim
   than it looks. The idempotency-window refusal in
   [deployment.md](../architecture/deployment.md) §4.4 was demonstrated the same way, because it is
   the neighbouring refusal and shipping one without the other would leave DR-029's pin unenforced.
   **Running the service found two defects that reading it did not.** The readiness log closure
   mutated its remembered set on every probe with no lock, so two concurrent `/readyz` requests raced
   on it, and it also never logged the first observation, which is the transition an operator actually
   needs. Both are fixed, and `TestReadyFuncLoggingIsSafeForConcurrentProbes` asserts the log callback
   is never entered twice at once; with the lock removed it fails in 9 runs out of 10. The first
   version of that test passed 16 of 20 times and was worthless, because it asserted that the probe
   goroutines had been scheduled before the test told them to stop.
6. ~~No package contains product behaviour: no cycle arithmetic, no balance arithmetic, no route
   handler beyond a health probe.~~ Done on 2026-09-27, and asserted rather than asserted-about.
   Two tests, both of which fail. `TestRouterServesOnlyTheTwoHealthProbes` walks nine method-and-path
   combinations in both readiness states and requires `200`, `503` or `404`/`405` exactly: the two
   probes answer and every product path is absent. `TestNoProductBehaviour` is the source-level
   check [testing-strategy.md](../architecture/testing-strategy.md) §4 blesses for a property with no
   other observable: the module's packages are exactly `cmd/quotacore`, `internal/api` and
   `internal/db`, and no file in the module mentions the cycle, balance, deduction or script
   arithmetic the product exists to perform. Both were falsified on throwaway copies: an
   `internal/cycle` package fails the first, and a function named `AdvanceCycle` fails the second.
   `X-Request-Id` is deliberately absent and is `IP-02`'s, because NFR-S10 requires every response
   to carry one and the header's format is a contract decision this phase may not make; leaving it
   out is stated here rather than left as a silent contradiction.

**What is in the module, and the one thing that is not.** The three requirements the scope names
are in, each pinned to an exact version with `go.sum` committed (NFR-S7): `chi` v5.3.2
([ADR-0010](../decisions/0010-go-chi-spec-first-openapi.md)), and `pgx` v5.11.0 with `goose` v3.28.0
([ADR-0011](../decisions/0011-postgres-only-control-plane.md)). An earlier revision of this note
also claimed `go-redis` was named by this scope. It is not, and the claim was wrong: the scope names
`chi` and the embedded migrations, and the data-store client belongs to the data-plane phases, so
depending on it now would add a requirement the build does not use. The module has three direct
requirements and eight indirect ones, and `go mod tidy` agrees.

`go mod tidy` completes and is idempotent on this graph: run twice, `go.mod` and `go.sum` come back
byte-identical, so the committed pair is what tidy produces. An earlier revision of this note
recorded the opposite, that tidy could not complete because the module proxy could not serve
`modernc.org/sqlite`, which `goose`'s own tests require. That was a transient proxy outage on the
author's machine, not a property of the graph, and the note was wrong; the second failure of that
kind in this phase, the first being the same outage behind an earlier "proxy unreachable" claim,
which is why the CI step below is the authority rather than either note.

**The migration set is empty on purpose, and the empty version is still recorded.**
`internal/db/migrations` holds one file, `00001_reserved.sql`, which creates no object. The
control-plane schema is `IP-08`'s and [data-model.md](../architecture/data-model.md) owns every
table, so this phase supplies the runner, the advisory lock, the checksum record and the
current-version check, and nothing else. A version is still recorded, which is what makes "migrations
are current" in `/readyz` a real condition rather than a constant. The runner is up-only
structurally: it calls `goose.UpContext` and there is no down path to call, so ADR-0011's production
rule cannot be bypassed by configuration. `TestUpOnlyIsStructural` asserts that by reading this
package's own source and failing if a down, reset or redo call appears.

**Two things the development Compose file deliberately does not do.** Both were measured on
2026-09-27 against the running stack rather than assumed, and both are the reference deployment's
job rather than development's. The Postgres image writes a `pg_hba.conf` whose local and loopback
lines are `trust`; `password_encryption` is `scram-sha-256` and every non-loopback connection
requires it, and because the file publishes no port those lines are unreachable from the host.
Setting `POSTGRES_HOST_AUTH_METHOD=scram-sha-256` was tried and changed nothing — the variable is
present in the container environment and the `trust` lines are still written — so it is not in the
file, because a setting that does nothing is a defect. And the two images are pinned to the tags
[deployment.md](../architecture/deployment.md) names rather than to digests, because a digest has
to be resolved against a registry to be written down. The no-`trust` requirement in
[deployment.md](../architecture/deployment.md) §7 binds the reference compose file, which is
`IP-15`'s.

**Exit criteria.** `git clone && docker compose up && make test` works on a machine that has never
seen the project.

**Deferred.** The data store and Postgres versions are pinned, not floating; the image is not
rebuilt on every upstream release.

---

## IP-02 — The OpenAPI contract

**Status** `BLOCKED`, gated on `IP-01`.

**Objective.** A machine-readable contract that the code is written against, generated from the
specification rather than from the code.

**Scope.**
- `api/openapi.yaml` covering every route in
  [api-conventions.md](../architecture/api-conventions.md): the data plane, the control plane, the
  status and audit routes, and the health probes.
- The response envelope, `X-Request-Id`, the `Idempotency-Key` header, pagination, and the error
  shape.
- Every one of the 33 codes in [error-catalog.md](../product/error-catalog.md), with the status the
  catalogue assigns.
- `additionalProperties: false` on every request body.

**Specification references.**
- [ADR-0010](../decisions/0010-go-chi-spec-first-openapi.md): the documents are the source of truth
  and the contract is generated from them
- [api-conventions.md](../architecture/api-conventions.md)
- [error-catalog.md](../product/error-catalog.md)

**Dependencies.** Blocked by `IP-01`. Blocks `IP-04` and `IP-08`.

**Definition of Done.**

1. Every catalogue code appears in the contract with the catalogue's status, and the contract
   introduces no code the catalogue does not define. The checker enforces the first half; a contract
   test enforces the second.
2. `Example validation` passes against every example in the contract.
3. A request containing an undeclared field is a `400 validation_failed`, which is what
   `additionalProperties: false` means in practice.
4. Every response in the contract declares `X-Request-Id` (NFR-S10).
5. The running service serves the same document it was generated from, so the contract cannot drift
   from the binary in a way a test would miss.
6. The catalogue's endpoint-by-code matrix and the contract agree; the checker's catalogue check
   passes.

**Exit criteria.** The contract is reviewed, and no route in the specification is missing from it.

**Deferred.** `501 not_implemented` is specified for v0.2+ surfaces on an MVP build, and the
specification's own unanswered route questions stay unanswered until `IP-00` answers them.

---

## IP-03 — Cycle engine and boundary matrix

**Status** `BLOCKED`, gated on `IP-01`.

**Objective.** A pure function from an anchor and an interval to the current window, correct in
every time zone including the awkward ones, and proved by a table rather than by prose.

**Scope.**
- Window computation from the anchor, in UTC internally, with the tenant's zone applied at the
  boundary.
- Month-end clamping, 29 February degradation, daylight-saving normalisation, and the absence of a
  boundary for `never`.
- The current cycle index, monotonic and non-decreasing.
- The full boundary table from [cycle-engine.md](../architecture/cycle-engine.md) §6 as a test
  fixture, including the daylight-saving pair.

**Specification references.**
- [DR-001](../product/domain-rules.md) to [DR-010](../product/domain-rules.md): the server is the
  time authority, a window is a pure function, boundaries are wall-clock times, clamping, `never`,
  and missed boundaries
- [ADR-0003](../decisions/0003-lazy-monotonic-cycle-rollover.md)
- [ADR-0007](../decisions/0007-per-tenant-timezone.md)
- [ADR-0008](../decisions/0008-cadence-model-anchored-and-rolling.md)
- [T-07](../architecture/testing-strategy.md), the boundary table
- NFR-D8, clock-skew tolerance, and NFR-OPS8, the time-zone database

**Dependencies.** Blocked by `IP-01`. Blocks `IP-04` and `IP-05`. May run in parallel with `IP-02`.

**Definition of Done.**

1. Every row of the boundary table passes, asserted to the exact RFC 3339 instants in the document,
   with offsets.
2. The daylight-saving pair passes as a pair: a spring-forward day has 23 hours, an autumn day has
   25, and neither boundary is skipped (DR-007).
3. `reset_interval = never` has no boundary, and a balance survives a simulated boundary because
   none is applied, asserted by reading `PTTL` as `-1` (DR-008).
4. A property test holds that the window projected by a caller equals the window the engine
   computes, for every interval and a spread of instants.
5. The cycle index never decreases under any sequence of calls, including a host clock that jumps
   backwards by a day; a backwards jump yields the `STALE` return and no change, which is the
   correct behaviour of a monotonic guard and the reason the test exists.
6. Mislatched boundaries are not replayed: a service stopped for three cycles transitions once, to
   the current cycle (DR-009, T-06).
7. The Go time-zone database is present in the binary, so a boundary is correct without a system
   database and without egress.

**Exit criteria.** The engine is a pure function with no I/O, no clock read of its own, and a test
suite that would catch a change to any row of the table.

**Deferred.** Rolling windows (`IP-19`) are not implemented here. The engine is deliberately built so
that adding them later does not modify calendar arithmetic, which is the whole point of
[ADR-0008](../decisions/0008-cadence-model-anchored-and-rolling.md).

---

## IP-04 — Data-plane skeleton, keyspace and snapshot cache

**Status** `BLOCKED`, gated on `IP-02` and `IP-03`.

**Objective.** The runtime key layout, the configuration snapshot, and the invalidation path, with
the property that the request path never waits on the control plane.

**Scope.**
- The key layout, including the balance hash and the cycle keys, per
  [data-model.md](../architecture/data-model.md).
- The configuration snapshot: tenants, plans, entitlements, overrides, in memory, refreshed on
  invalidation and on an interval.
- The invalidation channel, carrying a version so a stale message cannot overwrite a newer snapshot.
- The connection pools, and the proof that a control-plane connection is not on the request path.

**Specification references.**
- [ADR-0001](../decisions/0001-control-plane-data-plane-split.md)
- [DR-036](../product/domain-rules.md), [DR-039](../product/domain-rules.md)
- [INV-X1](../architecture/data-model.md) and [INV-X7](../architecture/data-model.md)
- [request-lifecycle.md](../architecture/request-lifecycle.md) §3, the snapshot in detail
- NFR-D3, configuration propagation, and NFR-T7, snapshot memory

**Dependencies.** Blocked by `IP-02` and `IP-03`. Blocks `IP-05`.

**Definition of Done.**

1. The request path opens no control-plane connection. Proven by the "Postgres unreachable" chaos
   test: with Postgres stopped, cached tenants enforce normally, uncached tenants get
   `503 control_plane_unavailable`, and `quotacore_dbpool_wait_seconds` does not rise.
2. A `KEYS` or `SCAN` call is absent from the request path, asserted by a source check in CI, not by
   a code review.
3. An invalidation message with an older version than the current snapshot is discarded, and the
   discard is counted.
4. A control-plane change is visible to the data plane within the published propagation bound, and
   the measurement is `quotacore_invalidation_lag_seconds`.
5. The snapshot for the tenant-scale dataset from the load suite stays within its memory cap
   (NFR-T7).
6. A refresh failure leaves the previous snapshot serving, and the failure is visible.

**Exit criteria.** The service enforces against a snapshot it can hold in memory, and stopping
Postgres changes nothing about that.

**Deferred.** The background rollover worker (`IP-12` owns the ledger worker; the rollover worker
arrives with the transition in `IP-05`).

---

## IP-05 — The four atomic scripts

**Status** `BLOCKED`, gated on `IP-03` and `IP-04`.

**Objective.** Four Lua scripts, shipped in the binary, that make a balance decision and a mutation
one indivisible step in one round trip.

**Scope.**
- `balance`, `check`, `consume` and `refund`.
- The cycle transition inside `consume` and `refund`, so a rollover is never a separate operation a
  caller can forget.
- Key expiry at `window_end + 24h`, which is strictly later than any rollover could need it.
- The ceiling check, and the refusal to initialise a balance that does not exist.

**Specification references.**
- [ADR-0002](../decisions/0002-lua-scripts-for-atomicity.md) and
  [ADR-0005](../decisions/0005-refund-as-first-class-operation.md)
- [DR-010](../product/domain-rules.md), [DR-017](../product/domain-rules.md) to
  [DR-023](../product/domain-rules.md), [DR-037](../product/domain-rules.md),
  [DR-045](../product/domain-rules.md)
- [INV-C1](../product/state-machines.md) to [INV-C6](../product/state-machines.md),
  [INV-I1](../product/state-machines.md), [INV-X5](../architecture/data-model.md),
  [INV-X6](../architecture/data-model.md)
- [T-01](../architecture/testing-strategy.md), [T-04](../architecture/testing-strategy.md),
  [T-05](../architecture/testing-strategy.md), [T-08](../architecture/testing-strategy.md),
  [T-09](../architecture/testing-strategy.md), T-10
- [performance.md](../architecture/performance.md) §3, one round trip
- NFR-S8, static scripts shipped in the binary

**Dependencies.** Blocked by `IP-03` and `IP-04`. Blocks `IP-06`, `IP-07`, `IP-19`.

**Definition of Done.**

1. `T-01` passes under `-race`: 100 concurrent decrements of a balance of 100 succeed exactly 100
   times, and the balance is zero, never negative (DR-017).
2. `T-04` passes: `balance <= limit + bonus` at every instant, and an attempt to exceed it is
   `409 grant_exceeds_ceiling` or `409 refund_exceeds_grant`, never a negative ceiling.
3. `T-08` passes: a denial changes no balance, no cycle state and no ledger record (DR-025).
4. `T-09` passes: a store failure leaves either a complete deduction or none, and never a partial
   one.
5. `T-10` passes: a restart re-reads state and re-derives the window, and grants nothing.
6. A balance hash that does not exist is `503 service_unavailable` and is never initialised from
   request arguments. This is the fail-closed rule (DR-045), and the single most important line in
   the phase.
7. `PTTL` on a cycle key is `window_end + 24h`; `PTTL` on a `never` entitlement's balance is `-1`.
8. The scripts are static Go-embedded files, and a test asserts that no request field is ever
   assembled into script text.
9. The round-trip count for one `consume` is one, asserted in the client, not assumed.

**Exit criteria.** A `consume` is one `EVALSHA`, and the balance, the cycle window and the
idempotency record move together or not at all.

**Deferred.** Rolling windows, which will add keys and a fifth script without changing these four
(DR-006, [ADR-0008](../decisions/0008-cadence-model-anchored-and-rolling.md)).

---

## IP-06 — Idempotency prevention in the data plane

**Status** `BLOCKED`, gated on `IP-05`, and on the `Q-21` answer from `IP-00`.

**Objective.** A retrying client is charged once, decided in the data store rather than in the
application, so two instances cannot both charge.

**Scope.**
- The idempotency record, written inside the `consume` and `refund` scripts.
- Fingerprinting, so a key reused for a different operation is rejected rather than replayed.
- The 24-hour window and the key expiry that enforces it.
- The required header on the two endpoints that need it.

**Specification references.**
- [ADR-0004](../decisions/0004-idempotency-prevention-and-detection.md)
- [DR-026](../product/domain-rules.md) to [DR-030](../product/domain-rules.md)
- [INV-I2](../product/state-machines.md)
- [T-02](../architecture/testing-strategy.md), [T-03](../architecture/testing-strategy.md)
- NFR-D2, the idempotency window
- [idempotency-and-retries.md](../research/idempotency-and-retries.md)

**Dependencies.** Blocked by `IP-05`. Blocks `IP-07`. **Must not be split from `IP-05`**: the
record is written by the script, and a partially migrated keyspace is a correctness defect.

**Definition of Done.**

1. `T-02` passes: 50 concurrent requests with one `Idempotency-Key` produce one deduction and one
   stored response, and the 49 others replay it byte for byte (DR-027).
2. `T-03` passes: the same key with a different operation, amount or tenant is
   `409 idempotency_key_reuse` and changes no state (DR-028).
3. A `consume` or `refund` without the header is `400 missing_idempotency_key` (DR-026).
4. A property test holds that replaying a key any number of times, from any number of instances,
   applies the mutation at most once.
5. The window is 24 hours, the expiry on the record is derived from that window, and
   `QUOTACORE_IDEMPOTENCY_TTL` is pinned rather than tunable (DR-029).
6. The fast store runs `maxmemory-policy noeviction` for the whole key space, sized for the window
   per the `Q-21` decision (DR-048, ADR-0017); and the sizing is asserted against the projection in
   [deployment.md](../architecture/deployment.md) §5.
7. Detection is **not** implemented here. Nothing in the control plane knows about replay yet; that
   is `IP-11`, and the phase asserts that a replay is invisible from outside the data store.

**Exit criteria.** Duplicate delivery from any client, in any order, at any concurrency, charges
once — and that is a property of the store, not of a process.

**Deferred.** Control-plane detection and the reconciliation cross-check (`IP-11`).

---

## IP-07 — Data-plane endpoints

**Status** `BLOCKED`, gated on `IP-05` and `IP-06`.

**Objective.** The four runtime routes, with the request lifecycle the specification annotates, and
with the codes and labels the catalogue assigns.

**Scope.**
- `POST /v1/consume`, `POST /v1/check`, `POST /v1/balance`, `POST /v1/refund`.
- Authentication, scope checks, and `X-Request-Id` on every response.
- `Idempotency-Key` handling, and the error envelope.
- The metric labels the catalogue requires, including `outcome` and `feature_key`.

**Specification references.**
- [request-lifecycle.md](../architecture/request-lifecycle.md) §1 and §2
- [api-conventions.md](../architecture/api-conventions.md)
- [error-catalog.md](../product/error-catalog.md)
- [DR-024](../product/domain-rules.md), [DR-025](../product/domain-rules.md),
  [DR-037](../product/domain-rules.md), [DR-040](../product/domain-rules.md),
  [DR-043](../product/domain-rules.md)
- [mvp-scope.md](../product/mvp-scope.md) §3, the concurrency assertion
- NFR-S4, NFR-S10

**Dependencies.** Blocked by `IP-05` and `IP-06`. Blocks `IP-11` and `IP-13`.

**Definition of Done.**

1. The concurrency assertion in [mvp-scope.md](../product/mvp-scope.md) §3 passes: 200 concurrent
   decrements of a balance of 100 succeed exactly 100 times.
2. `check` mutates nothing, and a test asserts that the balance and the ledger count are identical
   before and after (DR-024).
3. `GET /v1/balance` returns `cycle_start` and `cycle_end` as RFC 3339 instants with offsets, in the
   tenant's zone, and `cycle_end` is `null` for a `never` entitlement (INV-C6).
4. `balance < amount` is `429 quota_exceeded`, and the service's own protective limit is
   `429 rate_limited`. A test distinguishes them, because conflating them is how a customer gets
   told to slow down for running out of quota (DR-040).
5. A data-store failure is `503 service_unavailable` within the bound in NFR-L4, with no partial
   state and no hang.
6. Every response carries `X-Request-Id`, and it is the same value in every log line for that
   request (NFR-S10).
7. `quotacore_errors_total`'s `code` label set equals the catalogue's code set, asserted by a test.
8. A runtime key cannot reach a control-plane route, and the rejection is `403 insufficient_scope`
   (NFR-S4).

**Exit criteria.** A customer can enforce against a running service, and every refusal names a
remedy.

**Deferred.** Nothing. The four routes are the product.

---

## IP-08 — Control-plane schema and migrations

**Status** `BLOCKED`, gated on `IP-02`.

**Objective.** The Postgres schema, applied by embedded up-only migrations, with the constraints
that make misuse unrepresentable rather than merely discouraged.

**Scope.**
- The eight tables: `features`, `plans`, `plan_entitlements`, `tenants`,
  `tenant_entitlement_overrides`, `api_keys`, `usage_events`, `audit_log`.
- Foreign keys and unique constraints, including the single `plan_id` column on `tenants` that
  makes a second simultaneous plan unrepresentable.
- Enums, which must match the filter values in
  [api-conventions.md](../architecture/api-conventions.md).
- Indexes for every documented query, and the sizing for the eviction question.

**Specification references.**
- [data-model.md](../architecture/data-model.md)
- [ADR-0011](../decisions/0011-postgres-only-control-plane.md) and
  [ADR-0013](../decisions/0013-single-tenant-flat-model-no-pii.md)
- [DR-011](../product/domain-rules.md), [DR-033](../product/domain-rules.md),
  [DR-034](../product/domain-rules.md), [DR-035](../product/domain-rules.md),
  [DR-044](../product/domain-rules.md)
- [INV-E2](../product/state-machines.md), [INV-F2](../product/state-machines.md),
  [INV-F3](../product/state-machines.md), [INV-U2](../product/state-machines.md),
  [INV-X3](../architecture/data-model.md)
- NFR-S2, no personally identifiable information, and NFR-OPS3, migrations

**Dependencies.** Blocked by `IP-02`. Blocks `IP-09`, `IP-12`.

**Definition of Done.**

1. Migrations apply from an empty database and upgrade from the previous version, in CI, on every
   build. There is no down migration; rollback is a restore, and the deployment document says so.
2. The DDL matches [data-model.md](../architecture/data-model.md) exactly, checked by comparing the
   schema the checker reads against the document rather than by eye.
3. Two simultaneous plans for one tenant are unrepresentable: `tenants.plan_id` is a single column
   with a foreign key, and a test asserts the database rejects the second (DR-011).
4. Deleting a plan referenced by a tenant is refused by a foreign key, and surfaced as
   `409 plan_in_use` (DR-035).
5. `unit` is immutable once an assignment exists, enforced by a constraint or a transaction, and a
   test asserts `409 feature_unit_immutable` (INV-F2).
6. `audit_log` has no `UPDATE` or `DELETE` statement anywhere in the application, asserted by a
   source check (INV-U2, DR-044).
7. `external_id` is validated so that no personally identifiable information is required, accepted or
   stored, and `@` is rejected (NFR-S2, DR-034).
8. Every enum value in the DDL is a value the API accepts and vice versa; the checker's schema
   check passes.

**Exit criteria.** The schema is the specification's schema, and the constraints that matter are
constraints rather than application logic.

**Deferred.** Retention volumes, which are configuration in `IP-15`.

---

## IP-09 — Control-plane API and API keys

**Status** `BLOCKED`, gated on `IP-08`.

**Objective.** The administrative surface, with hashed scoped keys, audit in the same transaction,
and no anonymous path after bootstrap.

**Scope.**
- Feature, plan, tenant, override, key, event-read, audit-read, status and health routes.
- Key creation, listing and deletion, with the plaintext returned exactly once.
- The audit write, inside the same transaction as the mutation it records.
- Soft delete, archive, suspension and resumption.

**Specification references.**
- [ADR-0012](../decisions/0012-hashed-scoped-api-keys.md)
- [DR-031](../product/domain-rules.md) to [DR-035](../product/domain-rules.md),
  [DR-041](../product/domain-rules.md), [DR-043](../product/domain-rules.md)
- [INV-K1](../product/state-machines.md) to [INV-K3](../product/state-machines.md),
  [INV-T1](../product/state-machines.md) to [INV-T3](../product/state-machines.md),
  [INV-X3](../architecture/data-model.md)
- [error-catalog.md](../product/error-catalog.md), including `resource_version_conflict`
- NFR-S1, NFR-S4, NFR-S5, NFR-S9, NFR-S10

**Dependencies.** Blocked by `IP-08`. Blocks `IP-10`, `IP-14`.

**Definition of Done.**

1. No API key is recoverable in plaintext from any store, log or metric, and a test asserts it by
   scanning the database, the log output and the metric exposition after issuance (NFR-S1).
2. A key's plaintext is returned exactly once, never again, and there is no recovery path
   (INV-K3).
3. Rotation is an explicit pair of calls; nothing rotates implicitly, and the old key keeps working
   until it is deleted (INV-K1).
4. Every administrative mutation writes an audit row in the same transaction, with before and after
   values and the attributable key (DR-041, NFR-S5). A test aborts the transaction and asserts no
   audit row survives.
5. A suspended tenant is denied on the data plane, and suspension changes no balance and no cycle
   window (DR-032, INV-T2).
6. Archiving a feature preserves its balances and their history; the balance stays readable until
   the cycle ends, and enforcement stops with `403 feature_archived` (DR-016, INV-F1).
7. The bootstrap admin key cannot be created through the API, and the service refuses to start
   while an unacknowledged default key would be in use (INV-K2, NFR-S9).
8. No secret appears in a log line (DR-043), and a test scans a full request's log output for every
   request field name.
9. Concurrent edits to one control-plane resource produce `409 resource_version_conflict` rather
   than a lost write.

**Exit criteria.** Every administrative action is attributable, and none of them is anonymous,
unrecoverable or unaudited.

**Deferred.** Per-tenant runtime keys, which arrive in `IP-20`.

---

## IP-10 — Plan lifecycle and cycle-boundary semantics

**Status** `BLOCKED`, gated on `IP-09` and on the `Q-01`, `Q-02`, `Q-04` and `Q-14` answers from
`IP-00`.

**Objective.** The operations that change what a tenant is entitled to, each with the cycle
semantics the specification fixes, and each answered by the rules `IP-00` produced.

**Scope.**
- Feature and plan create, edit, archive.
- Plan apply, `apply-now` with its confirmation above the tenant count the `Q-04` answer sets.
- Tenant create, plan assignment, plan change, grant, set, force-rollover.
- The plan-impact route from `Q-14`.

**Specification references.**
- [ADR-0006](../decisions/0006-plan-edits-at-next-cycle-boundary.md)
- [DR-012](../product/domain-rules.md) to [DR-016](../product/domain-rules.md),
  [DR-018](../product/domain-rules.md), [DR-019](../product/domain-rules.md),
  [DR-020](../product/domain-rules.md), [DR-041](../product/domain-rules.md)
- [INV-E1](../product/state-machines.md), [INV-P1](../product/state-machines.md) to
  [INV-P3](../product/state-machines.md), [INV-TE1](../product/state-machines.md)
- [T-05](../architecture/testing-strategy.md), [T-06](../architecture/testing-strategy.md)
- [use-cases.md](../product/use-cases.md), [state-machines.md](../product/state-machines.md)

**Dependencies.** Blocked by `IP-09`. Blocks `IP-16`, and the v0.2 phases.

**Definition of Done.**

1. A plan edit changes no current balance, and takes effect at each tenant's own next boundary, not
   at a shared instant (DR-014, T-05, [ADR-0006](../decisions/0006-plan-edits-at-next-cycle-boundary.md)).
2. A plan change is a hard reset: a new anchor, a new cycle, the new limit only, and a
   `plan_changed` event (DR-013, and the `Q-01` and `Q-02` answers).
3. A grant is cycle-scoped and does not survive a plan change, because a plan change starts a new
   cycle.
4. An override can never exceed the plan allowance unless it is a grant, and the step ordering that
   makes that true is asserted (DR-015, INV-TE1).
5. `apply-now` re-anchors, requires a reason, is audited, and above the tenant count the `Q-04`
   answer sets, returns `409 confirmation_required` with a token rather than re-anchoring
   everything at once.
6. The impact route answers, for a proposed edit, how many tenants are affected and the earliest
   affected `cycle_end`, without changing anything.
7. A feature absent from the plan is `403 feature_not_in_plan`, never a zero balance
   (DR-012). The distinction is a customer-facing one and is tested in both directions.
8. Archiving a plan alters no assigned tenant's balance or cycle (INV-P1).
9. `T-06` passes for a plan applied across a service outage, so no tenant's transition is replayed
   or skipped.

**Exit criteria.** An operator can change an entitlement and can predict, before committing, which
tenants it affects.

**Deferred.** Nothing. Boolean entitlements and rolling windows change this surface in v0.2 and are
separate phases.

---

## IP-11 — Idempotency detection and reconciliation

**Status** `BLOCKED`, gated on `IP-07` and `IP-12`.

**Objective.** The control plane's half of the idempotency claim: notice when a data-plane record
and the ledger disagree, rather than preventing it again.

**Scope.**
- The reconciliation job, comparing applied mutations against ledger events.
- The read-only mode used during a rebuild.
- The reporting of an unreconcilable record, and the operator action.

**Specification references.**
- [ADR-0004](../decisions/0004-idempotency-prevention-and-detection.md)
- [DR-030](../product/domain-rules.md), [DR-042](../product/domain-rules.md)
- [INV-C4](../product/state-machines.md), [INV-U1](../product/state-machines.md),
  [INV-X2](../architecture/data-model.md)
- [T-11](../architecture/testing-strategy.md)
- [consistency-and-recovery.md](../architecture/consistency-and-recovery.md) §4 and §5
- [mvp-scope.md](../product/mvp-scope.md) §1, the disputed-deduction requirement

**Dependencies.** Blocked by `IP-07` and `IP-12`. Blocks nothing in v0.1; blocks `IP-16`.

**Definition of Done.**

1. `T-11` passes: within a cycle, every balance equals its opening allowance plus the sum of its
   deltas, and a mismatch is found and reported (DR-042, INV-C4).
2. A data-plane record with no corresponding ledger event is detected, and
   `quotacore_event_reconcile_mismatch_total` rises. Detection, not prevention: the prevention
   assertion from `IP-06` still holds.
3. During a rebuild the service is in read-only mode for its whole duration, and a test asserts it.
4. A disputed deduction is resolvable from the ledger and the audit trail alone, which is the
   [mvp-scope.md](../product/mvp-scope.md) §1 requirement stated as a test.
5. Every tenant the rebuild flags as unreconcilable really was, and every tenant it claims to have
   reconstructed matches exactly. Accuracy in both directions, not just in the happy one.
6. No tenant is granted a full allowance by any path at any point during a rebuild (DR-045). This is
   the assertion the restore drill exists for, and it is the one most likely to be weakened.

**Exit criteria.** A customer disputing a deduction gets an answer from the ledger, and the
reconciliation says which tenants cannot be answered.

**Deferred.** Nothing.

---

## IP-12 — Usage ledger, writer and archiver

**Status** `BLOCKED`, gated on `IP-08`.

**Objective.** One signed event per applied mutation, written off the request path, retained by
policy, and never able to slow enforcement down.

**Scope.**
- The event payload and its reconciliation identity.
- The in-process queue, the writer, and the drop policy when the queue is full.
- The archiver, and retention as a background job rather than an API call.
- The at-least-once contract, including the `event_id` consumers deduplicate on.

**Specification references.**
- [ADR-0016](../decisions/0016-usage-event-ledger.md)
- [DR-042](../product/domain-rules.md), [DR-044](../product/domain-rules.md)
- [INV-U1](../product/state-machines.md) to [INV-U3](../product/state-machines.md),
  [INV-W1](../product/state-machines.md) to [INV-W3](../product/state-machines.md),
  [INV-WK1](../product/state-machines.md), [INV-WK2](../product/state-machines.md)
- [consistency-and-recovery.md](../architecture/consistency-and-recovery.md) §4.5
- [FS-10](../product/error-catalog.md), NFR-D5, NFR-T8, NFR-C6

**Dependencies.** Blocked by `IP-08`. Blocks `IP-11` and `IP-14`.

**Definition of Done.**

1. Every applied mutation appends exactly one event, and `delta <> 0` is a constraint rather than a
   convention (INV-U1, INV-X2).
2. The events for a tenant-feature sum to the difference between its opening and closing balances
   for the cycle. A test asserts it, which is the property that makes the ledger worth keeping.
3. The ledger-saturation test passes: with the queue full, events are dropped,
   `quotacore_event_sink_dropped_total` rises, and **enforcement latency is unchanged**
   (FS-10, INV-W2). The test asserts a negative, and the negative is the point.
4. Duplicate delivery is possible by design and every payload carries an `event_id` for the consumer
   to deduplicate on; a test delivers each event twice and asserts the consumer's result is
   unchanged (INV-W1).
5. A dead-lettered event is visible and replayable, never silently dropped (INV-U3).
6. Retention is applied by a background job on a configured schedule, and the job is what removes
   rows (DR-044, NFR-C6). No API call deletes an event.
7. The writer holds a runtime-scoped credential and has no direct database access (INV-WK2).
8. Deleting or stalling the writer, or running it twice, changes no enforcement outcome
   (INV-WK1).

**Exit criteria.** The ledger is a best record of what happened, not a dependency of whether it
happened.

**Deferred.** Export to an external sink beyond the configured file or stream, and webhooks
(`IP-21`–`IP-23`).

---

## IP-13 — Observability

**Status** `BLOCKED`, gated on `IP-07`.

**Objective.** Every metric, log and probe the specification names, with cardinality that stays
bounded and alerts that name a runbook.

**Scope.**
- The enforcement, error, dependency, cache, ledger, HTTP and process metrics in
  [observability.md](../architecture/observability.md) §1.
- Structured logs, with `X-Request-Id` on every line and no personally identifiable information
  anywhere in telemetry.
- `/healthz`, `/readyz` and the status route.
- Alerts, and the operator runbooks they point at.

**Specification references.**
- [observability.md](../architecture/observability.md) §1 to §7
- [mvp-scope.md](../product/mvp-scope.md) §3, the observability capability
- NFR-O1 to NFR-O7, NFR-S6, NFR-S10
- [consistency-and-recovery.md](../architecture/consistency-and-recovery.md) §7, invariants restated
  as alerts
- [SECURITY.md](../../SECURITY.md) §2.1, what is never logged

**Dependencies.** Blocked by `IP-07`. Blocks `IP-15`.

**Definition of Done.**

1. Every metric named in [observability.md](../architecture/observability.md) §1 is emitted, checked
   by name against the document, so a removed metric fails the build rather than a dashboard.
2. `quotacore_errors_total`'s `code` label set equals the catalogue's code set.
3. The cardinality rules hold under the tenant-scale load shape: no unbounded label values, and
   `external_id` is not a label (NFR-O2).
4. Every log line is JSON, carries the request's `X-Request-Id`, and contains no personally
   identifiable information and no secret (NFR-O3, NFR-O6, DR-043).
5. `/healthz` and `/readyz` answer differently and correctly: liveness survives a data-store outage,
   readiness does not, because a service that cannot enforce is not ready.
6. Alerts exist for the three conditions the specification calls P0 or P1: `rollover_stale_total`,
   `balance_key_missing_total` and `ceiling_violation_total`, each naming the runbook that answers
   it.
7. Every runbook is followable by a person who did not write it. Reviewed by someone else, which is
   the only test that exists for a runbook.
8. The process runs unprivileged, on a non-root port, with a read-only filesystem outside its data
   volumes (NFR-S6).

**Exit criteria.** An operator who has never seen the service can tell whether it is enforcing,
and what to do about it.

**Deferred.** Tracing depth, and per-customer dashboards.

---

## IP-14 — The `quotacore` CLI

**Status** `BLOCKED`, gated on `IP-09` and `IP-12`.

**Objective.** One binary that administers the service, so an operator never needs a database client
or a copy-pasted `curl`.

**Scope.**
- `init`, `plan`, `feature`, `tenant`, `key`, `grant`, `set`, `rollover` and `events`.
- `quotacore serve` and `quotacore worker`, so the shipped binary is both the server and the
  administration tool.
- Exit codes distinct from HTTP statuses, and no secret printed.

**Specification references.**
- [mvp-scope.md](../product/mvp-scope.md) §2, the v0.1 CLI row
- [api-conventions.md](../architecture/api-conventions.md), which the CLI's calls obey
- [error-catalog.md](../product/error-catalog.md), for the exit-code mapping
- [request-lifecycle.md](../architecture/request-lifecycle.md) §1, when a CLI call is not a
  customer request
- NFR-S1, NFR-S4, NFR-S10

**Dependencies.** Blocked by `IP-09` and `IP-12`. Blocks `IP-15`.

**Definition of Done.**

1. Every command maps to exactly one documented endpoint or one local operation, and the mapping is
   a table in the document rather than an inference from the flag names.
2. `grant`, `set` and `force-rollover` are available from the CLI, because those are the three
   operations an operator reaches for when a customer disputes a deduction
   ([mvp-scope.md](../product/mvp-scope.md) §1).
3. A disputed deduction is fully investigable from the CLI alone, with no database access. A test
   performs a dispute and resolves it using only `quotacore events` and `quotacore tenant`.
4. No command prints a secret, and `quotacore key create` prints a plaintext exactly once, with a
   warning that it cannot be shown again (INV-K3).
5. Exit codes distinguish client error, not found, conflict, exhausted quota, rate limited,
   unavailable and internal error, and the mapping to the catalogue is documented.
6. A non-interactive invocation is possible for every command, and CI uses it, so the CLI is
   exercised by a test rather than by a human.
7. The CLI and the server are one binary, and a single binary is what the reference image ships
   (NFR-OPS6).

**Exit criteria.** The whole administrative surface is reachable from one executable, and CI proves
it.

**Deferred.** A shell-completion generator, and a configuration-file credential store beyond an
environment variable.

---

## IP-15 — Reference deployment and restore rehearsal

**Status** `BLOCKED`, gated on `IP-13`.

**Objective.** One `docker compose up`, no account and no external dependency, plus a restore
procedure that has actually been performed.

**Scope.**
- The multi-stage image, the Compose file, and the quickstart.
- Configuration keys, limits, timeouts and the retention defaults.
- Sizing, including the data store's memory projection for the idempotency window from the `Q-21`
  decision.
- Backup, restore, upgrade, rollback and the restore drill.

**Specification references.**
- [deployment.md](../architecture/deployment.md) §1 to §12
- [mvp-scope.md](../product/mvp-scope.md) §1, all eight steps
- [testing-strategy.md](../architecture/testing-strategy.md) §9, the restore drill
- [consistency-and-recovery.md](../architecture/consistency-and-recovery.md) §4
- [ADR-0009](../decisions/0009-redis-protocol-valkey-default.md),
  [ADR-0011](../decisions/0011-postgres-only-control-plane.md),
  [ADR-0017](../decisions/0017-noeviction-and-duplicate-reversal.md)
- NFR-A1 to NFR-A6, NFR-D4, NFR-D6, NFR-D7, NFR-T9, NFR-T10, NFR-OPS1 to NFR-OPS8, NFR-C5, NFR-C6

**Dependencies.** Blocked by `IP-13` and `IP-14`. Blocks `IP-16`.

**Definition of Done.**

1. `docker compose up` on a clean machine reaches a working service with no account, no signup, no
   egress and no manual step, and the quickstart in the README is executed by a script in CI
   ([mvp-scope.md](../product/mvp-scope.md) §1, NFR-OPS1).
2. The service makes no outbound connection of any kind in the MVP, asserted by a test that runs it
   with egress blocked (NFR-S3, NFR-C5).
3. Graceful shutdown completes in-flight requests, leaves no partial state and finishes within the
   bound in NFR-A5.
4. A restart loses no state: NFR-D6 and NFR-A6 hold across a kill and a clean stop.
5. The restore drill in [testing-strategy.md](../architecture/testing-strategy.md) §9 passes
   end to end, including its step 6 — no tenant is granted a full allowance by any path at any
   point (DR-045). The drill runs nightly against a real stack.
6. The data store's memory projection for the idempotency window is measured, not estimated, and it
   is the figure in [deployment.md](../architecture/deployment.md) §5. The measurement is the per-record
   footprint of an idempotency entry at the sustained-throughput figure in NFR-T1, held in a scratch
   instance that is then destroyed; the projection in §5 is a floor until it exists, and this item
   replaces the floor with a number (NFR-T9).
7. The store runs `maxmemory-policy noeviction`, and the rehearsal demonstrates the two states that
   policy creates: at the configured `maxmemory` the service returns `503 store_capacity_exhausted`
   and rejects writes, and a full flush of the idempotency namespace is never observed (DR-048,
   NFR-T9, T-13).
8. Total store loss is rehearsed, and the automatic reversal of a duplicate detected after it is
   observed end to end: an idempotency record lost to a flush, a replayed request, a detection, a
   `reversal` API-key event and a balance that returns to its prior value (DR-049, T-14).
9. An upgrade path from the previous release is documented and exercised, and rollback is a restore
   (NFR-OPS7).
10. The image runs unprivileged, on a non-root port, with a read-only root filesystem (NFR-S6).
11. The build produces an SBOM covering the base image and the Go module graph, and the licence scan
    that gates it covers the whole shipped runtime rather than the module graph alone. This item was
    moved here from `IP-01` on 2026-09-27, and the reason is recorded rather than quietly dropped:
    `IP-01`'s own DoD asked for an SBOM over "the Go module graph and the base image", but `IP-01`
    does not own an image and never will, because the scope above does not include one. The
    module-graph half of that item is met in `IP-01` and this is the half that could not be. An SBOM
    that omits the base image describes a binary the operator cannot run, which is the one thing an
    SBOM is for, so the obligation belongs with the phase that builds the thing (NFR-C3, NFR-C4).

**Exit criteria.** The third claim is demonstrated, and a restore has been performed by someone
following a document rather than by someone who remembered the commands.

**Deferred.** Kubernetes manifests, TLS termination, and multi-host high availability, all of which
[deployment.md](../architecture/deployment.md) §8 discusses and none of which the MVP requires.

---

## IP-16 — Full suite, the three claims, v0.1 exit

**Status** `BLOCKED`, gated on `IP-15`.

**Objective.** Run everything, demonstrate the three claims, and tick every line of the release
checklist.

**Scope.**
- All eleven named correctness tests, the load and soak suite, the chaos suite, and the restore
  drill, in CI and nightly.
- The three claims, each demonstrated by the phase that owns it.
- Every item in [mvp-scope.md](../product/mvp-scope.md) §3.

**Specification references.**
- [testing-strategy.md](../architecture/testing-strategy.md) §2 to §9
- [non-functional-requirements.md](../architecture/non-functional-requirements.md), every
  measurement
- [mvp-scope.md](../product/mvp-scope.md) §3, capability-level completion
- [PROJECT.md](../../PROJECT.md) and [ROADMAP.md](../../ROADMAP.md), for the status this phase
  changes

**Dependencies.** Blocked by every other v0.1 phase. Blocks `IP-17`, `IP-21`, `IP-24` and `IP-26`.

**Definition of Done.**

1. `T-01` to `T-11` all pass under `-race` in CI, and each is reported by name.
2. Claim 1: p99 at or below NFR-L2 at the sustained load in NFR-T1, with no database connection on
   the request path, and p99 stable rather than creeping across the run. The "Postgres
   unreachable" chaos test proves the second half.
3. Claim 2: `T-02` and `T-03` pass, the window is 24 hours, and the control plane detects a
   discrepancy without being able to cause one.
4. Claim 3: `docker compose up` on a clean machine, executed by CI, with no account and no external
   dependency.
5. The chaos suite is green, including the clock-jump pair and the data-store flush, and the
   `balance_key_missing_total` test asserts no full allowance is granted.
6. The soak run shows no leak, no goroutine growth, no file-descriptor leak, and no clock or counter
   drift.
7. Every NFR with a measurement has that measurement recorded, with the environment it was taken in.
8. Every line of [mvp-scope.md](../product/mvp-scope.md) §3 is ticked, or the gap is named.
9. `tools/check-docs.ps1` exits 0, and `PROJECT.md` and `ROADMAP.md` state v0.1 as done with the
   evidence.
10. The change is recorded in [CHANGELOG.md](../../CHANGELOG.md).

**Exit criteria.** Every claim in the README has a measurement, every capability in the scope has a
test, and a reader who doubts the product can find the evidence in one place.

**Deferred.** Nothing from v0.1. A shortfall here is a reason the release does not ship, not a
phase for later.
