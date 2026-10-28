# v0.1 evidence

Every claim v0.1 makes, the measurement or test behind it, and where that was taken, in one place
(`IP-16`'s exit criterion). A reader who doubts the product starts here. Each row names the test that
proves it, so the claim can be re-run rather than believed.

This document is being written by `IP-16`. Sections 1, 2, 4 and 5 are filled in from the release
run, and until then they say so.

---

## 1. The three claims

Recorded from the release run.

## 2. Non-functional requirements, measured

Recorded from the release run.

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

Recorded at close: every line of [mvp-scope.md](../product/mvp-scope.md#3-definition-of-done-per-capability)
§3 with the test that ticks it.

## 5. Named gaps

Recorded at close.
