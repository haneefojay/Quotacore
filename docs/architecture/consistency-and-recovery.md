# Consistency and Recovery

What the system guarantees about state, what it deliberately does not guarantee, and exactly how
to recover when each store is damaged. Written so that an engineer at 03:00 with an alert can
find the procedure without reading the design.

---

## 1. The consistency claim

**One sentence:** a balance is strongly consistent per tenant and per feature, and every applied
mutation is atomic, idempotent and monotonic.

Everything else follows from that sentence, including the things that are *not* guaranteed.

| Property | Guaranteed | Mechanism |
| --- | --- | --- |
| Atomic deduction | Yes | One script execution (ADR-0002) |
| No overspend under concurrency | Yes | Same execution; a denial is a return, not a race (NFR-T3) |
| No double charge on retry | Yes, for a reused idempotency key | Record written in the same execution (DR-030) |
| Correct cycle advance | Yes, monotonically | Pure-function boundary plus a backwards-refusing transition (ADR-0003) |
| Correctness with no background worker | Yes | The request path advances the cycle itself (DR-009) |
| Correctness with a duplicated worker | Yes | Monotonic and idempotent transition (INV-C2) |
| No partial mutation on error | Yes | Atomic execution; a `500` implies nothing was applied |
| Balance reads | Yes, strongly consistent, from the store | Never from a cache (INV-X1) |
| Configuration reads | **No** — a bounded propagation delay | Snapshot plus pub/sub (NFR-D3) |
| Ledger completeness | **No** — best-effort, with a visible gap | Bounded queue and a drop counter (ADR-0016, FS-10) |
| Cross-plane agreement | **No** — a stale limit is possible for ≤ the propagation bound | Snapshot staleness is bounded and self-correcting (INV-X8) |
| Durability of a full-allowance balance | **No** — the fast store is rebuildable, not durable | Restore from configuration plus ledger (NFR-D7) |
| Durability of an idempotency record | **No, and not rebuildable.** The ledger records usage, not keys | Reverse the double charge once detected (DR-049, NFR-T10). This asymmetry is why the second claim is worded as it is |

The three "no" rows are the design, not oversights. Each is chosen so that the "yes" rows can be
absolute. A system that claimed all of them would be claiming something it could not deliver.

> **Amended 2026-10-20 (`IP-10`).** "Correct cycle advance, monotonically" holds under one
> anchor. A plan change or an apply-now commits a new anchor, the tenant's new pins and a pending
> re-anchor in one Postgres transaction; the balance hashes are rewritten after it by a
> compare-and-set on the window each was read with, inline while the request has time and by
> every instance's background drain otherwise. Until a tenant is drained it is served under its
> previous numbering, which may be stale but is never a fresh allowance, and a rewrite that runs
> twice changes it once.

## 2. Why a stale configuration cannot produce a wrong cycle

This is the subtle interaction between the two planes, and it is why the cycle state lives in the
fast store while the anchor lives in Postgres.

A stale snapshot can supply a stale **limit**, a stale **interval**, a stale **anchor**, or a stale
**tenant state**. Consider each:

| Stale field | Worst effect | Why it is bounded |
| --- | --- | --- |
| Limit | A request is measured against the previous limit | Corrected at the next cycle, and at worst for the propagation interval. Money moves; correctness of the *boundary* is unaffected |
| Interval | A boundary is computed with the previous interval | The boundary is recomputed at every request from the stored index, so the error self-corrects at the next request that crosses a boundary. It can shift one window, not skip or duplicate one |
| Anchor | A boundary is computed from the previous anchor | A changed anchor is applied by a plan change, which is itself a hard reset and a re-anchor, so a stale anchor produces a *stale but valid* window that converges on the next refresh |
| Tenant state | A suspended tenant is not denied, or a deleted tenant is | Bounded by the propagation interval. This is the worst of them, and it is a **security** property, which is why the propagation bound is tight, published, and alerted on |
| Feature state | An archived feature is still consumable | Same bound, same reasoning |

