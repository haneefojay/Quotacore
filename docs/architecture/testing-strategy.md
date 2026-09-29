# Testing Strategy

The product's central claims are testable properties, so the test suite is not a safety net bolted
on to the design — it **is** the demonstration of the design. This document states what is proven
how, and which claims are deliberately not automatable.

The rule that shapes everything: **a claim in the documentation with no test behind it is a
marketing claim.** Every assertion in the "verified by" columns of the other architecture
documents resolves to a test named here.

---

## 1. Layers

| Layer | What it proves | Where it runs | Gate |
| --- | --- | --- | --- |
| Unit | Pure functions: cycle arithmetic, boundary computation, validation, formatting, fingerprinting | Every commit | Blocking |
| Property | Invariants hold for generated inputs, not just chosen ones | Every commit | Blocking |
| Script | The Lua scripts behave exactly as specified, including under injected concurrency | Every commit, against a real engine | Blocking |
| Integration | The whole service against real Postgres and a real data store | Every commit | Blocking |
| Contract | The OpenAPI document, the generated code and the error catalogue agree | Every commit | Blocking |
| Concurrency | The atomicity and monotonicity claims, under real contention | Every commit | Blocking |
| Chaos | Behaviour when dependencies fail, are slow, or vanish | Nightly | Blocking for the release |
| Load and soak | The latency and throughput targets | Nightly | Blocking for the release |
| Security | The controls in [security-model.md](security-model.md) §11 | Every commit | Blocking |
| Restore drill | Recovery works, performed by a clean checkout | Nightly | Blocking for the release |

**Real dependencies, no mocks, below the integration layer.** A mock of Redis proves the mock
works. The scripts, `EVALSHA` semantics, `EXPIREAT` behaviour, `noeviction` write rejection, Postgres
constraint behaviour and transaction isolation are exactly the things that differ between
implementations and versions, and a mock hides every one of them. The engine matrix runs the
integration suite against each supported engine, because the whole reason to abstract over the
data store is that we claim it works with more than one (A-09).

---

## 2. Correctness tests, the ones that matter

These are the tests the product exists to pass. Each maps to a claim in the documentation.

### T-01 — Atomic decrement under contention

```
Given  a tenant with balance 100 and a single feature
When  200 concurrent consumes of amount 1 arrive
Then  exactly 100 return 200
And   exactly 100 return 429 quota_exceeded
And   the final balance is 0
And   exactly 100 usage_events rows exist, each summing to 0
And   every 429 response reports balance = the value at the time it was denied
```

The last assertion matters. A denial that reports a stale balance sends the customer into a
debugging session that ends in distrust. Run with 1, 2, 4, 8, 16 and 32 concurrent clients, since
a race that only appears at high concurrency is the normal case.

### T-02 — Idempotent replay

```
Given  balance 100
When  the same Idempotency-Key is sent 50 times concurrently with amount 30
Then  exactly one deduction of 30 occurs
And   the final balance is 70
And   all 50 responses carry the same state: balance, limit, bonus, cycle index and both window ends
And   the one applied response and the 49 replays differ only in `replayed` and `Idempotent-Replay`
And   exactly one usage_events row exists
And   each response after the first has Idempotent-Replay: true
```

The state every caller is told must be identical, because a replayed response that differs in a
timestamp or a rounding artifact forces clients to diff, and a client that diffs will eventually
decide the service is inconsistent. The responses themselves are not byte-identical, and that is
deliberate: the first body says `applied` and the rest say `replayed`, so a client can tell a
first delivery from a replay without comparing bodies (DR-027, api-conventions.md §6 and §7.1).

### T-03 — Idempotency fingerprint mismatch

```
Given  a key already used for feature A, amount 30
When  the same key is used for feature B, or for amount 31
Then  409 idempotency_key_reuse
And   the balance is unchanged
And   the original record still replays correctly
```

The last assertion covers INV-I2: a mismatched attempt must not damage the original record. This
is a realistic corruption path and it is easy to get wrong by overwriting the record before
comparing.

### T-04 — Ceiling invariant

```
Given  limit 100, bonus 0, balance 0
When   a refund of 100 is applied            → balance 100, 200
And    a further refund of 1 is applied       → 409 refund_exceeds_grant, balance still 100
And    an admin grant of 1 is applied         → 409 grant_exceeds_ceiling, balance still 100
```

