# Changelog

The specification is versioned separately from the product. Nothing here describes shipped software:
the first release of Quotacore has not been built, and [PROJECT.md](PROJECT.md) explains why.

Dates are ISO-8601. Entries follow [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
specification uses [semantic versioning](https://semver.org/spec/v2.0.0.html) on documents rather
than on code. A change that alters existing behaviour is recorded as `Changed` even when no code
exists, because a future implementer needs to know which statements are new.

## [Unreleased]

### Added

- **The v0.1 evidence and the named-test report (`IP-16`).** [docs/release/v0-1-evidence.md](docs/release/v0-1-evidence.md)
  holds, in one place, each claim, NFR, named test and scope line with what proves it. Its table of
  named tests is the catalogue `tools/testreport` reads from `go test -json`: the integration suite
  now runs under `-race` in CI, and the step fails if a named test of its suite did not run, was
  skipped or failed; the reference suite is reported the same way. `T-15`, a consume raced against
  a tenant delete over 40 randomised interleavings, is the fifteenth row's first test.
- **The reset worker (`IP-16`, `Q-13`).** `internal/worker` runs inside the service while
  `QUOTACORE_WORKER_ENABLED` is true (the default), every `QUOTACORE_RESET_SCAN_INTERVAL` (60 s) in
  pages of `QUOTACORE_RESET_BATCH_SIZE` (500): each cached tenant-feature whose cycle began since
  the previous scan is advanced through the data plane's new `Advance`, which is the check script for
  an amount of zero, the request path's own transition. A tenant this instance serves is therefore
  rolled at its boundary, and its ledger opening written then, rather than at its next request. The
  worker has no route to SQL, the store or the scripts except that call (INV-WK2, a source check),
  pauses in read-only mode, and loses a race with a request harmlessly; a test drives both orders and
  a simultaneous one on the same boundary and finds one transition and one event every time.
- **The property tests (`IP-16`, testing-strategy §3).** `pgregory.net/rapid`, a test-only
  dependency, generates operation sequences (consume, refund, grant, set, an override applied now, a
  plan edit, boundaries passing, a caller behind the store, a replayed key) against one balance hash
  through the real scripts, and a model that knows only the rules predicts every field after every
  step: the ceiling, a never-decreasing index, a denial that changes nothing byte for byte, one
  application per key, no expiry, and DR-042's opening-plus-deltas identity per cycle. A failure
  shrinks to its shortest sequence. A second property renders random boundaries in six zones and
  parses them back. CI runs 100 sequences of each; the nightly run passes `-rapid.checks`.
- **The load, soak and chaos suites (`IP-16`, testing-strategy §5 and §6).** `test/harness` starts
  the Compose stack with `compose.perf.yml`, which pins the service and both stores to two CPUs
  (NFR §0's reference host) and sizes the data store by deployment §5, seeds it through the admin
  API, and drives it with an open-loop generator that measures from each request's release, so a
  stall is counted rather than omitted. `test/load` runs every row of §5: warm consume and check
  latency, gated on the service's own time from its access log with the client's beside it;
  2,000 a second for 30 minutes with D16's proof that no request touches Postgres (commits,
  connections and pool wait) and the store's growth per idempotency record against `IP-15`'s
  figures; 10,000 a second for a minute; 50,000 tenants on a plan of 100 features; ledger
  saturation and throughput (NFR-T8); a Postgres outage under load; startup to ready; and a soak
  that asserts slopes rather than final values. The chaos rows a proxy can inject run in process
  under `-race` on every pull request: one balance hash deleted, the data store delayed by 400 ms
  and by 2 s, and pub/sub muted. `TestQ08ConfigurationPropagation` measures NFR-D3 over 200
  changes between two instances. `test/chaos` fills the disk under Postgres and moves the host's
  clock 24 h each way. T-11 takes `QUOTACORE_T11_OPERATIONS` for its 5,000-operation run.
- **The two commands after a Postgres restore (`IP-16`, `Q-28`).** `quotacore config bump` records
  a new configuration version against every tenant and publishes the invalidation, so every
  instance re-reads the restored configuration at once (consistency-and-recovery §4.2, step 4).
  `quotacore audit note --reason` appends an audit row about the installation itself, recording the
  truncation a restore leaves in the history (step 6). `audit_log.subject_type` and the contract's
  `AuditEntry.subject_type` gain `system` for it, in migration 12. The restore runbook names both
  commands.
- **NFR-L7 measured (`IP-16`).** `TestNFRL7RolloverOverhead` compares the median consume that
  crosses a boundary with one that does not, against the real store, on every commit.
- **The heavy workflows (`IP-16` D11).** `heavy.yml` runs load, chaos, the clock pair, T-11 and the
  properties at scale, and the soak, each job recording its environment with
  `tools/environment.sh` and uploading its figures. `nightly.yml` calls it every night and adds
  the soak on Sundays; `full-suite.yml` calls it on a pull request labelled `full-suite` (and the
  soak with `soak`), because a schedule runs only from main. `release.yml` publishes and signs an
  image from a `v*` tag (`Q-27`) and is a dry run otherwise.
- **The reference deployment (`IP-15`).** A two-stage [Dockerfile](Dockerfile): a builder pinned
  by digest, and a `scratch` runtime holding the static binary and `/tmp`, uid 65532, entrypoint
  `quotacore`, healthcheck `quotacore healthcheck`. [docker-compose.yml](docker-compose.yml) is now
  the whole reference stack: the service read-only, without capabilities, under
  `no-new-privileges`, on host loopback only, with a 40 s stop grace; Postgres 16 and Valkey 8
  pinned by digest; Postgres `scram-sha-256` only, with an init script that creates the
  non-superuser `quotacore` role the service connects as; Valkey `noeviction` under a `maxmemory`,
  with no persistence; and the stores on an internal network. `docker-compose.metrics.yml`
  publishes the metrics listener for an outside scraper.
- **The quickstart in the README**, identical to deployment.md §2 and run verbatim on a clean
  runner by `tools/quickstart.sh`, which then checks J-1 step 6.
- `quotacore healthcheck`, which asks the service's own `/readyz` and exits 0 only on `200`.
- **The start-up check DR-048 asked for.** The service reads `maxmemory_policy` from `INFO memory`
  and refuses to start against anything but `noeviction`; the prober reads it again, and a policy
  changed under a running instance makes it unready as `data store refuses to evict`.
- `test/reference`, the suite against the Compose stack and the built image: the restore drill and
  T-14 with the real `quotacore rebuild`; T-13 at a 12 MB `maxmemory`; a stop under load and a
  `SIGKILL`; a packet capture of the service's namespace with a negative control
  (no egress); an upgrade from the previous build and a rollback by restore; the image's hardening,
  size and layers; the SBOM; and two file-only tests, every Compose variable documented and the
  README's quickstart equal to the guide's.
- `tools/license-scan.ps1 -Image`: the licence gate over a whole image, every OS package's
  copyright text and every file no package owns, and a CycloneDX SBOM naming the base, the OS
  packages (none, for `scratch`, said explicitly) and the modules. `make sbom IMAGE=…` runs it. A
  distroless control image fails it, naming `base-files`, `netbase` and `ca-certificates`.
- `tools/idemsize`, which measures an idempotency record's cost in the fast store and a ledger
  row's in Postgres; its report is
  [ip-15-idempotency-footprint.md](docs/research/ip-15-idempotency-footprint.md).
- CI: a `reference` workflow (the image, the quickstart, the suite, the image SBOM) on every pull
  request, and `nightly.yml`, which runs it on a schedule once it is on `main`.
- deployment.md §4.7, the Reference Compose file's own variables, and Q-27 (publishing and signing
  the image), Q-28 (no command for consistency-and-recovery §4.2's steps 4 and 6) and Q-29
  (`rebuild --set` keeps an estimated limit) in the register, each with a default.

- **The `quotacore` CLI (`IP-14`).** `internal/cli`: one command for each of the contract's 32 admin
  operations, read from the contract by `operationId` so a route cannot drift from its command;
  `grant`, `set` and `rollover` at the top level; `init`, the quickstart's five calls, which asks
  only at a terminal and takes `--yes`; and `key mint`, which prints a new admin token for
  `QUOTACORE_BOOTSTRAP_ADMIN_KEY` and touches nothing. Each request is the generated contract type,
  validated before it is sent. The admin key comes only from `QUOTACORE_ADMIN_KEY` (falling back to
  the bootstrap key), never a flag. No write is ever retried, and `plan apply` never re-issues
  itself with a confirmation token.
- [docs/architecture/cli.md](docs/architecture/cli.md): the mapping table, the environment, the
  exit codes by catalogue code, and what a command never does. Tests hold the table to the binary
  and the contract, and the exit codes to error-catalog.md §2, both ways. deployment.md §4.6 lists
  `QUOTACORE_URL` and `QUOTACORE_ADMIN_KEY`.
- CLI tests against a real stack: every command makes exactly its row's call; grant, set and
  rollover; a dispute investigated and corrected from stdout alone; no secret in any output; exit
  codes from provoked answers; and the built binary serving and administering itself.

- **Observability (`IP-13`).** `/metrics` on its own listener, `QUOTACORE_METRICS_ADDR`
  (`127.0.0.1:9090`), serving every family observability.md §1 names: the store's latency, error
  classes, script cache and memory from a hook on the store client; `datastore_up`,
  `controlplane_up` and `quotacore_clock_skew_seconds` from a prober; the rollover, stale-rollover
  and ceiling counters from the scripts' answers; `quotacore_reanchors_pending`; and the HTTP,
  build and process series. A test parses §1 and fails on a family or label missing in either
  direction, and another on a series count that grows with tenants.
- `ops/alerts/quotacore.rules.yml`, one Prometheus rule per §5 row, and its promtool cases, which CI
  runs from a pinned, checksummed release. A P0 or P1 rule names its runbook.
- `docs/runbooks/`: an index, eleven runbooks with the same eight headings, and a review record. A
  test holds each to the commands the binary has and the routes the contract has.
- Log proofs: a workload of canaries in every customer field and both connection strings, after
  which no line may carry one, every request line carries its own id and every other line a `job`;
  and a source check of every key a call site hands a logger.
- `Q-25` (no configured `cycle_index` ceiling in v0.1) and `Q-26` (clock skew above 1 s, the alert's
  threshold).

- **Idempotency detection and reconciliation (`IP-11`).** `internal/reconcile` checks a
  tenant-feature's ledger row by row and against its balance hash, with no I/O of its own. An
  in-service pass, every `QUOTACORE_RECONCILE_INTERVAL` under one advisory lock, reverses each
  duplicate deduction once under a derived idempotency key, verifies what changed since its
  checkpoint, confirms each suspect with a second read, records it in `reconcile_findings`, and
  counts it once in `quotacore_event_reconcile_mismatch_total`. It never corrects a balance.
- Read-only mode (`QUOTACORE_STARTUP_MODE=readonly`): the store's own hook refuses every tenant-key
  write, every route that can write one answers `503 service_unavailable`, and status reports
  `mode`. A Postgres advisory-lock fence, held shared by each normal instance and exclusively by a
  rebuild, keeps a normal instance from writing beside a rebuild.
- The binary's first local operations: `quotacore verify-tenant`, `verify-all`, `reconcile-report`
  and `rebuild`. `rebuild` classifies each tenant-feature from the ledger as exact, unverified,
  unreconcilable or present, writes only the exact ones with the create-only `rebuild.lua`, and
  writes the rest only on an audited `--accept` or `--set`.
- Migration `00011`: `usage_events.reconciles_key`, the dead-letter resolution, the findings and
  their checkpoint, and the ledger's own loss record (`usage_event_gaps`, `ledger_sessions`), so a
  crashed writer or a dropped event makes the cycles it covers unverified rather than exact.
- Integration tests for the six DoD items, T-11 and T-14.

- **The usage ledger, its writer and its retention (`IP-12`).** `internal/ledger` is a bounded queue
  the request path hands each event to without waiting, and one writer per process that appends in
  batches through its own two-connection pool. A full queue drops and counts (`queue_full`); a lost
  connection holds the batch and retries with capped backoff; a refusal is bisected to its row and
  kept as a dead letter. The writer mints a UUIDv7 `event_id` at enqueue and inserts with `ON
  CONFLICT DO NOTHING`, so a redelivery changes nothing. `/v1/admin/status` reports the queue's depth,
  the writer's state and the drops; a shutdown writes what is queued within the grace.
- Migration `00010`: `usage_events.cycle_start`, a BRIN index on `created_at`, and
  `usage_event_dead_letters`. The retention job runs in every instance, prunes `usage_events` after
  `QUOTACORE_EVENT_RETENTION_DAYS` (90) and `audit_log` after `QUOTACORE_AUDIT_RETENTION_DAYS` (400)
  in batches of 5 000 under one advisory lock, every `QUOTACORE_RETENTION_INTERVAL` (1h), and replays
  rejected dead letters on each tick. `QUOTACORE_EVENT_QUEUE_SIZE`, `QUOTACORE_EVENT_BATCH_SIZE` and
  `QUOTACORE_EVENT_FLUSH_INTERVAL` size the queue.
- Eight ledger metrics: enqueued, written, dropped by reason, queue depth, write latency, duplicates
  detected, dead-lettered by reason, and dead letters outstanding.
- Every rollover is a row. The six scripts that share the transition answer a ninth value, the
  balance the rollover discarded, and whichever request rolled, a check or a refused refund included,
  hands the sink a `system` `cycle_rolled_over` before its own event.
- Integration tests for the eight DoD items against the rows the writer appended, and real-row
  assertions in T-01, T-02, T-08, the admin error matrix and the plan-change test.

- **Plan lifecycle and cycle-boundary semantics (`IP-10`).** `internal/admin` serves the eight
  operations that move a balance or a boundary: the plan entitlement edit, apply-now, impact, a
  tenant's plan change, grant, set, force-rollover, and setting and clearing an override. A plan edit
  takes effect at each tenant's own next boundary; a plan change and an apply-now re-anchor the
  tenant to a fresh cycle at the new limit and hand the discarded balance to the sink as a
  `plan_changed` event. An apply-now above `QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD` (50) tenants
  answers `409 confirmation_required` with a single-use token that lives for
  `QUOTACORE_CONFIRMATION_TTL` (15m) and is honoured only for the same plan, version and reason while
  the count has not grown. Impact answers the count and the earliest boundary and writes nothing.
- Migration `00009`: `tenant_entitlement_pins`, the cadence and feature set each tenant was anchored
  on, with a retired feature's limit and the end of the window it stays for; and `pending_reanchors`,
  one row per tenant whose hashes still have to be rewritten after a committed plan change or apply.
  The snapshot interns pin sets as it interns plans.
- Five control-plane scripts beside the data plane's three: `grant`, `set` and `relimit` open with
  consume's transition block byte for byte; `reanchor` overwrites a hash under a compare-and-set
  guard; `rollover` refills the computed window.
- The re-anchor drain: a request drains its own tenants inline, and every instance drains the rest
  in chunks of 100 on start and on each refresh tick, so a change committed while the fast store is
  away lands when it returns, once. `QUOTACORE_CONFIG_REFRESH_INTERVAL` is refused at an hour or
  more, the bound the drain's stale-anchor argument needs.
- Integration tests for the nine DoD items: the admin half of the endpoint × error matrix and of
  T-08 over all 32 admin routes, with the tenants' fast-store keys added to what a refusal must leave
  unchanged; `ip10Ops` and `ip10Only` are deleted, and no admin route answers a bare `404`.

- **The control-plane API (`IP-09`).** `internal/admin` serves 24 admin operations: keys (create,
  list, revoke), features (list, create, get, update, archive, delete), plans (list, create, get,
  archive, delete), tenants (list, create, get, update, suspend, resume, delete), the audit and
  usage-event reads, and status. Each mutation is audited in its own transaction by a Postgres store
  in `internal/db` with the caller's key, the request id, `before`, `after` and the reason, and each
  versioned write refuses a stale `version` as `409 resource_version_conflict`. A new tenant's
  opening balances are written before its row commits, and every change reaches the data plane
  through the configuration invalidation. Lists page with HMAC-signed cursors. A revoke evicts the
  key from the serving instance's cache at once. `snapshot.Stats` feeds status's cache figures.
- An acknowledged, well-formed `QUOTACORE_BOOTSTRAP_ADMIN_KEY` is inserted as `bootstrap-admin`
  only into an empty `api_keys` table, once across concurrent starts, with a `bootstrap` audit row; a
  malformed one refuses to start. No route can create it, and the name `bootstrap` is reserved.
- A per-client-address admin rate limit, `QUOTACORE_RATELIMIT_ADMIN_RPM` (60), checked before the
  credential, with `QUOTACORE_TRUSTED_PROXIES` naming the proxies whose `X-Forwarded-For` it believes.
- Integration tests for the nine DoD items and the obligations `IP-05`, `IP-07` and `IP-08`
  deferred: the admin half of the endpoint × error matrix and of T-08, every IP-09 route reachable
  with an admin key, the PII corpus at the boundary, and a CI step that starts the binary with a
  bootstrap key and reads status with it.

- **The admin surface's contract and specification amendments (`IP-09`).** Two routes,
  `DELETE /v1/admin/plans/{id}` and `DELETE /v1/admin/features/{feature_key}`, so P-6 and F-6 and
  the `plan_in_use` and `feature_in_use` codes have a route that reaches them. An optional `unit` on
  a feature `PATCH`, so `feature_unit_immutable` has one too, and an optional `kind` on a feature
  create, where `boolean` is `501`. A new code, `404 api_key_not_found`, on revoke, and the keys
  cell drops `resource_version_conflict`, since a key has no version (35 codes). Every admin route
  but status names `429 rate_limited` for the new per-client-address admin limit, and the `500` and
  `503`s that authentication in front of it can return. `AuditEntry.reason` and migration
  `00008_audit_reason.sql` keep the reason every archive, delete, suspend and resume already
  required. The admin list cursor's signing key is `qc:cfg:cursor_key`.
- **The data plane (`IP-07`).** `POST /v1/consume`, `POST /v1/check`, `GET /v1/balance` and
  `POST /v1/refund` serve the contract: decoding that names every bad field in one
  `validation_failed`, `Idempotency-Key` enforcement, the scripts behind them, `Idempotent-Replay` on
  a replay, and an event handed to a sink after the response (a no-op until `IP-12`).
- Bearer authentication on every `/v1` route, in ADR-0018's format, with a constant-time SHA-256
  verify, a bounded key cache refreshed by the configuration invalidation, a rate-limited miss path,
  and `last_used_at` written off the path at most once a minute. A runtime key is `403
  insufficient_scope` on every control-plane route.
- The error envelope, written by one function, with each status read from the contract and every
  catalogue code pre-registered on `quotacore_errors_total`; `X-Request-Id` on every response and in
  every log line of its request; and a per-key protective rate limit that answers `429 rate_limited`
  with `Retry-After`, distinct from `429 quota_exceeded`.
- `QUOTACORE_REQUEST_TIMEOUT`, `QUOTACORE_MAX_BODY_BYTES`, `QUOTACORE_METADATA_MAX_BYTES`,
  `QUOTACORE_KEY_CACHE_ENTRIES` and the rate-limit settings, as deployment.md lists them.
- Integration tests for the eight DoD items and the obligations `IP-04`, `IP-05` and `IP-06` deferred:
  the endpoint × error matrix, T-01, T-02 and T-08 over HTTP, T-09 and T-10 against the built
  binary, a dead datastore and a Postgres outage through a TCP proxy.

- **The control-plane schema (`IP-08`).** Six up-only migrations in `internal/db/migrations` create
  the eight tables of [data-model.md](docs/architecture/data-model.md) §2, and
  `TestTheSchemaIsTheDataModel` compares the migrated catalogue with the document's own `sql` blocks.
  Every earlier version upgrades to the same schema with its rows intact, and concurrent starts
  migrate once.
- A checksum record, `quotacore_migration_checksums`: the runner refuses to start when an applied
  migration's file has changed, and records the checksums of a database migrated before it existed.
- `db.Classify`, which turns each constraint the migrations create into its error-catalogue code
  before any route exists, and `TestEveryConstraintIsClassified`, which fails on a constraint with no
  entry. `DeletePlan`, `DeleteFeature` and `SetFeatureUnit` refuse as `plan_in_use`,
  `feature_in_use` and `feature_unit_immutable`; `ValidExternalID` is the boundary half of DR-034.
- The Postgres snapshot `Loader`, wired in `cmd/quotacore`: the tenant's version from the datastore,
  then its rows in one statement (DR-039), with a deleted tenant answered as unknown (INV-T1).

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
- `IP-06`, idempotency prevention in the data plane. `internal/script/idempotency.go` computes the
  fingerprint — SHA-256 over the operation, tenant, feature and amount, each length-prefixed with a
  big-endian `uint64` so a field's contents cannot shift the boundary between fields, hex-encoded
  and truncated to 32 characters — and pins `IdempotencyWindow = 24 * time.Hour`. The runner sends a
  second key (the record) and a seventh argument (the fingerprint) for `consume` and `refund` only,
  and refuses a mutation with no key as `ErrMissingIdempotencyKey` before it reaches the store;
  `check` is unchanged, because it mutates nothing, so it has nothing to replay and nothing to
  record (DR-026, NFR-L5). A block, byte-identical in `consume.lua` and `refund.lua`, looks the
  record up before the transition, replays the seven recorded values on a matching fingerprint
  without writing, refuses a mismatched fingerprint as `reused`, and records an applied mutation
  with `SET … EX 86400`. Two new `Decision` values, `replayed` and `reused`, carry the two outcomes;
  `Allowed()` is true for `replayed`. **Proven against a real valkey:** 50 concurrent deliveries of
  one key deduct once and replay 49 times, each answering the recorded state rather than the live
  balance; a key reused for a different amount, feature or operation is refused and the original
  record still replays (T-03, INV-I2); the record's TTL is the pinned 24 hours and a replay does
  not extend it (DR-029); and a property test applies any amount at most once across two runner
  instances (DR-030). The window is now linked across all three places it is pinned —
  `script.IdempotencyWindow`, the Lua `local DAY = 86400`, and `cmd/quotacore`'s refusal of a
  different `QUOTACORE_IDEMPOTENCY_TTL` — by a test on each boundary.

### Changed

- **Register (`IP-16`).** The seven rows due before v0.1 and still open are answered, each with its
  default: `Q-08` (measured), `Q-12`, `Q-15`, `Q-25`, `Q-26`, `Q-27` and `Q-28`. `Q-30` is new and
  asks where NFR-L3 is measured; it does not block v0.1. Six questions remain open, none due before
  v0.1.
- **testing-strategy §5 and §6, and IP-16's DoD 1 and DoD 6, amended (`IP-16`).**
  - The latency rows are gated on the service's own time, with the client's figure recorded.
  - The soak is 5 h 30 m, with slope assertions, because a hosted runner's job ends at 6 h.
  - Each chaos row now has a stated place to run.
  - DoD 1 counts all fifteen named tests.
- **`IP-16` is `IN PROGRESS`**, opened 2026-10-28, the day `IP-15`'s code merged and before any file
  it owns exists, by a dated amendment to its dependency on `IP-15`: the runbook and restore review
  that `IP-13` and `IP-15` wait on is carried as the release's one named gap, and v0.1 is declared
  done only once it is recorded. Its "Blocks" line now matches the index (`IP-19` and `IP-27` added).
- **The Compose file's role** (`IP-15`): from two development stores to the reference
  deployment. The integration suite in CI now publishes the stores with `.github/compose.ci.yml`,
  runs as the superuser it needs to create scratch databases, and has a scratch store of its own;
  the `service` job runs the built image instead of the binary in a `postgres:16` container, and
  the former `compose` job is part of `reference`.
- **The data-store memory floor** (`IP-15`, deployment §5): 106 GiB, measured, replacing the
  projected 24 GiB. A record costs 522.9 bytes at the contract's 255-character key and 266.9 at a
  UUID's, so a deployment whose clients send UUIDs may size to 54 GiB; the old projection counted a
  short key and value and no table. A ledger row costs Postgres 475 bytes with its indexes.
- **The drain** (`IP-15`): a shutdown that overruns `QUOTACORE_SHUTDOWN_GRACE` closes the listener
  and still gives the ledger five seconds to flush what the answered requests wrote, rather than
  none.
- **The pools' connection parameters** (`IP-15`, deployment §7): `search_path=public` and a
  `statement_timeout` of `QUOTACORE_CONTROLPLANE_TIMEOUT` on every service pool; the local
  operations' pool has no timeout, because a rebuild reads the whole ledger.
- deployment.md §1 to §3, §6, §7, §11 and §12 item 2, consistency-and-recovery §4.1 (nightly, not
  weekly), observability §4 and `IP-15`'s DoD items 5 to 9, by dated amendments.
- **`IP-15` is `IN PROGRESS`**, opened 2026-10-27 before any file it owns exists, by a dated
  amendment to its dependency on `IP-13`: the runbook review that `IP-13` waits on is performed on the
  reference stack, in the same session as `IP-15`'s restore, and both phases close on that row.
- **`IP-14` is `COMPLETE`**, opened and closed 2026-10-27 with 7 of 7 DoD items met. Item 1 names
  `init` as the one composite command, item 3 is read as corrected with `grant` or `set`, and item
  7's image half is `IP-15`'s, by dated notes; the scope's `quotacore worker` waits on `Q-13`.
  `IP-15` now waits on `IP-13` alone.
- One dispatch for the binary: no argument or `serve` runs the service, and `IP-11`'s four local
  operations are rows of the same table. Their exit codes follow cli.md §3: `2` is now usage, and
  an error is `5`, `6`, `9` or `10` by its cause, where it was always `2`.
- The runbooks call the CLI. Every `curl` to an admin route is its command; the metrics scrape,
  `/readyz` and `/v1/balance` keep theirs. `TestEveryRunbookIsFollowable` parses each `quotacore`
  line against the binary and fails on a `curl` to `/v1/admin/`.
- deployment.md §2: `init --yes`, and the `try` line names a variable rather than printing the key
  twice; §4.1: the bootstrap key is minted with `quotacore key mint`.
- **`IP-13` is `IN PROGRESS`**, opened 2026-10-21 before any file it owns exists. Nothing gated it:
  `IP-07` closed on 2026-10-19. By the end of the day 7 of its 8 DoD items were met; item 7 waits on
  a person's review of the runbooks, so `IP-15` stays `BLOCKED`.
- The probes answer the contract: `/healthz` is `200 {"status":"ok"}` until exit, and `/readyz`
  `200 {"status":"ready"}` or a bare `503`, with observability.md §4's terms and no tenant term.
  The contract's `/readyz` gains its `503`, and `/metrics`'s description and example name its
  listener and real series (`make generate`, `make docs`).
- Logs: `ts` in UTC with microseconds and a lowercase `level` from one constructor; JSON from the
  first line, before the configuration is read; a `job` on every line no request caused; `actor`,
  `action` and `subject_type` on admin lines. §2's `reconcile_key` example field is removed.
- The listen and metrics ports refuse to start below 1024, and a process running as uid 0 warns.
- Dated `IP-13` notes on observability.md §1 to §6, NFR-S10, deployment.md §4.1, SECURITY.md §6
  (gap 9, the unauthenticated loopback metrics port), A-17, and `IP-13`, `IP-14` and `IP-15`'s
  items. The specification now holds 76 documents.

### Changed

- **`IP-11` is `COMPLETE`**, opened and closed 2026-10-20 with 6 of 6 DoD items met. `IP-16` now
  waits on `IP-15` only. Items 3, 5 and 6 carry a dated note dividing them with `IP-15`'s restore
  drill, and `IP-14` gains the four local operations by a dated note. Dated `IP-11` notes on DR-045
  and DR-049, consistency-and-recovery §4.1, §4.5, §5 and §6 with a new §4.6 (the dispute runbook),
  error-catalog's `service_unavailable` and FS-22, state-machines C-1a, INV-I4 and I-6,
  data-model §2.7 and §2.12 to §2.15, deployment's `QUOTACORE_STARTUP_MODE` and
  `QUOTACORE_RECONCILE_INTERVAL`, request-lifecycle step 2, observability §1.5 and §5,
  testing-strategy T-11 and T-14, UC-16 step 4 and security-model §2. The contract gains
  `Status.mode`, and the events route's key filter also matches a reversal's `reconciles_key`.

### Changed

- **`IP-11` is `IN PROGRESS`**, opened 2026-10-20 before any file it owns exists. Nothing gated it:
  `IP-07` closed on 2026-10-19 and `IP-12` on 2026-10-20.

- **`IP-12` is `COMPLETE`**, opened 2026-10-20 before any file it owns existed and closed the same day
  with 8 of 8 DoD items met. `IP-11` and `IP-14` are now `NOT STARTED`. A force-rollover reopens the
  cycle it is in, so data-model §2.7's reconciliation identity reads from a cycle's last opening.
  `force_rolled_over` carries its reason in `metadata`, and the events route answers a null window
  for a row stored against a start the tenant's anchor does not compute. `Q-24` is added with a
  default: an immediate override's balance move is written as `plan_changed`.

- The specification and contract amendments the ledger needs (`IP-12`). `usage_events` gains
  `cycle_start` and a BRIN index on `created_at`, and a cycle is `(tenant, feature, cycle_start,
  cycle_index)`, opened by its `cycle_rolled_over`, `plan_changed` or `force_rolled_over` row or, with
  none, at the effective limit; a new `usage_event_dead_letters` table keeps what the ledger refuses.
  The writer mints `event_id` and writes `created_at` from the instant the request accepted.
  request-lifecycle step 9 distinguishes a full queue, a transient write error (held and retried) and
  a refusal (dead-lettered). A check that rolls a window emits the rollover (IP-07 item 2). DR-049,
  T-14 and consistency-and-recovery §4.1 read the detected duplicate as a second deduction under one
  idempotency key, not a repeated `event_id`; §5's `verify-tenant` formula no longer counts a grant
  twice. `Status.queue_depth` and `worker` describe the ledger queue and writer, and
  `listUsageEvents` no longer promises `system` gap events that §4.5 forbids. IP-12's DoD 3, 4, 7
  and 8 are read for the writer by dated notes, since they cite webhook and reset-worker
  invariants. `Q-09`, `Q-20` and `Q-22` are answered with their defaults. There is no replica in
  v0.1 (ROADMAP, ARCHITECTURE).

- **`IP-10` is `COMPLETE`**, opened 2026-10-20 after `IP-09` closed, before any file it owns
  existed, and closed the same day with 9 of 9 DoD items met. Item 4 is read by DR-015's body, and
  the ledger rows, the reconciler's view of a re-anchor, a pending-re-anchor gauge and the CLI
  commands are handed to `IP-11` to `IP-14` by dated amendments. `IP-16` still waits on `IP-11` and
  `IP-15`.

- **Plan lifecycle and cycle-boundary semantics, specified (`IP-10`).** Each tenant is pinned to the
  cadence and feature set it was anchored on, so an interval edit or an added feature reaches an
  existing tenant at its next re-anchor and a removed feature stays until the end of the window it
  was removed in (E-3, E-4, DR-014's note, data-model §2.10). A plan change or an apply-now commits a
  pending re-anchor that a drain finishes in the fast store, which replaces consistency-and-recovery's
  "self-correcting" (§2.11, §1, §3, §6). A grant raises the bonus with the balance, so
  `grant_exceeds_ceiling` is a `set` above `limit + bonus` or a grant past `2^53 − 1`; T-04's grant
  line, UC-14's `51` line, the contract example and the catalogue's remedy are corrected. A `set`
  takes `0` to `limit + bonus`, and DR-018's negative balance is `Q-23` (23 open questions).
  Force-rollover refills the window the anchor puts the current instant in (C-5, UC-12, INV-C1,
  INV-X4). An override's `immediate` re-limits the open cycle and keeps what was spent (cycle-engine
  §7, data-model §2.5, UC-13). The confirmation token is also bound to the reason and to the count it
  was issued against, its `409` writes no audit row (P-3b), and impact previews the plan as saved
  (UC-19, api-conventions §10.1 and §10.2). T-5 allows a suspended tenant. The tenant balance routes
  and the override clear gain `409 tenant_deleted`, and the clear gains `404 feature_not_found`.
  Impact takes no query parameter and, reading only Postgres, answers `control_plane_unavailable`
  rather than `service_unavailable`. `QUOTACORE_CONFIG_REFRESH_INTERVAL` is refused at or above one
  hour.

- The bootstrap admin key is one the operator mints and supplies, never one the service generates
  or prints. api-conventions §5, security-model §3, UC-15, J-1 and SECURITY.md said it was "printed
  once", which ADR-0018's environment-variable model never did; deployment.md §4.1 gains a recipe
  for minting one. Revocation in the contract and UC-15 now says "within NFR-D3 on every other
  instance" rather than "no cache delay", as `IP-07` already corrected elsewhere. P-5 has no v0.1
  route, and P-6 and F-6 audit a successful delete. The descriptions of `Feature.id`, the feature
  list's pagination, `ArchiveRequest`, the two identifier paths and the usage event's cycle fields
  are corrected (`IP-09`).
- **`IP-09` is `COMPLETE`**, closed 2026-10-20 with nine of nine DoD items met, and `IP-10` is now
  `NOT STARTED`; `IP-14` waits on `IP-12` alone. `IP-05`'s `adminOnly` list is gone: every admin code
  is provoked over HTTP, and the three only `IP-10`'s routes reach are listed as its.
- **`IP-09` is `IN PROGRESS`**, opened 2026-10-19 after `IP-07` closed. Its scope and `IP-10`'s
  overlapped on most of the admin surface; both are amended so `IP-09` builds the resource routes and
  `IP-10` everything that moves a balance or a boundary.

- **ADR-0018 supersedes ADR-0012** (2026-10-19). Keys keep everything ADR-0012 decided, with one token
  format, the contract's `qc_<runtime|admin>_<hex prefix>.<base64url secret>`, where three had been
  written, and the SHA-256 verifier ADR-0012 chose, where eight documents and three contract
  descriptions had drifted to Argon2id. Each of those carries a dated correction.
- **The contract, for `IP-07`.** `IdempotencyKey` takes api-conventions' grammar,
  `[A-Za-z0-9._:-]{8,255}`, instead of a ULID only, so a customer can derive the key from their own
  operation identifier. `GET /v1/balance` requires `tenant_external_id`. `BalanceEntry.consumed` is
  `limit + bonus - balance`, not a ledger read. A replay is described as identical in its state values
  and different in `replayed`, as `IP-06` already corrected elsewhere. The `ValidationFailed` and
  `PayloadTooLarge` examples agree that an oversized `metadata` is `413`.
- **Documents corrected for `IP-07`.** The error envelope has no `documentation_url` and no
  `retry_after_ms: null` (error-catalog §1). `service_unavailable` promises no *partial* change, with a
  same-key retry as the safe response. DR-026 names `missing_idempotency_key`. Revocation is immediate
  on the serving instance and within NFR-D3 elsewhere (api-conventions §5, mvp-scope, K-2). The
  balance route reports the stored cycle as one consistent set, and omits an archived feature whose
  hash has expired (request-lifecycle §2). `rejected` means a `5xx` (observability §1.1).
  `QUOTACORE_AMOUNT_MAX` and `QUOTACORE_MAX_FEATURE_KEY_LEN` are fixed by the contract.
- **`IP-07` is `COMPLETE`**, started and closed 2026-10-19, with Definition-of-Done items 2 and 8
  amended so the ledger half of a check is `IP-12`'s and the admin handlers and revoke route are
  `IP-09`'s. `IP-13` is now `NOT STARTED`. CI's smoke test now expects the data plane to ask for a
  credential in the envelope, and `GET /v1/consume` to be `405`.
- **`IP-07` was opened `IN PROGRESS`** on 2026-10-19 after `IP-08` closed. The IP-07 scope line said
  `POST /v1/balance`; it is `GET`, as the contract and every other document say.

- **`IP-08` is `COMPLETE`**, started and closed 2026-10-19, with Definition-of-Done items 4, 5, 6 and
  7 amended so the `409` statuses and the boundary validation call are `IP-09`'s and the retention
  deletion is `IP-12`'s. `IP-09` and `IP-12` are now `NOT STARTED`.

- **`IP-08` is `IN PROGRESS`, opened 2026-10-19**, before any file it owns was written. `IP-07` and
  `IP-08` read `BLOCKED` in the phase map although every dependency of both was `COMPLETE`; `IP-07`
  is now `NOT STARTED`. The phase map in [v0-1-enforcement-path.md](docs/roadmaps/v0-1-enforcement-path.md)
  had also kept `IP-06` at `BLOCKED` and `IP-03` and `IP-05` undated, and is brought in line with
  [roadmap-index.md](docs/roadmaps/roadmap-index.md).
- **The specification amendments `IP-08` needed before it could build the schema, 2026-10-19.**
  [data-model.md](docs/architecture/data-model.md) now states `tenants_plan_idx`, which its own prose
  and the impact route already required; a `seq` column on `usage_events`, which §2.2 already cited
  and the within-batch order of `balance_after` needs; a `CHECK` on `audit_log.subject_type` with the
  four values the API documents; and that no migration ships a down section and every applied
  migration's checksum is recorded. [api-conventions.md](docs/architecture/api-conventions.md) §2
  said the internal identifiers were UUIDv7; they are v4, which is what the data model generates
  and PostgreSQL 16 can produce. The `UsageEvent.request_id` description in `api/openapi.yaml` said
  a `system` event carries `null`; the column is `NOT NULL`, so a job mints the id, and only the
  description changed (`make generate`, `make docs`). `Q-22` asks how a zero-delta transition meets
  `delta <> 0`, with a default, and `Q-09`'s deadline moves to `IP-12`, because retention changes
  no table. `SECURITY.md` §6 gains gap 8, `tenants.display_name`.
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
- **The statements about idempotency that the implementation contradicted were corrected by
  `IP-06`, across every document that carried them.** `T-02` in
  [testing-strategy.md](docs/architecture/testing-strategy.md) said all 50 responses to one
  `Idempotency-Key` are byte-identical; they are not, and must not be, because the first says
  `applied` and the retries say `replayed` and carry `Idempotent-Replay: true` (DR-027,
  [api-conventions.md](docs/architecture/api-conventions.md) §6 and §7.1). What is identical is the
  seven state values behind the decision, and that is now what the test asserts. The same rationale
  in §7.1 of that document ("the replayed response is byte-identical to the original") was corrected
  for the same reason. [data-model.md](docs/architecture/data-model.md) §3.3 described the record as
  a JSON body with an HTTP status and a write of `SET … EX 86400 NX`; it is a tab-separated string of
  the fingerprint, the transition verdict and the six balance-hash fields, written `SET … EX 86400`
  with no `NX` and only when a mutation is applied, so a denial is never recorded (ADR-0004, ADR-0002).
  Removing the `NX` also took it out of the required-command list in
  [deployment.md](docs/architecture/deployment.md) §6 and out of the record's row in the
  [Redis/Valkey licensing note](docs/research/licensing-redis-vs-valkey.md), each with a dated note.
  The same edit removed a sentence in §3.2 that said a hash could be "lost to memory pressure", which
  the `noeviction` policy makes impossible (DR-048). The
  [idempotency research note](docs/research/idempotency-and-retries.md) — the note ADR-0004 rests on —
  carries a dated 2026-09-30 update recording that the replay is state-identical and not
  byte-identical, and that the record write no longer uses `NX`; its findings are left as the
  point-in-time records they are.

### Fixed

- **A cycle boundary was rendered up to a second early** (`IP-16`, found by the formatting property).
  An anchor keeps milliseconds and the wire carries seconds, so a `cycle_end` was rounded down to a
  second at which the old cycle was still open, and a client that waited until it was refused again.
  Every `cycle_start` and `cycle_end` is now rounded up (`envelope.Boundary`, api-conventions §3).
  `TestUpgradeAndRollback` compares a balance read from builds either side of the change with a
  boundary allowed to differ by that second; every figure must still be equal.
- **`TestSustainedThroughput` read noise as creep** (`IP-16`, first full-suite run). It fitted a
  least-squares slope to the client's per-minute p99, and two noisy late minutes on the shared runner
  (6.8 and 6.4 ms against a typical 1.3 ms) projected to 1.4 ms of creep where the median p99 of the
  first and last thirds differed by 0.4 ms. "p99 stable" is now gated on the service's own per-minute
  p99 from its access log, as Q-30 gates every latency figure, as the median of the last third of the
  run's minutes less the median of the first, within 1 ms; both creeps are recorded.
- **A deleted tenant was still served by the instance that deleted it** (`IP-16`, found by `T-15`,
  DR-047). A tenant in the snapshot stayed there until the invalidation came back over pub/sub, so a
  consume sent after the delete's `200`, to the same instance, could complete. The admin routes now
  drop each tenant they wrote from that instance's snapshot before publishing, so the next request
  reads it as committed; other instances still learn of it within NFR-D3.
- **A full store did not refuse writes** (`IP-15`, T-13). A script without a shebang is checked
  against `maxmemory` only at its first write that may grow memory; the consume script's first
  write is a `PERSIST`, which is not one, so every later `SET` and `HINCRBY` went through and a
  store with a 12 MB limit grew past 70 MB. Every script now declares `#!lua`, under which the store
  refuses the whole script while it is over its limit, and a test holds every script to it.

- **A runbook command the binary refused** (`IP-14`): *Data store lost* ran
  `quotacore rebuild --apply` without the `--reason` a writing rebuild requires. The runbook test
  checked only a command's first word, and missed two multi-line `curl`s entirely.
- **Nothing was served at `/metrics`** (`IP-13` finding 1): the collectors were registered on a
  registry no listener served.
- **A fresh install was never ready** (`IP-13` finding 2): readiness required a known tenant, so an
  install answered `503` until its first tenant was created.

- **A Postgres outage stopped enforcement.** `IP-11`'s fence put an instance in read-only mode
  whenever its fence connection failed, so an outage longer than one fence check refused every
  tenant write with `503`, against NFR-A2 and mvp-scope §3. An instance now goes read-only only
  when Postgres answers and a rebuild holds the fence; a check that cannot reach Postgres keeps
  the mode it had. consistency-and-recovery §4.1 step 2 carries a dated note.
  `TestAPostgresOutageKeepsTheFenceMode` reproduces it, and found it before the fix.

- **`TestTheLedgerReconcilesEveryCycle` was flaky.** Its seeded checks could meet the bounded-miss
  limiter and be answered `503`; they now retry a `503` for up to five seconds.
- **Two integration tests failed when every package ran at once.** `go test ./...` runs packages
  in parallel against one store. `TestConfigNamespaceAndInvalidationRoundTrip` read a config version
  that `cmd/quotacore`'s stacks bump; it now keeps its keys in a database of its own and waits for
  its own invalidation on the server-wide channel. `TestTheScriptsWorkIsNotProportionalToTheTenant`
  took the median of 11 timings, which other packages' load could inflate; it now takes the fastest
  of 21.

- **An idle tenant was locked out a day after its window closed.** Every balance hash was written
  with `EXPIREAT window_end + 24h`, only provisioning and a re-anchor may create one (DR-045), and no
  v0.1 phase runs the worker that would have kept an idle tenant's hash alive, so an hourly tenant
  idle for 25 hours was answered `503 service_unavailable` until a rebuild. A balance hash now has no
  expiry: the six transition scripts, `rollover.lua`, `reanchor.lua` and provisioning `PERSIST` it, which
  also clears the expiry an earlier binary left at the hash's next write. INV-C3, INV-X6, data-model
  §3.1 and §5, cycle-engine §5, request-lifecycle step 3, testing-strategy §4, performance,
  deployment's command list and `IP-05` item 7 carry dated notes. `TestABalanceHashNeverExpires`
  and `TestAnIdleTenantIsStillServedAfterADay` reproduce it. A deleted tenant's hashes now stay
  until removed: deleting them on soft deletion would answer an in-flight request `503`, which T-15
  does not permit.

- **A consume of zero answered `503`** (`IP-05`). consume.lua passed the negated amount to
  `HINCRBY`, and Lua renders the negation of zero as `-0`, which Redis refuses as not an integer, so
  DR-023's valid zero-amount request failed as a store fault. The increment is formatted as an
  integer.

- **An immediate override moved a balance with no ledger event** (`IP-10`). The relimit applied the
  change in limit to the balance and discarded the script's answer. The move is now handed to the
  ledger as `plan_changed`, and a rollover its transition applied as `cycle_rolled_over`.

- **`TestArchivingAFeaturePreservesItsBalance` failed about one run in fifteen** (`IP-09`). It
  waited for the archive to reach the data plane by consuming, and a consume that landed before the
  invalidation changed the very hash the test compares. It waits with a check, which writes nothing.

- **The contract's `timezone` pattern refused `UTC`** (`IP-02`), its own default, because it
  required an `Area/Location` name. No route took a time zone until `IP-09`'s tenant create. The
  pattern admits `UTC`, and the generated types and reference page were regenerated.

- **A silent datastore held a request for five seconds** (`IP-04`). go-redis ignores a context's
  deadline unless `ContextTimeoutEnabled` is set, so `QUOTACORE_DATASCRIPT_TIMEOUT` did not bound a
  store that accepted and never answered. It is set now.
- **The snapshot compared its configuration version outside its lock** (`IP-04`), a data race between
  the subscriber and the ticker, and an invalidation could lower a version a reconcile had raised.
- **Reloading a cached tenant on the same plan leaked a plan reference** (`IP-04`), so the plan was
  never retired or its bytes refunded once the tenant was evicted.
- **Concurrent first sights of one tenant each spent a miss token** (`IP-04`), so a burst of cold
  traffic was refused by the limiter that bounds Postgres. Concurrent misses for one identifier now
  share one read.

- `make generate` regenerated nothing: it ran `ogen-check`, which only compares. A new `ogen` target
  writes the generated files, and `generate` runs it.

- **An archived feature on a suspended tenant resolved `suspended`** (`IP-04`). The snapshot checked
  the tenant's state before the feature's archive state, and its archived set was never filled by the
  `Loader` contract. `Entitlement` now carries `Archived`, and archived resolves first.
- **The migration runner kept no checksums and released its lock through the pool** (`IP-01`). goose
  records versions only, so an edited migration went unnoticed, and the advisory unlock could run on
  a different connection than the lock. The runner now records checksums, pins one connection, and
  uses a goose `Provider` instead of goose's global state.

- The snapshot subscriber missed a configuration change announced before its subscription was
  confirmed, so the change waited a full refresh interval rather than NFR-D3's second, and a cache
  hit wrote to the recency list under a shared lock, a data race once two requests hit at once. The
  subscriber now reconciles each time it subscribes and a hit takes the write lock; both are proved
  by tests that fail without the fix, and recorded in the `IP-04` section of
  [v0-1-enforcement-path.md](docs/roadmaps/v0-1-enforcement-path.md).
- Two evidence rows named `TestTheScriptsCallOnlyTheCommandsTheyNeed`; the test is
  `TestScriptsCallOnlyTheCommandsTheyNeed`.
- `tools/check-docs.ps1` counted "Files in the repository" without `-Force`, so on Linux and macOS
  it skipped the five dotfiles that Windows counts, and CI's Ubuntu runner measured 142 against
  the inventory's correct 147. The count now includes hidden files and matches `.git` with either
  path separator, so it is the same number on every platform.
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