> **Amended 2026-10-20 (`IP-10`).** The interval and anchor rows assumed an interval could change
> under a running tenant. It cannot: a tenant's cadence is pinned until its next re-anchor, so a
> stale snapshot can supply a stale interval only across a re-anchor, together with a stale
> anchor. A stale anchor is bounded by the refresh interval, which is refused at or above one
> hour, so a stale snapshot can never outlive the shortest first window under a new anchor. A
> request computed under the stale anchor therefore meets either the hash not yet rewritten, which
> it agrees with, or the hash rewritten into the new anchor's first window, which has not closed;
> the transition rolls only once the stored window has closed, so it answers stale and changes
> nothing (C-3).

The key point: **a stale configuration can produce a stale *decision*, never an inconsistent
*state*.** Two requests can be answered differently, but the balance after each is a legal state
produced by a legal transition. That is what makes a bounded propagation delay acceptable here
and unacceptable for a balance read.

## 3. Dependency failure matrix

| Failure | Enforcement | Control plane | Ledger | Recovery |
| --- | --- | --- | --- | --- |
| Data store unreachable | `503 service_unavailable`, fail-closed, ≤ 250 ms | unaffected | queues and then drops | Automatic on recovery. No restart, no reconciliation (NFR-A4) |
| Data store unreachable, worker running | Worker logs and counts, continues | unaffected | as above | Automatic |
| Postgres unreachable | Unaffected for cached tenants; `503 control_plane_unavailable` for uncached | down | the batch in hand is held and retried; the queue fills, then new events drop as `queue_full` (amended 2026-10-20, `IP-12`: this read "unaffected, appends continue", which no writer to an unreachable database can do) | Automatic. Cache miss rate is the leading indicator |
| Postgres unreachable at startup | Service **refuses to start** | down | n/a | Fix the database. Refusing is correct: a service that cannot validate its configuration should not start and serve wrong answers |
| Both unreachable | `503 service_unavailable` | down | drops, counted | Automatic |
| Pub/sub unavailable | Correct; propagation widens to the refresh interval | unaffected | unaffected | Automatic. `quotacore_invalidation_lag_seconds` rises |
| Ledger sink saturated | **Unaffected.** Enforcement continues | unaffected | drops, counted | None needed. The gap is visible and bounded by the queue depth times the drain rate |
| Clock skew | Boundaries shift | unaffected | unaffected | Operator action. NTP. Alerted (NFR-D8) |
| Process killed mid-request | No state change; the script is atomic | unaffected | at most one dropped event | Automatic |
| Process killed during an apply-now | Postgres transaction rolls back; the fast store is untouched until commit | recovers | unaffected | Automatic. This is why `apply-now` writes Postgres first and the fast store second |
| Data store unreachable during a plan change or an apply-now (`IP-10`) | The change commits with its pending re-anchors; the tenants are served their previous state, or `503` while the store is down | unaffected | the `plan_changed` events wait for the rewrite | Automatic. Every instance drains pending re-anchors at start and on each refresh tick |
| Data store unreachable during a grant, set, force-rollover or immediate override (`IP-10`) | `503 service_unavailable`; the transaction rolls back and no audit row is written | unaffected | nothing to hand over | Retry. Nothing was applied |
| Disk full on the data store | `503` (write errors surface) | grows | grows | Operator action. Alerts fire on disk usage before this happens |
| Bad data written by an operator | Depends on the field | — | — | `force-rollover` with an audit record. The monotonic sanity ceiling turns an unbounded index into an alert rather than a permanent freeze |

## 4. Recovery procedures

Written as runbooks. Each is rehearsed in CI where possible, and at least once by a person who
did not write it (A-19).

### 4.1 Data store lost or corrupted — total loss