Run over every state in a generated state space: all combinations of limit, bonus and balance
within range, with every operation applied in every order. This is a property test, and it is the
one that most reliably finds an ordering bug in a script (see section 3).

### T-05 — Monotonic rollover

```
Given  a tenant with cycle_index 5
When   a transition is attempted with target_index 4
Then   it is refused, nothing changes, and the caller treats it as success
And    the same tenant with target_index 7 advances once, to 7
And    only one cycle_rolled_over event exists
```

Plus the race: the request path and the worker both target the same boundary, in both orderings,
in a loop, asserting exactly one transition and one event every time.

### T-06 — Missed boundaries

```
Given  a monthly tenant idle for 97 days
When  any data-plane endpoint is called
Then  the tenant is in the current window
And   its balance is the current limit, not a multiple of it
And   exactly one cycle_rolled_over event exists
And   the cycle_index jumped by 3, not by 1
```

Parametrised over 1 hour, 1 day, 31 days, 97 days, 400 days and 4 years, because a rollover bug
that only appears after a long idle period is the kind that is discovered in an incident.

### T-07 — Boundary table

Every row of the table in [cycle-engine.md](cycle-engine.md#6-test-matrix), asserted as an exact
RFC 3339 value with offset. This is a data-driven test: the table is a test fixture, so adding a
row to the documentation is adding a test, and a documented behaviour that is not in the table is
a behaviour nobody has decided.

### T-08 — Denials change nothing

For every error code in [error-catalog.md](../product/error-catalog.md), the failing request is
followed by an assertion that the balance, the bonus, the cycle index, the event count and the
idempotency record are all unchanged. Parametrised over the catalogue, so a new error code cannot
be added without this test.

### T-09 — Atomicity under failure

```
Given  a consume of amount 30 against balance 100
When  the process is SIGKILLed at a randomised point during the request
Then  on restart, the balance is either 100 or 70
And    never any other value
And    the event count is 0 or 1
```

Run 200 times at randomised delays. This is the test that proves the strongest claim in the
system — that a crash cannot produce a partial mutation — and it can only be written against a
real engine, because the guarantee comes from the engine's execution semantics.

### T-10 — Restart safety

Kill the process during normal traffic; restart; assert that every balance, cycle index and
idempotency record is byte-identical to a snapshot taken immediately before the kill. Run
repeatedly, including during a rollover.

### T-11 — Reconciliation

For every tenant and feature in a generated dataset, assert the four-way agreement from
[consistency-and-recovery.md](consistency-and-recovery.md#5-reconciliation). This runs in the
nightly suite against a dataset that has had thousands of operations, rollovers, grants, sets and
plan changes applied to it, because a reconciliation test on a clean dataset proves nothing.

### T-12 — Wide `apply-now` needs a confirmation

A `POST /v1/admin/plans/{id}/apply` against a plan with more assigned tenants than
`QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD` asserts, in order: `409 confirmation_required`; **no**
tenant re-anchored; the returned `affected_tenants` equal to the counted number; the returned
`earliest_cycle_end` equal to the minimum affected boundary. Then the same call with the token
applies. Then a third call with the same token is refused. Two further cases: a plan whose assigned
tenants equal the threshold exactly, which applies without a token; and a token issued against
version *n* replayed after the plan moves to version *n+1*, which is refused. The negative assertion
matters most — a `409` that had already half-applied the change would pass every other check.

### T-13 — A full store refuses rather than displaces

Fill the fast store to `maxmemory` with idempotency records, then issue a `consume` whose key is
absent. Assert `503 service_unavailable`, that `balance` is unchanged, and that a **pre-existing**
idempotency record inside its window is still readable afterwards. That last assertion is the one
that distinguishes `noeviction` from `allkeys-lru`, and it is the reason the policy is a rule rather
than a configuration default. Read `INFO memory` and `CONFIG GET maxmemory-policy` at the start, so
the test fails loudly if the deployed policy drifts from `noeviction`.

### T-14 — A store loss inside the window is reversed

```
Given  a consume of 30 that completed, with its idempotency record written
When  the fast store is flushed, then rebuilt from the ledger
And    the client retries the identical request inside the 24 h window
Then   the second deduction is applied, because the record is gone
And    reconciliation detects the duplicate event_id
And    exactly one refund of 30 is issued against that key
And    the tenant's net balance equals a single charge of 30
And    a second reconciliation pass issues no further refund
```

The third assertion is the one that makes the guarantee honest: the system is not pretending the
second charge did not happen, it is undoing it. Runs in the nightly suite because it requires a
flush and a rebuild.

### T-15 — Delete is not retroactive

Race a `consume` against `DELETE /v1/admin/tenants/{id}` at randomised interleavings, over many
runs. Three outcomes are permitted and nothing else is: the consume completes and its deduction
stands; the consume is denied with `404 tenant_not_found`; or the consume is denied with
`409 tenant_deleted`. A consume that completes and is then unwound, or one that succeeds against a
tenant deleted before the request, is a failure. Assert the audit log shows the delete and the
completion in commit order.

---

## 3. Property tests

Where the invariants are simple enough to state and the state space is too large to enumerate.
The generator is not a fuzzer over bytes; it is a generator over **valid operation sequences**.

| Property | Generator | Assertion |
| --- | --- | --- |
| `0 <= balance <= limit + bonus` at every step | Random sequences of consume, refund, grant, set, rollover, plan change, override | The invariant holds after every step. Any violation is a P0 defect with a minimal reproducing sequence |
| `cycle_index` never decreases | Random sequences including out-of-order worker invocations | Monotonicity holds |
| A denial changes nothing | Random invalid operations | State is byte-identical before and after |
| The same key never applies twice | Random sequences with deliberate repeats | One deduction per key, always |
| `balance == opening + Σ(delta)` per cycle | Random sequences crossing boundaries | The DR-042 identity holds for every cycle |
| The projected window equals the computed window | Random anchors, intervals, zones and times | The transition's view and the engine's pure function always agree |
| Rounding and formatting round-trip | Random instants and zones | A rendered timestamp parses back to the same instant at the same offset |

**Shrinking is mandatory.** A property test that reports a violation with a 4,000-step sequence
and no minimal example will be ignored, and therefore will not be fixed. The generator shrinks to
the shortest failing sequence, and the shrunk sequence is committed as a regression test with a
name describing the defect.

## 4. Script tests

The scripts are tested directly, against a real engine, without the HTTP layer in the way. This
is possible because the scripts are static and take everything they need as `ARGV` (NFR-S8).

| Test | What it proves |
| --- | --- |
| The happy path, per script | The expected balance and return shape |
| Every return branch | Each documented status is reachable, which stops dead code accumulating in a script |
| `EXPIREAT` set to `window_end + 24h` | INV-C6, asserted by reading `PTTL` |
| `reset_interval = never` | No expiry is set at all, asserted by reading `PTTL` as `-1`, and the balance survives a simulated boundary (DR-008). The mirror of the row above: a `never` entitlement is the case where the key outlives every cycle |
| No `KEYS`, no `SCAN`, no unbounded loop | A source-level assertion over the script text, plus a fuzz test asserting execution time is flat as metadata and key length grow |
| A `NOSCRIPT` condition | The client re-registers with `SCRIPT LOAD` and the request succeeds |
| A key with 200 features in the plan | Execution time is unchanged, proving O(1) in customer-controlled data |
| A balance hash that does not exist | `503 service_unavailable` and `balance_key_missing_total` rises. The script never initialises it from `ARGV` (DR-045) |
| Argument validation inside the script | A hand-crafted call with a malformed argument is refused, so a future caller cannot bypass the Go validation |
| The `2^53` bound | Values above it are refused inside the script, matching the API and the DDL |

The last one exists because there are three enforcement points for the bound and a later change
could tighten only two.

## 5. Load and soak

| Test | Shape | Pass condition |
| --- | --- | --- |
| Warm latency | 100 req/s, 10 min, after a warm-up | p50 ≤ 1 ms, p99 ≤ 5 ms, p99.9 ≤ 20 ms (NFR-L1 to L3) |
| Sustained throughput | 2,000 req/s, 30 min | No error, no unbounded memory growth, p99 stable rather than creeping |
| Burst | 10,000 req/s for 60 s | Queueing acceptable, errors not |
| Boundary-crossing | Requests timed to land on a boundary | Rollover overhead ≤ 1 ms (NFR-L7) |
| Tenant scale | 50,000 tenants, 100 features on one plan | Snapshot within its memory cap, p99 unchanged from the small-dataset run |
| Idempotency scale | 2,000 req/s for 24 h, projected | The data store's memory projection in [deployment.md](deployment.md#5-sizing) is correct, and no `noeviction` rejection occurs |
| Ledger saturation | More than the queue can hold | Drops occur, `event_sink_dropped_total` rises, **enforcement latency is unchanged** (FS-10) |
| Soak | 24 h at moderate load | No leak, no goroutine growth, no file-descriptor leak, no clock or counter drift |

The ledger-saturation test asserts a negative: that enforcement latency is *unchanged* under
ledger pressure. That is the property that justifies dropping events rather than blocking, and it
needs a test, because the intuitive alternative is to block.

## 6. Chaos tests

Run nightly, against a real stack, with fault injection at named points.

| Test | Injection | Expected |
| --- | --- | --- |
| Data store unreachable at request time | Network partition | `503` within 250 ms, no partial state, no hang (NFR-L4) |
| Data store killed mid-request | `SIGKILL` the container | `503` or a success with a complete deduction. Never anything else |
| Data store flushed | `FLUSHALL` | Every affected tenant gets `503`, `balance_key_missing_total` rises, and **no tenant is granted a full allowance** (DR-045). This test exists to prove the fail-closed behaviour and the detection, not to prove the outcome is fine |
| One balance hash deleted mid-cycle | `DEL` on a single `qc:{t:T}:bal:F` key | That tenant-feature alone is `503`; every other tenant is unaffected. A per-key loss must not be a per-tenant outage, and must not become free service |
| Postgres unreachable | Stop the container | Cached tenants enforce normally; uncached tenants get `503 control_plane_unavailable`; `dbpool_wait` does not rise (NFR-A2) |
| Postgres unreachable before startup | Start with no database | The service **refuses to start** |
| Postgres restored from a backup | Restore to a new database, restart, reconcile | Differences are reported and confined to the truncation window (§4.2) |
| Latency injection, 400 ms | Delay the data store | Every request fails within the budget; the queue does not grow without bound |
| Latency injection, 2 s | Delay the data store | Same. The timeout is not a suggestion |
| Pub/sub unavailable | Break the channel | Propagation widens to the refresh interval; `invalidation_lag` rises; correctness holds |
| Clock jump, +24 h | Change the host clock | Boundaries shift; `rollover_stale` or an advance; the alert fires |
| Clock jump, −24 h | Change the host clock | Every transition returns `STALE`; nothing moves; the tenant is stuck until the clock recovers; the alert fires. This is the correct behaviour of a monotonic guard, and asserting it is the point |
| Slow drain of the ledger writer | Fill the queue, then stop the writer | Drops are counted; enforcement is unaffected |
| SIGTERM under load | Graceful shutdown | In-flight requests complete; no partial state; 30 s bound (NFR-A5) |
| Disk full on Postgres | Fill the volume | Writes fail loudly; the service does not silently accept and drop administrative mutations |

The clock-jump tests are the ones most likely to be removed because they are inconvenient. They
are also the only proof that a monotonic guard behaves correctly when the world is inconsistent,
and the failure they protect against is a tenant receiving a free allowance.

## 7. Contract tests

| Test | Assertion |
| --- | --- |
| Catalogue completeness | Every error code in [error-catalog.md](../product/error-catalog.md) exists in the OpenAPI document with the documented status, and every code in the document exists in the catalogue |
| Metrics completeness | Every code also appears in `quotacore_errors_total`'s documented label set (section 1.2 of [observability.md](observability.md)) |
| Example validation | Every example in the specification parses and validates |
| `additionalProperties: false` | A typo in a request field is a `400`, not a silently ignored field |
| Envelope shape | Every documented non-2xx response validates against the envelope schema |
| Endpoint × error matrix | Each endpoint returns only the codes its row of the matrix lists, and returns `400 validation_failed` for the others |
| Generated types | The Go and TypeScript types rebuild from the specification, and the served validation uses them |
| Deprecated surface | A deprecated endpoint sends `Deprecation` and `Sunset` |

The endpoint × error matrix test is the one that prevents the catalogue and the implementation
from diverging, which is the failure mode that makes an error catalogue fiction.

## 8. Security tests

The list in [security-model.md](security-model.md) §11, executed rather than described. In
summary: no plaintext secret in any store or log; every admin path refuses a runtime key;
revocation is immediate on the next request; the ceiling holds for every error code; the `2^53`
bound is enforced in all three places; the `external_id` constraint rejects an email address; Lua
metacharacters in every string field are fuzzed; the service refuses to start with an
unacknowledged bootstrap key; and a network-policy test asserts no outbound connection other than
to the two datastores.

Two of these deserve their own emphasis because they are the most likely to be quietly dropped:

- **The no-egress test.** It is the only automated check that the product does not phone home,
  which is a stated value proposition (NFR-S3). A regression here is invisible otherwise.
- **The no-plaintext-secret test.** It runs over the database, the logs *and* the metrics
  endpoint, because a key in a metric label is a key in a monitoring system forever, and label
  cardinality rules are exactly the kind of thing that gets relaxed under pressure.

## 9. Restore drill

Nightly, from a clean checkout, against a seeded stack. The full procedure from
[consistency-and-recovery.md](consistency-and-recovery.md#41-data-store-lost-or-corrupted--total-loss):

1. Seed a dataset with thousands of operations across many tenants, features, intervals and time
   zones, deliberately including mid-cycle `set`, `grant` and `plan_changed` events.
2. Snapshot the expected balances.
3. Flush the data store entirely.
4. Run the rebuild.
5. Assert every tenant that the rebuild claims to have reconstructed matches the expected balance
   **exactly**, and that every tenant it flags as unreconcilable was in fact unreconcilable.
6. Assert the service was in read-only mode for the whole rebuild, and that no tenant was granted
   a full allowance by any path at any point (DR-045).

Step 6 is the assertion that matters most. A recovery procedure that silently grants every
customer a fresh allowance while "restoring" is worse than no recovery procedure, and it would
pass a test that only checked the reconstruction accuracy.

## 10. What is not tested automatically, and why

| Not tested | Why | What substitutes for it |
| --- | --- | --- |
| Latency from the customer's network | Not in our environment | The deployment checklist requires a load test in situ, and the docs say why the number differs |
| Behaviour under a customer's own bad code | Not knowable | The error catalogue, the FS-03 disclosure, and the integration recipe for the customer's CI |
| That a customer reads the documentation | Not testable | Plain language, error messages that name remedies, and J-1's time-to-first-enforcement as the metric |
| That the competitive position holds | Not a software property | The assumptions register, the dated accepted risk in its [accepted-risks](../product/assumptions-and-open-questions.md#accepted-risks) table, and the five design-partner conversations scripted in [a-01-design-partner-validation.md](../research/a-01-design-partner-validation.md) |
| Human response to a P0 alert | Not a software property | The runbooks, and the requirement that a person who did not write them can follow them |
| True availability over months | Out of scope for a test suite | The NFRs state it, and the deployment guide states which topology can honestly offer it |

## 11. Test data and determinism

| Concern | Rule |
| --- | --- |
| Time | A single injectable clock. No test calls `time.Now()`. A test that depends on the wall clock is a flaky test, and a flaky correctness test gets deleted rather than fixed |
| Randomness | Seeded and logged. A failing property test prints its seed, so it is reproducible |
| Identifiers | Deterministic UUIDv7 sequences per test, so a failure is readable |
| Money-shaped values | Boundary values are always in the dataset: 0, 1, limit−1, limit, limit+1, ceiling, ceiling+1, and `2^53` |
| Time zones | Every test involving a boundary runs in UTC, `Europe/Berlin`, `America/New_York` and `Asia/Kolkata`. Kolkata is in the set specifically because a half-hour offset breaks assumptions that a whole-hour offset hides |
| Isolation | Each test gets a fresh keyspace by prefix and a fresh schema. Parallel tests never share state, and a shared-state test is a source of phantom failures |
| Data volume | Property tests run small and fast in the per-commit suite, and large in the nightly suite. A property test that takes 40 minutes does not get run |
