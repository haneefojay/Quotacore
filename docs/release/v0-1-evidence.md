# v0.1 evidence

Every claim v0.1 makes, the measurement or test behind it, and where that was taken, in one place
(`IP-16`'s exit criterion). A reader who doubts the product starts here. Each row names the test that
proves it, so the claim can be re-run rather than believed.

The run of record is the `IP-16` pull request at commit `01ddb7d`: the
[full suite](https://github.com/haneefojay/Quotacore/actions/runs/40801904335) (load, soak, chaos,
clock and the deep suites) and [CI](https://github.com/haneefojay/Quotacore/actions/runs/40801903578)
(build, unit, race, integration, reference, SBOM). Both were green. The commits after it on that
pull request change documents and tests only, and run CI again.

**Environment.** A GitHub-hosted `ubuntu-latest` runner, 4 vCPU and 16 GiB, runner image
`ubuntu-24.04` 20261025.353.1. The service, Postgres 16 and Valkey are pinned to CPUs 0 and 1 with
4 GiB between them by `test/harness/compose.perf.yml`, which is the reference host of NFR §0 with its
stores on the same host. The open-loop generator runs on CPUs 2 and 3. The `evidence-load` and
`evidence-soak` artifacts of the full-suite run hold `lscpu`, the kernel, the image digests and every
figure below as JSON.

**Which latency.** Latency targets are gated on the service's own time for each request, read from
its access log, with the client's figure beside it ([Q-30](../product/assumptions-and-open-questions.md#2-open-questions)).
All latencies are in microseconds (µs) unless they say otherwise.

---

## 1. The three claims

**1. P99 under 5 ms at 2,000 requests per second, with no database connection on the request
path.** Met.

- `TestSustainedThroughput` sent 2,000 consumes a second for 30 minutes: 3.6 million requests,
  every one answered `200`. The p99 was 1,958 µs at the client. The p99 drift between the first
  and last thirds of the run was −33 µs at the service and +57 µs at the client, inside the 1 ms
  flatness bound. Memory showed no growth.
- `TestWarmConsumeLatency` measured the steady path at 100 a second for 10 minutes: p99 389 µs at
  the service and 753 µs at the client.
- No database on the request path, proved two ways (IP-16 D16):
  - Over the 30-minute run, Postgres sessions went from 5 to 7, which is the ledger writer and the
    snapshot refresher, and the `pgx` pool wait stayed at zero across 3.6 million requests.
  - `TestPostgresOutageUnderLoad` stopped Postgres under 2,000 a second. All 240,000 requests
    succeeded, and the p99 was 0.72 ms against a 1.22 ms baseline.

**2. A retrying client is charged once.** Met.

- `T-02` sends 50 deliveries of one key and gets one deduction and one recorded answer. `T-03`
  refuses a reused key with a different operation. Both run under `-race` on every pull request.
- The window is 24 hours, and the store is configured so it never evicts (`TestTheWindowIsTwentyFourHoursAndTheScriptsSaySo`,
  `TestTheServiceRefusesAnEvictingStore`).
- The one hole is total store loss. `T-14` loses the store inside the window: the duplicate is
  detected and reversed once (DR-049, `TestADuplicateDeductionIsReversedOnce`).
- The soak ran for 5 h 30 m at 500 a second, about 9.9 million requests. Afterwards every success
  had exactly one ledger row, and every balance equalled its grant less its consumption.

**3. One `docker compose up` and it works.** Met.

- The README's quickstart runs verbatim on a clean runner on every pull request
  (`tools/quickstart.sh`, `TestTheQuickstartIsTheDeploymentGuide`).
- The image runs unprivileged (`TestTheImageRunsUnprivileged`). The service opens no outbound
  connection (`TestTheServiceMakesNoOutboundConnection`). Startup to ready took 0.88 s
  (`TestStartupToReady`).

## 2. Non-functional requirements, measured

Every [NFR](../architecture/non-functional-requirements.md) whose "measured by" names a test or a
measurement. A row that is a validation rule cites the test that enforces it. The rows not listed
are statements of design (NFR-S, NFR-OPS, NFR-C), and the static and reference suites hold them.
NFR-L6 has no measurement; see §5.

| NFR | Target | Measured | Test |
| --- | --- | --- | --- |
| NFR-L1 | consume p50 ≤ 1.0 ms | 153 µs at the service, 283 µs at the client | `TestWarmConsumeLatency` |
| NFR-L2 | consume p99 ≤ 5.0 ms | 389 µs at the service, 753 µs at the client | `TestWarmConsumeLatency` |
| NFR-L3 | consume p99.9 ≤ 20 ms | 1,101 µs at the service, 1,790 µs at the client | `TestWarmConsumeLatency` |
| NFR-L4 | ≤ 250 ms with a dead store | A blackholed store returned `503` inside the bound, measured at the service. Delays of 400 ms and 2 s also failed inside it, and nothing queued behind them | `TestADeadDataStoreIs503WithinTheBound`, `TestChaosStoreLatency` |
| NFR-L5 | check p99 ≤ 3.0 ms | 341 µs at the service, 707 µs at the client | `TestCheckLatency` |
| NFR-L7 | rollover adds ≤ 1.0 ms | About −8 µs, which is noise, from the difference of two medians against the real store | `TestNFRL7RolloverOverhead` |
| NFR-L8 | start to ready ≤ 5 s | 0.88 s | `TestStartupToReady` |
| NFR-T1 | 2,000/s for 30 min, p99 stable, no memory growth | 3.6 M requests, all `200`. p50 255 µs, p99 1,958 µs, p99.9 6,523 µs at the client. Per-minute p99 drift −33 µs (service). Memory slope negative | `TestSustainedThroughput` |
| NFR-T2 | 10,000/s for 60 s, no failure | 600,000 requests, all `200`. p50 2.3 ms, p99 302 ms at the client, which is queueing | `TestBurst` |
| NFR-T3 | 200 racing for 100, exactly 100 | Exactly 100 | `TestT01AtomicDecrementUnderContention`, `TestT01TwoHundredConsumesOverHTTP` |
| NFR-T4 | 50,000 tenants | 50,000 cached (689 evictions at the cap). p99 1.79 ms against a 1.16 ms small-dataset baseline. Service memory 74.6 MB | `TestTenantScale` |
| NFR-T5 | 100 features a plan | 100 accepted; 101 is `400 validation_failed` and writes nothing | `TestAPlanHoldsAtMostAHundredFeatures`, `TestTenantScale` |
| NFR-T6 | `metadata` 4 KiB | Above it, `413 payload_too_large` | `TestBodyAndMetadataCaps`, `TestEndpointErrorMatrix` |
| NFR-T7 | snapshot ≤ 64 MiB | Inside the cap at 100 features a plan | `TestSnapshotMemoryWithinNFRT7` |
| NFR-T8 | ≥ 2,000 appends/s | 420,000 events written at 2,000/s, none dropped | `TestLedgerThroughput` |
| NFR-T9 | the full window at NFR-T1, `noeviction` | 249.9 B a record over 3.66 M records, within 10% of `IP-15`'s figure, nothing refused. The soak measured 238.1 B a record | `TestSustainedThroughput`, `TestDeploymentSizingCitesTheMeasurement` |
| NFR-T10 | reversal within one reconciliation cycle | Reversed once, inside the cycle | `TestADuplicateDeductionIsReversedOnce`, `TestT14AStoreLossInsideTheWindowIsReversedOnce` |
| NFR-A2 | 100% for cached tenants in a Postgres outage | 240,000 of 240,000 under load. `controlplane_up` was 0 and the pool wait stayed at 0 | `TestPostgresOutageUnderLoad`, `TestPostgresOutageSparesCachedTenants`, `TestAPostgresOutageKeepsTheFenceMode` |
| NFR-A3 | 0% in a store outage | Every request returned `503`, with no partial deduction | `TestADeadDataStoreIs503WithinTheBound`, `TestT09KillDuringConsume` |
| NFR-A4 | ≤ 1 s to recover | The first retry after the store returned succeeded, inside the test's 0.5 s retry window | `TestADeadDataStoreIs503WithinTheBound`, `TestChaosStoreLatency` |
| NFR-A5 | 30 s drain | A stop under load drained inside the grace period, with nothing partial | `TestAStopDrainsWithinTheGrace` |
| NFR-A6, NFR-D6 | restart loses nothing | A kill and a restart change no balance, cycle or key | `TestT10RestartChangesNothing`, `TestAKillLosesNothing` |
| NFR-D1 | strong per tenant and feature | Exactly one outcome under contention | `T-01`, `T-09` |
| NFR-D2 | 24 h window | 24 h, and the scripts say so | `TestTheWindowIsTwentyFourHoursAndTheScriptsSaySo` |
| NFR-D3 | propagation ≤ 1 s p99 ([Q-08](../product/assumptions-and-open-questions.md#2-open-questions)) | Over 200 suspends and resumes between two instances: p50 41 ms, p99 51 ms, max 70 ms. CI asserts the 1 s p99 on every pull request; this distribution was taken on the development container. With pub/sub muted, propagation widened to the refresh interval and stayed correct | `TestQ08ConfigurationPropagation`, `TestChaosPubSubUnavailable`, `TestRevokeReachesASecondInstanceWithinTheBound` |
| NFR-D4 | audit in the mutation's transaction | Every mutation is audited exactly once | `TestEveryMutationIsAuditedOnce` |
| NFR-D5 | best-effort ledger, visible drops | With Postgres paused and a queue of 1,000, 239,000 events were dropped and counted. p99 was 0.58 ms against a 1.52 ms baseline | `TestLedgerSaturation` |
| NFR-D7 | rebuild from configuration and ledger | The drill restores, rebuilds and reconciles | `TestTheRestoreDrill`, `TestRetentionBoundsWhatIsExact` |
| NFR-D8 | NTP required; a jump is safe | +24 h advanced once. −24 h was `STALE` with nothing moved, and `rollover_stale_total` rose | `TestChaosClockJump` |
| Soak (testing-strategy §5) | no leak or drift | 5 h 30 m at 500/s, about 9.9 M requests, no error. Over the second half, memory grew 1.5 MB on a 25.1 MB mean; goroutines (−11.5 on 38) and descriptors (−11.2 on 48.5) fell. The worst clock skew was 0.99 ms | `TestSoak` |

The chaos rows of [testing-strategy.md](../architecture/testing-strategy.md#6-chaos-tests) §6 ran
where its 2026-10-28 amendment places them, and all were green in the run of record. The two not
named above are disk full (`TestChaosPostgresDiskFull`) and one balance hash deleted
(`TestChaosOneBalanceHashDeleted`).

## 3. The named correctness tests

Every row of [testing-strategy.md](../architecture/testing-strategy.md#2-correctness-tests-the-ones-that-matter)
§2, with the test functions that prove it and the suite they run in. This table is the catalogue
`tools/testreport` reads: CI fails when a named test did not run, was skipped or failed, so a row
cannot lose its test without CI noticing. The `integration` suite is `go test -race` against real
Postgres and a real data store on every pull request. The `reference` suite runs against the
Compose stack and the built image on every pull request.

| Row | What it proves | Test | Suite |
| --- | --- | --- | --- |
| `T-01` | 200 racing consumes for a balance of 100: exactly 100 succeed | `TestT01AtomicDecrementUnderContention`, `TestT01TwoHundredConsumesOverHTTP` | integration |
| `T-02` | 50 deliveries of one key deduct once and answer one state | `TestT02FiftyDuplicateDeliveriesAreChargedOnce`, `TestT02AReplayReportsTheRecordedStateNotTheLiveOne`, `TestT02FiftyDeliveriesOverHTTP` | integration |
| `T-03` | A reused key with a different operation is refused and the record survives | `TestT03AReusedKeyIsRefusedAndTheRecordSurvives` | integration |
| `T-04` | The ceiling `limit + bonus` holds | `TestT04TheCeilingHolds` | integration |
| `T-05` | A transition never moves backwards, and the request path and the reset worker racing on one boundary roll it once | `TestT05TheTransitionIsMonotonic`, `TestTheWorkerAndTheRequestPathRollABoundaryOnce` | integration |
| `T-06` | Missed boundaries are skipped, not replayed, and a plan applied across an outage converges | `TestT06MissedBoundariesSkippedIsNotMissed`, `TestT06PlanAppliedAcrossAnOutage` | integration |
| `T-07` | Every row of the boundary table | `TestBoundaryMatrix` | integration |
| `T-08` | A refusal changes nothing | `TestT08ADenialChangesNothing`, `TestT08EveryRefusalChangesNothing` | integration |
| `T-09` | A crash cannot produce a partial mutation | `TestT09AFailedRequestLeavesNothingBehind`, `TestT09KillDuringConsume` | integration |
| `T-10` | A restart loses and grants nothing | `TestT10ARestartReReadsAndGrantsNothing`, `TestT10RestartChangesNothing` | integration |
| `T-11` | Four-way reconciliation over a seeded dataset | `TestT11EveryCycleReconcilesAndAMismatchIsReported` | integration |
| `T-12` | A wide `apply-now` needs a confirmation | `TestT12WideApplyNeedsAConfirmation` | integration |
| `T-13` | A full store refuses rather than displaces | `TestAFullStoreRefusesRatherThanDisplaces` | reference |
| `T-14` | A store loss inside the window is reversed once | `TestT14AStoreLossInsideTheWindowIsReversedOnce` | integration |
| `T-14` | The same, against the reference stack | `TestAStoreLossInsideTheWindowIsReversed` | reference |
| `T-15` | A delete is not retroactive | `TestT15DeleteIsNotRetroactive` | integration |

## 4. The v0.1 scope, line by line

Every line of [mvp-scope.md](../product/mvp-scope.md#3-definition-of-done-per-capability) §3, in
its order, with the test that ticks it.

| Capability | Line | Test |
| --- | --- | --- |
| Metered consumption | One script does idempotency, rollover, ceiling and deduction | `TestTheSixScriptsShareOneTransitionBlock`, and a reading of `internal/script` |
| | 200 racing for 100: exactly 100 | `TestT01AtomicDecrementUnderContention`, `TestT01TwoHundredConsumesOverHTTP` |
| | One key 50 times deducts once | `TestT02FiftyDuplicateDeliveriesAreChargedOnce`, `TestT02FiftyDeliveriesOverHTTP` |
| | A reused key with a different amount is `409` and changes nothing | `TestT03AReusedKeyIsRefusedAndTheRecordSurvives` |
| | `check` mutates nothing | `TestCheckChangesNothingInsideACycle` |
| Cycle and rollover | Idle across three boundaries is placed, not replayed | `TestIdleAcrossBoundaries`, `TestT06MissedBoundariesSkippedIsNotMissed` |
| | The request path and the worker race to one result | `TestTheWorkerAndTheRequestPathRollABoundaryOnce` |
| | A rollover already behind is rejected | `TestT05TheTransitionIsMonotonic` |
| | The boundary table, with DST and `never` | `TestBoundaryMatrix`, `TestDaylightSavingPair`, `TestNeverHasNoBoundary` |
| | `GET /v1/balance` renders the cycle in the tenant's zone | `TestBalanceRendersTheCycleInTheTenantsZone` |
| Money-touching operations | A refund above the ceiling is `409 refund_exceeds_grant` | `TestEndpointErrorMatrix` |
| | A refund on `never` is applied and never expires | `TestABalanceHashNeverExpires` |
| | Every applied mutation appends one reconciling event | `TestEveryAppliedMutationAppendsOneEvent`, `TestTheLedgerReconcilesEveryCycle` |
| Control plane | A plan edit lands at each tenant's next boundary | `TestPlanEditLandsAtEachTenantsOwnBoundary` |
| | `apply-now` re-anchors, with a reason, before and after | `TestApplyNowReanchorsAndIsAudited` |
| | `apply-now` above the threshold needs a single-use token | `TestT12WideApplyNeedsAConfirmation` |
| | The impact preview mutates nothing; an empty plan is zero | `TestImpactChangesNothing` |
| | A tenant's plan change is a hard reset with `plan_changed` | `TestAPlanChangeIsAHardReset` |
| | Suspend denies; resume keeps the balance | `TestSuspensionDeniesAndPreservesState` |
| | An archived feature denies and keeps balances | `TestArchivingAFeaturePreservesItsBalance` |
| | Deleting an assigned plan is `409 plan_in_use` | `TestDeletingAnAssignedPlanIsPlanInUseOverHTTP` |
| Auth | No key is recoverable from the database, logs or metrics | `TestNoKeyIsRecoverableAfterIssuance`, `TestNoLogLineCarriesCustomerDataOrASecret` |
| | `runtime` cannot reach `/v1/admin`; `admin` can reach the data plane | `TestARuntimeKeyIsRefusedOnEveryControlPlaneRoute`, `TestAnAdminKeyWorksOnTheDataPlane` |
| | A revoked key is refused here at once, elsewhere within NFR-D3 | `TestRevokeReachesASecondInstanceWithinTheBound` |
| | The bootstrap key is logged at `warn` by prefix | `TestBootstrapKeyIsIssuedOnceOnAnEmptyTable`, `TestKeyCreateWarnsOnce` |
| Operations | `docker compose up` works with no manual step | `tools/quickstart.sh` in CI, `TestTheQuickstartIsTheDeploymentGuide` |
| | A control-plane outage spares cached tenants | `TestPostgresOutageSparesCachedTenants`, `TestPostgresOutageUnderLoad` |
| | A store outage is a `503` in budget, never partial, never an allow | `TestADeadDataStoreIs503WithinTheBound`, `TestChaosStoreLatency` |
| | A Postgres outage spares cached tenants | `TestAPostgresOutageKeepsTheFenceMode` |
| | `/healthz` and `/readyz` are distinct and correct | `TestTheProbesAnswerTheContract`, `TestReadinessFollowsTheDataStore`, `TestHealthcheckAsksItsOwnReadyz` |
| | Every documented metric is emitted | `TestEveryDocumentedMetricIsServed` |
| Contract and documentation | The binary serves `api/openapi.yaml` | `TestServedContractMatchesTheEmbeddedDocument` |
| | Every catalogued error is in the contract | `TestContractAndCatalogueAgree` |
| | The README quickstart runs in CI against a real stack | `tools/quickstart.sh` in the `reference` job |

`tools/testreport`'s `TestEveryTestTheEvidenceNamesExists` fails CI if any test this document
names stops existing.

## 5. Named gaps

1. **The runbook and restore review.** Closed 2026-10-29. Claude followed every runbook and the
   restore drill on the reference stack with each fault injected, the owner approved the report, and
   the row is in [the runbooks' review table](../runbooks/README.md#review). Its fixes, and those of
   the code review that followed, are in CHANGELOG.
2. **NFR-L6, control-plane p99 ≤ 300 ms, is not measured.** No admin load test exists. Nothing in
   v0.1 depends on the figure, and the NFR says it is irrelevant to correctness. The data-plane side
   of that row, that admin load is not visible on the data plane, is not measured either.
   [Q-31](../product/assumptions-and-open-questions.md#2-open-questions) records this, with a
   default.
3. **The burst run's latency is information, not a gate.** At 10,000 a second on two CPUs, the p99
   was 302 ms at the client. That is queueing, which NFR-T2 allows. NFR-L3's "10× expected peak"
   is undefined until [Q-30](../product/assumptions-and-open-questions.md#2-open-questions) is
   answered, and this run is the figure it will be compared against.
4. **The soak ran for 5 h 30 m, not 24 h.** A hosted job ends at 6 h. `QUOTACORE_SOAK_DURATION`
   runs the same test for 24 h on a self-hosted runner, with nothing else changed (IP-16 Q2).
5. **The release acts are not taken.** v0.1 does not yet have a merge of `dev` into `main`, a `v0.1.0`
   tag, or the signed image that `release.yml` publishes from that tag. Those are a maintainer's
   steps (IP-16 D19).