**Impact.** No balances. Every tenant-feature whose balance hash is absent is un-enforceable and
returns `503 service_unavailable`, increments `quotacore_balance_key_missing_total`, and joins
the rebuild queue. The design does **not** fall back to granting the current limit, because for a
mid-cycle tenant that is indistinguishable from free service (DR-045).

**Detection.** `quotacore_balance_key_missing_total` rising above zero, or
`GET /v1/admin/status` reporting an empty key space.

**Procedure.**

1. **Do not restart the service in normal mode.** Read-only mode is the safe default, and
   DR-045 means a restart cannot grant a full allowance by itself — but it would convert a
   visible, countable fault into a quieter one.
2. Set `QUOTACORE_STARTUP_MODE=readonly` so the process serves status and health but performs no
   writes to the fast store.
3. Restore Postgres from backup. Confirm the backup's timestamp precedes the loss.
4. Run the **rebuild** command, which for each tenant and feature replays `usage_events` for the
   **current cycle only** and writes the reconstructed balance.
5. Compare the reconstruction against the configuration's current limit, and report every tenant
   whose reconstructed balance does not reconcile.
6. For tenants where reconciliation is not possible, the operator chooses explicitly: restore
   from the ledger as-is, or set the balance and record a `set` event with a reason.
7. Switch back to normal mode.

> **Amended 2026-10-20 (`IP-11`).** Steps 2 to 6 are now commands, and "read-only for the whole
> rebuild" is a property of the fleet rather than a runbook step.
>
> - **Step 2.** `QUOTACORE_STARTUP_MODE=readonly` refuses every write to a tenant's keys (balance
>   hashes and idempotency records) with `503 service_unavailable`, and nothing else: reads, key
>   revocation, feature and plan edits, suspension and the pagination cursor still work, because
>   they write Postgres or no tenant key. The re-anchor drain, the reconciliation pass and the
>   retention job do not run, `/readyz` is not ready, and `GET /v1/admin/status` reports `mode`.
>   Every instance in normal mode holds a shared Postgres advisory lock, the **fence**; a rebuild
>   that writes takes it exclusive, so it refuses to start beside a normal instance, a normal
>   instance refuses to start beside it, and an instance whose fence connection drops serves
>   read-only until it holds the fence again. The rebuild waits two fence checks after taking it
>   before it reads anything.
> - **Step 3** applies only when Postgres was lost too, because a restore truncates the very
>   ledger the rebuild reads; the operator then passes `--ledger-restored-at <instant>`, and every
>   cycle is unverified, since everything after that instant is gone.
> - **Step 4** is `quotacore rebuild`, a dry run that prints a JSON report, or with `--apply` a
>   write. It enumerates every tenant's effective entitlements from Postgres, and for each
>   tenant-feature whose hash is absent reads the whole ledger, the unresolved dead letters, the
>   writer's recorded gaps and any crashed writer's session (section 6), and classifies the
>   ledger's last cycle: **exact** (it reconciles and determines all six fields, no loss is
>   recorded in it, and its rows are inside retention), **unverified** (it reconciles, but a
>   recorded loss covers it), or **unreconcilable** (the identity or chain fails, a rejected dead
>   letter names it, a relimit emptied its balance so the limit is unknown, its rows name no
>   window, or it opened before the retention horizon with no opening row, which is every `never`
>   feature older than the retention). `--apply` writes only exact hashes, through a create-only
>   script that writes nothing where any hash exists, records an `unreconcilable` finding for each
>   unreconcilable one, and writes one `tenant.rebuild` audit row per tenant written. A hash that
>   is present is verified and never rewritten. A flagged tenant-feature stays absent and keeps
>   answering `503`.
> - **Step 5** compares the ledger with itself, not with the limit: every row against the one
>   before, and the hash against the last row, as section 5 describes.
> - **Step 6** is two flags of the same command, each for one tenant-feature and each audited
>   with a required `--reason`: `--accept <tenant>:<feature>` writes the ledger's estimate, which
>   is ADR-0016's bounded fail-open margin chosen explicitly for that tenant, and
>   `--set <tenant>:<feature>=<n>` writes `n` and appends a `set` row whose delta is `n` less the
>   estimate, so the cycle still reconciles.
> - **Step 7** is a restart of every instance with `normal`.

**What is guaranteed.** For a tenant whose only activity in the current cycle was consumption and
refund, the ledger reconstructs the balance exactly, because `balance = opening + Σ delta` within
a cycle (DR-042) and `balance_after` is stored on every event.

**What is not.** A tenant with a `set`, a `grant`, or a mid-cycle `plan_changed` may not
reconcile, because the opening allowance of the cycle is not itself an event. The rebuild flags
these rather than guessing. This is why full-loss recovery is described as valid for a
*full-allowance-recovery* case and not as a general restore (ADR-0016, NFR-D7).

> **Amended 2026-10-20 (`IP-12`).** Every opening that moves a balance is now a row: a
> `cycle_rolled_over`, `plan_changed` or `force_rolled_over` event whose `balance_after` is the
> cycle's opening, and a grant or `set` is an ordinary delta inside it
> ([data-model.md](data-model.md#27-usage-events-the-ledger)). Such a tenant therefore reconciles
> exactly over the rows that were written. What may still not reconcile is a cycle with a dropped or
> dead-lettered row (DR-042's stated limit) and a cycle longer than the event retention, whose
> earliest rows the retention job has deleted.

**Idempotency records are the one thing the rebuild cannot restore, and this is the important
consequence of total loss.** A balance is reconstructible because the ledger records usage with its
`balance_after`. An idempotency record is not reconstructible, because the ledger records *usage and
not keys* — nothing in `usage_events` says which `Idempotency-Key` produced a deduction. So after a
total loss:

1. Balances come back, exactly, for the reconcilable tenants.
2. Every idempotency record is gone, and a client retrying a pre-loss request inside its 24-hour
   window is charged a second time.
3. The `usage_events` partial unique index catches it, because the replayed request is a second
   deduction under the same `Idempotency-Key`. Amended 2026-10-20 (`IP-12`): this read "produces the
   same `event_id`". Each applied mutation mints its own `event_id`, so the replay has a new one; the
   index is on `(tenant_id, idempotency_key)`, and the refused event is kept in the dead-letter table
   for the reversal ([data-model.md](data-model.md#212-usage-event-dead-letters)).
4. Reconciliation therefore issues **exactly one** refund for the second deduction, restoring a
   single net charge (DR-049, NFR-T10, FS-22).

Step 4 is why a detected duplicate is a remedy and not merely a counter. Detection without a remedy
left the customer carrying the double charge, which is the outcome this whole path exists to
avoid. The correction is eventual rather than immediate, and the reversal is idempotent on
`event_id`, so a second reconciliation pass issues nothing further.

**This is the honest boundary of the second claim.** It is not "charged once, unconditionally". It
is "charged once, and a double charge caused by store loss is reversed". The alternative —
persisting the store so records survive a restart — was considered and rejected in
[ADR-0017](../decisions/0017-noeviction-and-duplicate-reversal.md), because an append on the
`consume` path puts the p99 and throughput requirements at risk to protect a window that reversal
already closes.

**Rehearsal.** Automated in CI, weekly: flush the data store, rebuild from a seeded ledger, assert
every reconciliation, then replay a pre-loss request and assert exactly one reversal (T-14). A
recovery procedure that has never been run is a hypothesis.

> **Amended 2026-10-20 (`IP-11`).** The reversal in step 4 of the list above is issued by the
> in-service pass (section 5) under the key `#reversal/<event_id>`, with the client's key carried
> on the row as `reconciles_key` (DR-049's note). Each property of this section is proved against
> the real code by IP-11's integration tests; the drill itself, run nightly against the Compose
> stack with the real binary, is `IP-15`'s.

### 4.2 Postgres lost — restore from backup

**Impact.** Audit history and ledger history are truncated to the backup point. Configuration is
restored. Runtime balances are **not** affected, because they live in the fast store.

**Procedure.**

1. Stop the service. Stopping is required: the service refuses to start without Postgres
   (section 3), and that refusal is a feature.
2. Restore Postgres to a new database. Never restore over a live one.
3. Start the service. Migrations are idempotent and re-run safely.
4. Force a full configuration refresh by bumping `qc:cfg:version` and publishing an invalidation.
   This closes any gap between the restored configuration and the runtime store.
5. Reconcile: for each tenant-feature, compare the runtime balance against the rebuilt ledger for
   the restored period, and report differences. Differences after a *known* truncation point are
   expected; differences outside it are an incident.
6. Record the truncation in the audit log, so the gap in history is documented rather than
   discovered later by a customer.

**What is guaranteed.** Enforcement resumes within NFR-A4. Balances are untouched, because they
were never in Postgres.

**What is not.** Events after the backup point are gone. There is no write-ahead replication to
another node, because that is a deployment topology the MVP does not include (NFR-C5, and the
exclusions in [overview.md](overview.md)).

### 4.3 The service was down for a long period

**Impact.** None on balances. This is the property the lazy-rollover design exists to provide.

**Procedure.** None. On the first request, the cycle is advanced by the transition function to the
current window. The tenant lands in the correct cycle, receives one allowance, and no multiple
reset has occurred (DR-009, FS-05).

**Verification.** Exactly one `cycle_rolled_over` event exists for that boundary, and
`cycle_index` jumped rather than incrementing one at a time.

### 4.4 A cycle rolled over incorrectly

**Symptom.** A tenant received a fresh allowance in the wrong window, or is stuck in a window that
has already ended.

**Procedure.**

1. Read `GET /v1/balance`. Note `cycle_index`, `cycle_start`, `cycle_end`.
2. Compare against the boundary table in [cycle-engine.md](cycle-engine.md#6-test-matrix) for that
   interval and zone. This is why that table exists as exact timestamps.
3. If the stored `cycle_index` is ahead of the computed one, the monotonic guard is doing its job
   and the tenant is stuck. `POST /v1/admin/tenants/{id}/force-rollover` with a reason resets it to
   the current window and writes an audit record.
4. If the stored index is behind, the next request advances it correctly. No action.
5. If the computed window is wrong, the cause is in the cycle engine, and the boundary table is
   the test that should have caught it. Add the case to the table.

### 4.5 Ledger gap detected

> **Amended 2026-10-20 (`IP-11`).** Every drop is now also recorded by tenant and feature in
> `usage_event_gaps` ([data-model.md](data-model.md#214-usage-event-gaps)), so step 1 reads the
> table rather than reconstructing the window from alert timestamps, and the reconciliation pass
> verifies each named tenant-feature on its next run. The table is a record that something is
> missing, kept outside the ledger, so step 3 still holds.

**Symptom.** `quotacore_event_sink_dropped_total` is non-zero, or a customer's history shows a
jump.

**Procedure.**

1. Record the drop window from the metric's `reason` label and the alert timestamps.
2. For each affected tenant-feature, compare the runtime balance against the sum of events in the
   window. Where they differ by exactly the missing deltas, the runtime store is authoritative and
   correct; the history is incomplete.
3. Do **not** insert synthetic events to fill the gap. A fabricated event is indistinguishable from
   a real one in every future query, and it makes the ledger untrustworthy for the customer, which
   is its primary purpose.
4. If the customer needs a complete history, export the runtime balances for the window with a
   documented, clearly-labelled procedure. Honest, derived, and marked as such.

### 4.6 A customer disputes a deduction

> **Added 2026-10-20 (`IP-11`).** The answer comes from the ledger and the audit trail, never from
> the fast store, so it is the same after a store loss.
>
> 1. `GET /v1/admin/tenants/{id}/events?idempotency_key=<key>` returns every row under the
>    client's key: the deduction, and if a store loss let a retry apply it twice, the second
>    deduction and its reversal, a `refunded` row with `source: system`.
> 2. `GET /v1/admin/tenants/{id}/events` for the cycle returns its rows. Each row's
>    `balance_after` less its `delta` is the row before's `balance_after`; the cycle opens at its
>    `cycle_rolled_over`, `plan_changed` or `force_rolled_over` row, or at the limit when none
>    opened it, and its deltas sum to the last `balance_after`.
> 3. `GET /v1/admin/audit?tenant_external_id=<id>` attributes every administrative row: each
>    `granted`, `set`, `plan_changed` and `force_rolled_over` row, and each reversal, shares its
>    `request_id` with exactly one audit row, which names the actor and the reason.
> 4. `quotacore verify-tenant <id>` confirms the tenant's history reconciles and ends at its hash.

## 5. Reconciliation

**Reconciliation is a procedure, not a process.** There is no background reconciler comparing two
stores, because such a process either finds nothing and costs a full scan forever, or finds
something and has to decide what to do about it while the system is running.

Instead, reconciliation is:

| Tool | When | Cost |
| --- | --- | --- |
| `quotacore verify-tenant <id>` | On demand, or after an incident | One tenant's events, seconds |
| `quotacore verify-all` | After a restore, or monthly | Full scan, minutes, off-peak |
| `quotacore reconcile-report` | After a restore, producing a file of differences | Full scan plus output |

`verify-tenant` asserts, per tenant and feature:

```
  balance (runtime) == balance_after (last event in the current cycle)
                    == opening + Σ(delta of every other row) in the current cycle
                    == the value in the last event
```

> **Amended 2026-10-20 (`IP-12`).** The middle line read `limit + bonus + Σ(delta)`, which counts a
> grant twice: a grant raises `bonus` (DR-020) and is itself a delta. The opening is the
> `balance_after` of the row that opened the cycle, or the effective limit when no row did
> ([data-model.md](data-model.md#27-usage-events-the-ledger)).

The four-way agreement is the check. A single comparison against the ledger would miss a case
where both the ledger and the runtime store are wrong in the same way, which is exactly the case a
partial ledger write or a replay bug produces.

A tenant failing verification is a **P1 incident**, not a dashboard item. It means either an
invariant is broken or a procedure was performed incorrectly, and both require a human.

> **Amended 2026-10-20 (`IP-11`).** Reconciliation is now also a bounded process, and the four
> commands exist. **The pass** runs in every instance every `QUOTACORE_RECONCILE_INTERVAL`, one
> instance at a time under an advisory lock. It reverses each unresolved duplicate (DR-049), then
> verifies only the tenant-features whose ledger rows arrived since its checkpoint, or that a
> recorded gap names, reading a row only once it is older than the settle period (the larger of a
> minute and ten flush intervals). A disagreement is confirmed only when a second read one settle
> period later finds the same hash and the same disagreement, so an event still in another
> instance's queue is never reported. Each confirmed finding is a `reconcile_findings` row
> ([data-model.md](data-model.md#213-reconciliation-findings-and-checkpoint)) and raises
> `quotacore_event_reconcile_mismatch_total` once. It never corrects a balance. It is bounded by
> what changed, so it is not the full scan forever this section rejects; the full scans stay on
> demand. **The commands** are local operations of the binary: `verify-tenant` and `verify-all`
> verify whole histories with the same confirmation and record what they find, `reconcile-report
> [--since]` prints the findings, and `rebuild` is section 4.1's step 4. Each prints JSON and exits
> 1 when a person must look.
>
> The four-way check above lists one comparison twice. It is read as four legs: the hash's
> balance equals the last row's `balance_after`; every row follows from the one before inside a
> cycle (the identity) and across a boundary, where an opening row's starting balance is the
> closed cycle's last `balance_after` (the chain, which is how an event dropped at the end of a
> closed cycle is found); and the hash's window is the last row's, or a later one it opened at the
> limit with nothing to record (Q-22). A tenant-feature that changed and then fell silent is found
> by its next event or by `verify-all`.

## 6. Restart and shutdown behaviour

| Event | Behaviour | Why it is safe |
| --- | --- | --- |
| `SIGTERM` | Drain for 30 s, then exit. In-flight requests complete | A script execution is atomic; there is no half-applied state to lose (NFR-A5) |
| `SIGKILL` | Immediate | Identical outcome. The process holds no authoritative state (NFR-D6) |
| Crash mid-request | The script either completed or did not | Atomicity |
| Crash during the ledger write | At most one event lost; the drop counter is not incremented, because the process died | The gap is detectable by verification, not by a counter. Stated as a known imprecision in the metric's meaning. Amended 2026-10-20 (`IP-11`): since IP-12 a crash loses up to the queue (`QUOTACORE_EVENT_QUEUE_SIZE`). Each writer keeps a `ledger_sessions` row it refreshes every five seconds with the instant before which everything it accepted is settled, and closes after a clean drain; a session left open and stale is a crash, and the rebuild treats every cycle from its settled instant to its last sight as unverified |
| Crash during an admin transaction | Postgres rolls back; the fast store was not written | Ordering: Postgres first, fast store second |
| Crash between the Postgres commit and the fast-store write in `apply-now` or a plan change | Configuration is committed with a pending re-anchor per tenant; the tenants are served their previous state until drained | Amended 2026-10-20 (`IP-10`): this said "self-correcting", and the scripts would leave such a tenant `STALE` indefinitely. The pending rows are drained by any instance, at start and on each refresh tick; the reverse order would be unrecoverable |
| Commit failure after a grant, set, force-rollover or immediate override applied its script (`IP-10`) | The balance changed and its audit row did not commit | Logged at `error` with the request id. The reverse order would audit a change that may not have happened, which is worse: an audit row must never describe something that did not occur |
| Replay of the ledger into the fast store | Not a supported operation | The fast store is authoritative at runtime; replaying into it would fabricate state. The rebuild in 4.1 is the supported path and is a distinct, audited operation |

That last row deserves emphasis. "Replaying" events into the live store is the operation most
likely to be attempted by an engineer under pressure, and it is the one operation that makes
things permanently worse. The supported reconstruction is a rebuild into an empty store, verified
before the service returns to normal.

## 7. Invariants, restated as alerts

Each of these is a condition whose *occurrence* is a defect, not a warning. They are collected in
[observability.md](observability.md).

| Condition | Meaning | Severity |
| --- | --- | --- |
| A transition returned `STALE` because the stored index was ahead | The clock moved backwards, or a bad write happened | P1 |
| `verify-tenant` reports a four-way disagreement | An invariant is broken | P1 |
| A `usage_events` insert violated the partial unique index | Either the runtime store failed to prevent a duplicate — a P0 class bug in the script — or the store was lost and a client retried inside its window. The two are told apart by whether a store-loss event falls in the same window | P0 |
| A reversal was issued for a detected duplicate | The remedy path ran. Expected after a declared store loss, a defect otherwise, and either way it means a customer was charged twice for a moment | P1, or expected during a declared loss |
| `balance > limit + bonus` observed at read time | The ceiling check is broken | P0 |
| A `cycle_index` exceeded the sanity ceiling | A bad write | P1 |
| `quotacore_balance_key_missing_total` rose | A provisioned balance hash was lost, or the store was flushed. Enforcement is fail-closed for those tenant-features | P0 |
| Ledger drops with `reason=error` sustained for over an hour | The history guarantee is not being met | P2 |
| Invalidation lag above the bound for over five minutes | Configuration is stale beyond the published contract | P2 |
| Clock skew above the threshold | Boundaries are wrong | P1 |
