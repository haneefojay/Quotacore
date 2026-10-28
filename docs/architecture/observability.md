# Observability

Metrics, logs, traces, health probes and alerts. The design rule: **every number a customer might
be asked about during an incident is a metric, not a query.** An operator should never need to
open a database to answer "is enforcement working", and never needs to guess whether a number is
trustworthy.

NFR references are in [non-functional-requirements.md](non-functional-requirements.md).

---

## 1. Metrics

Prometheus exposition on `:9090/metrics`, bound to localhost inside the container by default and
authenticateable where that is not possible. No customer data appears in any metric name, label or
value (NFR-O6).

### 1.1 Enforcement

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `quotacore_consume_total` | counter | `outcome` = `allowed` \| `denied` \| `replayed` \| `rejected` | Every `consume` call |
| `quotacore_refund_total` | counter | `outcome` as above | Every `refund` call |
| `quotacore_check_total` | counter | `outcome` = `allowed` \| `denied` | Advisory checks |
| `quotacore_consume_amount_total` | counter | `feature_key` | Units consumed. Feeds the "should this match my own logs" question |
| `quotacore_rolled_over_total` | counter | `interval` | Cycle transitions applied, by the request path or the worker |
| `quotacore_rollover_stale_total` | counter | — | Transitions refused because the stored index was ahead. **Non-zero is a clock or data defect** (P1) |
| `quotacore_balance_key_missing_total` | counter | — | A balance hash was absent and a request was failed closed with `503` rather than initialising it (DR-045). Any occurrence is a P0 |
| `quotacore_ceiling_violation_total` | counter | — | Observed `balance > limit + bonus`. **Always zero. Non-zero is P0** |

> **Amended 2026-09-29 — "had to be initialised" was never true.** This description said a missing
> hash was initialised, which contradicts [DR-045](../product/domain-rules.md): a missing balance is
> a `503`, never a full allowance handed to whoever triggered the loss. `IP-05` proved that against a
> real store, and its scripts answer `state_missing`. The increment is `IP-07`'s, which is where each
> code becomes reachable over HTTP, and the collector is `IP-13`'s. The metric is not renamed, and
> the identifier is unchanged.

`outcome` is the label to alert on. `denied` is a business outcome and is expected to scale with
usage; `rejected` is a failure and should be near zero. `rejected` means the service failed closed,
a `5xx`. A `4xx` other than `quota_exceeded` is the client's error and is counted in
`quotacore_errors_total` only, so a misconfigured key cannot fire the P0 alert below.

> **Amended 2026-10-19 (`IP-07`).** The table named `check_total`'s values but not its label, which is
> `outcome`, and did not say which failures `rejected` covers. Both are stated above. Keeping them in one counter with distinct
values is deliberate: a customer asking "why is my quota going up?" is answered by the same series.

> **Amended 2026-10-21 (`IP-13`) — what the three transition counters count.** Each is counted once
> per script execution by the script runner, never on a replay. `rolled_over_total` counts a
> transition the script applied, data plane or admin; its `interval` is the entitlement's, from the
> contract's six. A force-rollover and a re-anchor are rewrites, not transitions, and are not
> counted. v0.1 has no reset worker (`Q-13`), so every transition is the lazy one a request applies,
> and the counter rises when traffic crosses a boundary rather than at the boundary; whichever phase
> answers `Q-13` counts its worker's transitions here. `rollover_stale_total` counts a `stale`
> verdict whose stored index is ahead of the caller's target, except the one benign case: the store
> one cycle ahead, opened less than the clock-skew threshold (`Q-26`) after the caller's reading,
> which is two synchronised instances meeting at a boundary. A `stale` because the caller's snapshot
> named an older anchor, or its window had not opened, is not counted. `ceiling_violation_total`
> counts any state, from a script's reply or the balance route's read, whose balance exceeds
> limit plus bonus.

### 1.2 Errors

One counter per error code, plus a total:

| Metric | Labels |
| --- | --- |
| `quotacore_errors_total` | `code`, `endpoint` |
| `quotacore_latency_seconds` | histogram, `endpoint`, `outcome` |

The full set of `code` values is the set in [error-catalog.md](../product/error-catalog.md), and a
contract test asserts the two lists match. A code that exists in the API but not in the metrics
would be invisible during exactly the incident it was written for.

`quotacore_latency_seconds` histograms use buckets that resolve the SLOs in NFR-L1 to NFR-L4
directly, so a graph shows "p99 under 5 ms" without interpretation.

> **Amended 2026-10-21 (`IP-13`).** `quotacore_latency_seconds` covers the four data-plane
> operations. Its `outcome` is section 1.1's four values plus `refused`, a `4xx` the caller caused,
> which is neither a business outcome nor a failure of the service. `endpoint` is the contract's
> `METHOD /path` on both series.

### 1.3 Dependencies

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `quotacore_datastore_up` | gauge | — | `1` when the data store answered within the timeout |
| `quotacore_datastore_latency_seconds` | histogram | — | Round-trip time. The leading indicator of a tail-latency problem |
| `quotacore_datastore_errors_total` | counter | `class` | By class: `timeout`, `noscript`, `oom`, `refused` |
| `quotacore_datastore_memory_used_ratio` | gauge | — | Used `maxmemory` fraction. The leading indicator of FS-21, and the reason the sizing floor in [deployment.md](deployment.md#5-sizing) is a floor rather than a target |
| `quotacore_controlplane_up` | gauge | — | Postgres health. Its absence must not affect the data plane |
| `quotacore_dbpool_wait_seconds` | histogram | — | **Pool wait time.** A rising value is the mechanism by which a control-plane problem would reach the data plane, and it is the thing to watch precisely because that coupling is supposed to be impossible |
| `quotacore_script_loads_total` | counter | `command` | `evalsha` \| `eval`. A `noscript` storm means a restart loop or a flush; the script is then re-registered, which is correct but slow |
| `quotacore_script_cache_hits_total` | counter | — | Ratio must stay above 0.99 |
| `quotacore_clock_skew_seconds` | gauge | — | This process's clock minus the data store's, corrected for half the round trip. Added 2026-10-21 (`IP-13`), because a clock-skew alert is required (section 5, A-17, NFR-D8) and nothing measured it |

> **Amended 2026-10-21 (`IP-13`).** The `Labels` column is new, so a test reads every label from
> one place. Every data-store command is timed and classified by a hook on the store's client; a
> read-only refusal never reaches the store and is not an error here. `datastore_errors_total` has a
> fifth class, `other`, for an error none of the four describes, so no store error goes uncounted;
> `refused` is a connection the store, or anything in front of it, refused or closed.
> `datastore_up`, `clock_skew_seconds` and the readiness term (section 4) come from one probe, a
> `TIME` every second within `QUOTACORE_DATASCRIPT_TIMEOUT`; `datastore_memory_used_ratio` from
> `INFO memory` every ten seconds, and it is `0` when `maxmemory` is unset, which logs a `warn` at
> start; `controlplane_up` from a Postgres ping every ten seconds. `script_loads_total`'s label is
> `command`: `evalsha` for a script run by digest, `eval` for a script body sent to the store, which
> the runner does only with `SCRIPT LOAD` at start and after `NOSCRIPT`; it never sends `EVAL`.
> `script_cache_hits_total` counts the digests the store found, so the ratio is hits over `evalsha`.

### 1.4 Configuration cache

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `quotacore_config_cache_entries` | gauge | — | Current size. Eviction is a signal the bound is too small |
| `quotacore_config_cache_evictions_total` | counter | — | Any sustained non-zero rate means NFR-T7 is undersized |
| `quotacore_config_cache_hit_ratio` | gauge | — | Should be above 0.99 in steady state |
| `quotacore_config_cache_miss_total` | counter | `result` | By result: `loaded`, `unknown`, `refused`, `error`. **The leading indicator of a control-plane outage reaching the data plane** (J-6) |
| `quotacore_invalidation_lag_seconds` | histogram | — | Time from a committed change to the last subscriber observing it. This is the measurement behind NFR-D3 |
| `quotacore_controlplane_refresh_total` | counter | `result` | By result: `ok`, `error`, `skipped` |
| `quotacore_reanchors_pending` | gauge | — | Re-anchors committed in Postgres and not yet applied to the fast store, as of the last drain tick. Added 2026-10-21 (`IP-13`), the gauge `IP-10` handed on. A re-anchor stays pending only while the store cannot take the rewrite |

### 1.5 Ledger

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `quotacore_event_enqueued_total` | counter | — | Accepted onto the bounded queue |
| `quotacore_event_written_total` | counter | — | Rows actually appended |
| `quotacore_event_sink_dropped_total` | counter | `reason` | **By `reason`: `queue_full`, `error`, `shutdown`.** Non-zero is a visible gap in history (FS-10) |
| `quotacore_event_queue_depth` | gauge | — | Current depth. Sustained growth is a P2 |
| `quotacore_event_write_latency_seconds` | histogram | — | Batch write time |
| `quotacore_event_duplicate_detected_total` | counter | — | The partial unique index fired. Zero in normal operation. Non-zero is P0: either the runtime store failed to prevent a double charge (DR-030), or a total store loss lost the idempotency record and a client retried inside its window (FS-22). The two are told apart by whether a store-loss event is in the same window |
| `quotacore_duplicate_reversal_total` | counter | — | Refunds issued by reconciliation for a detected duplicate (DR-049). Non-zero means a customer was charged twice for a moment. Idempotent on `event_id`, so a second reconciliation pass must not increment it |
| `quotacore_event_reconcile_mismatch_total` | counter | — | Verification disagreements. Non-zero is P1 |
| `quotacore_event_dead_lettered_total` | counter | `reason` | **By `reason`: `duplicate_idempotency_key`, `rejected`.** Events the ledger refused and the writer kept in the dead-letter table rather than losing (INV-U3). Added 2026-10-20 (`IP-12`) |
| `quotacore_event_dead_letters` | gauge | `reason` | **By `reason`.** Dead-letter rows outstanding, refreshed on each retention tick. A `rejected` row that stays is one whose replay keeps failing. Added 2026-10-20 (`IP-12`) |

> **Amended 2026-10-20 (`IP-11`).** Both reconciliation counters are now emitted, by the
> in-service pass. `duplicate_reversal_total` rises once per reversal committed, never on a replay
> of one, under the refund key `#reversal/<event_id>` (DR-049's note). `event_reconcile_mismatch_total`
> rises once per confirmed disagreement between the hash and the ledger, of the kinds `identity`,
> `chain`, `hash_balance` and `hash_cycle`, when its `reconcile_findings` row is first inserted, so
> a disagreement found again by a later pass or by `verify-all` is not counted twice. It has no
> label: the kind, the tenant and the feature are in the finding row and the `error` log line,
> because they are query material and never labels (section 1.7). An absent hash is a
> `hash_missing` finding and not a mismatch; `balance_key_missing_total` is its alert.

`event_sink_dropped_total` and `event_duplicate_detected_total` are the two metrics that mean
money. Both are P0-adjacent, both are asserted zero in tests, and both are described in the
operator runbook with the recovery procedure rather than with a suggestion to investigate.

### 1.6 HTTP and process

`quotacore_http_requests_total{endpoint,method,status}`,
`quotacore_http_duration_seconds{endpoint}`, `quotacore_rate_limited_total{key_prefix}`,
`quotacore_build_info{version,commit,zone_data_version}`,
`quotacore_goroutines`, `quotacore_memory_bytes`, `quotacore_gc_pause_seconds`,
`quotacore_uptime_seconds`.

> **Amended 2026-10-21 (`IP-13`).** These are served under the names above, read from the runtime
> on each scrape; the client library's `go_*` and `process_*` collectors are not registered, because
> this section names its own. `http_requests_total` and `http_duration_seconds` count every request,
> the probes and the unmatched path included; `endpoint` is the contract's operation or `unmatched`,
> and `method` is the matched route's method or `other`, because a raw method is text the caller
> chooses. `gc_pause_seconds` is a histogram whose sum is estimated from the runtime's bucket
> midpoints, since the runtime keeps no sum. `build_info` is `1`, and its `zone_data_version` is
> the toolchain release whose `time/tzdata` the binary embeds, as the status route reports.

### 1.7 Cardinality rules

| Rule | Reason |
| --- | --- |
| No `tenant_id` label on the data plane | 50,000 tenants is 50,000 series per counter. Unbounded label values are the fastest way to destroy a Prometheus |
| `feature_key` as a label is allowed and bounded by NFR-T5 | 100 features is 100 series, which is fine and genuinely useful |
| `key_prefix` is allowed on auth and rate-limit metrics | Bounded by the number of keys, and it is how a leaked key is identified in an alert. A revoked key's series stays until the process restarts, so the bound is the keys ever issued (amended 2026-10-21, `IP-13`) |
| `code` as a label is allowed and bounded | It is the error set, not a free-text value |
| No `request_id`, no `external_id`, no `metadata` in any label | They are unbounded and they are customer data |

Per-tenant observability is a **query**, not a label: `GET /v1/admin/tenants/{id}/events` and the
CLI's `verify-tenant`. That is the right tool for it.

## 2. Structured logs

One JSON object per line to stdout, collected by the customer's platform. Fields:

```json
{
  "ts": "2026-09-27T14:23:11.482Z",
  "level": "info",
  "msg": "consume",
  "request_id": "01JCQ7Z4K2M9XQ3F8V6T0B5NDE",
  "endpoint": "POST /v1/consume",
  "status": 200,
  "duration_ms": 1.42,
  "outcome": "allowed",
  "replayed": false,
  "rolled_over": false,
  "amount": 4200,
  "balance": 41250,
  "limit": 1000000,
  "interval": "monthly",
  "cycle_index": 3
}
```

| Field | Always | Notes |
| --- | --- | --- |
| `ts`, `level`, `msg` | yes | RFC 3339, UTC, microsecond precision |
| `request_id` | yes on every request line | The join key across logs, events and customer reports (NFR-S10) |
| `endpoint`, `status`, `duration_ms` | yes on every request line | — |
| `outcome` | yes on the data plane | `allowed`, `denied`, `replayed`, `rejected` |
| `code` | on every non-2xx | The error code from the envelope |
| `amount`, `balance`, `limit`, `interval`, `cycle_index` | on data-plane lines | The numbers a support conversation needs |
| `actor`, `action`, `subject_type` | on every admin line | From the audit record |
| `duration_ms` on state changes | yes | Rollover, apply-now, rebuild |
| `error` | when applicable | A short string. Never a stack trace in a request log |

> **Amended 2026-10-21 (`IP-13`).** Every line a request causes carries that request's
> `request_id`; a line no request caused (the ledger writer, retention, the reconciliation pass, the
> re-anchor drain, the key-cache refresh, the fence, startup and shutdown) carries a `job` field
> naming its source instead, and no `request_id`, which is how NFR-S10 is read. `ts` is written in
> UTC with microseconds and `level` in lowercase by the one logger constructor every path uses, and
> the process writes JSON from its first line, before its configuration is read, so a refusal to
> start is JSON too. `actor` is the admin key's prefix; `action` and `subject_type` are the audit
> record's. The example's `reconcile_key` is removed: no row defines it, and the nearest candidate,
> the client's `Idempotency-Key`, is client-chosen text that stays out of logs.
> `QUOTACORE_LOG_FORMAT=text` remains for a person at a terminal; these rules are proved for the
> default, `json`.

### 2.1 What is never logged

| Never | Why |
| --- | --- |
| A secret, in any form, including a truncated one | A prefix is the identifier; the rest is the credential (DR-043) |
| `tenant.external_id`, or a tenant display name | Customer data (NFR-O6). `tenant_id` is a UUID, which is an identifier and not PII, and is logged on admin lines only |
| `metadata` content, or its keys | Free text from a customer, and it can contain anything |
| A request or response body | Same reason, plus volume |
| An `Authorization` header value | Obvious, and it is asserted by a test |
| A SQL statement with bound values | Statements are logged at debug; values are not |

### 2.2 Log levels in use

| Level | Used for | Examples |
| --- | --- | --- |
| `error` | Something an operator must act on | Script failure, ledger write error, migration failure, `verify-tenant` mismatch |
| `warn` | Degradation that is expected to be self-correcting | Cache miss burst, control-plane refresh failure, a `noscript` reload, clock skew detected, plaintext data-store connection |
| `info` | The request and state-change lines | Every request, every rollover, every admin mutation |
| `debug` | Per-dependency timings, refresh details | Off in production |
| Bootstrap key | `warn`, once, with the prefix only | Because a credential appearing in a log is a real event, and suppressing the log line would be worse (INV-K2) |

## 3. Traces

Optional, OpenTelemetry, sampled. The `request_id` is the correlation key in every case, and the
implementation must work with tracing disabled — it is a debugging aid, not a dependency.

Spans on the data plane, in order: `http.request` → `auth.verify` → `config.resolve` →
`cycle.compute` → `datastore.evalsha` → `respond`. A span is created only where there is a
measurement worth having; a span per validation branch is noise.

Sampling is head-based at a low rate by default, with **always-sample on any non-2xx and on any
`rolled_over: true`**. The first is obvious. The second matters more than it looks: a rollover is
the one event where the cycle engine, the clock and the script all participated, and it is exactly
the event an operator will need to explain afterwards.

## 4. Health and status

| Endpoint | Auth | Semantics | Used by |
| --- | --- | --- | --- |
| `GET /healthz` | none | The process is alive and not shutting down. **No dependency checks.** A liveness probe that fails on a database outage causes a restart loop during an outage | Orchestrator liveness |
| `GET /readyz` | none | The process can serve enforcement: the data store answered within the timeout, the snapshot is populated, migrations are current, and the process is not draining. No detail in the body | Load balancer, compose healthcheck |
| `GET /v1/admin/status` | `admin` | Dependency health, version, commit, zone-data version, server time, cache size, the ledger queue's depth, the ledger writer's state, and the ledger-drop counter, each for the instance that answers (amended 2026-10-20, `IP-12`) | Operator and support |

> **Amended 2026-10-21 (`IP-13`).** `/healthz` answers `200 {"status":"ok"}` from start until
> the process exits, draining included, as the contract says: a liveness probe that failed during a
> graceful drain would have the process killed mid-drain. `/readyz` answers `200
> {"status":"ready"}` while every term holds and a bare `503` otherwise. Its terms are: the data
> store answered the prober's last ping (every second) within `QUOTACORE_DATASCRIPT_TIMEOUT`; the
> snapshot has completed its first load, whatever the tenant count, so a fresh install is ready
> before its first tenant; migrations are current; the process is not draining; and it is in
> `normal` mode, so an instance started read-only, or one whose fence is lost or held by a rebuild,
> is not ready. A Postgres outage is deliberately not a term: cached tenants still enforce, and
> making every instance unready at once would turn a P2 into an outage (NFR-A2).
>
> **Amended 2026-10-27 (`IP-15`).** One more term: the data store reports `maxmemory_policy`
> `noeviction` in `INFO memory`, read at start (where any other value refuses to start, DR-048) and
> on every slow probe after. A policy changed under a running instance makes it unready as `data
> store refuses to evict`, logs the old and new policy at `error`, and logs again at `info` when it
> is restored. The image's healthcheck, `quotacore healthcheck`, is a client of `/readyz` and
> nothing more, so the Compose healthcheck and a load balancer see the same answer.

**Liveness and readiness are deliberately different, and liveness deliberately ignores
dependencies.** Conflating them is the classic way an infrastructure outage becomes a crash loop
that makes the outage worse. The reasoning is worth stating because it looks wrong until it is
explained: restarting a process whose database is unreachable achieves nothing, destroys the warm
snapshot cache, and turns a degraded system into a cold one.

`/readyz` returning false does not stop the process from serving cached tenants. It stops the load
balancer sending *new* traffic, which is the correct behaviour when the process genuinely cannot
enforce.

## 5. Alerts

Severity definitions: **P0** money or data is wrong now. **P1** a stated guarantee is broken.
**P2** degraded, self-correcting or bounded. **P3** needs attention this week.

| Alert | Condition | Sev | First response |
| --- | --- | --- | --- |
| Ceiling violated | `quotacore_ceiling_violation_total > 0` | P0 | Halt. The invariant in DR-019 is broken. Read [consistency-and-recovery.md](consistency-and-recovery.md) §4.4 |
| Double charge detected | `quotacore_event_duplicate_detected_total > 0` | P0 | Halt. Either the idempotency guarantee in DR-030 is broken, or a store loss lost the record. Check for a store-loss event first: if there was one, the reversal in DR-049 is the remedy and this is an incident, not a script bug. If there was not, the script is at fault — rotate runtime keys and find the window of exposure |
| Duplicate reversal issued | `quotacore_duplicate_reversal_total > 0` | P1 | Expected during a declared store loss, a defect otherwise. Confirm each reversal has a matching `event_id` and that a second reconciliation pass issued nothing further |
| Data store memory approaching `maxmemory` | `quotacore_datastore_memory_used_ratio > 0.8` for 10 min | P2 | The store runs `noeviction` (DR-048), so the ceiling is a `503` for every tenant, not an eviction. Add memory before the ratio reaches 1.0; do not raise the limit to make the alert go away |
| Balance hash missing | `quotacore_balance_key_missing_total` above zero | P0 | The store lost a provisioned balance. Run the rebuild in §4.1 of the recovery runbook; do not restart in normal mode |
| Enforcement failing closed | `outcome="rejected"` above 1% for 2 min | P0 | The customer is being denied service. Check datastore health, then the customer's own error handling |
| Reconciliation mismatch | `quotacore_event_reconcile_mismatch_total > 0` | P1 | Run `verify-tenant`. A human decides; do not auto-correct |
| Stale rollover refusal | `quotacore_rollover_stale_total` rising | P1 | Check clock synchronisation. The monotonic guard is working; the clock is wrong |
| Data store unreachable | `quotacore_datastore_up == 0` for 60 s | P1 | Expected to fail closed. Confirm no partial deductions, then wait |
| p99 latency breach | `latency{p99} > 5 ms` for 5 min | P2 | Check `datastore_latency`, then `script_cache_hits`, then GC |
| Control plane down | `quotacore_controlplane_up == 0` for 60 s | P2 | Watch `config_cache_miss_total`. Cached tenants are unaffected; this becomes P1 if the miss rate also rises |
| DB pool wait | `quotacore_dbpool_wait_seconds{p99} > 0.05` | P2 | The coupling the architecture forbids is forming. Find which admin query is holding connections |
| Ledger drops | `event_sink_dropped_total` rising for over 1 min | P2 | History has gaps. Enforcement is unaffected. See FS-10 before doing anything |
| Ledger queue depth | `quotacore_event_queue_depth > 80% of cap` sustained | P2 | The writer is behind. Expect drops if it continues |
| Ledger dead letter, duplicate | `quotacore_event_dead_letters{reason="duplicate_idempotency_key"} > 0` | P0 | The same condition as *Double charge detected*, seen from the table that holds the second deduction for reconciliation's reversal (DR-049). Follow that row |
| Ledger dead letter, rejected | `quotacore_event_dead_letters{reason="rejected"} > 0` for over 1 h | P2 | An event the ledger refuses and whose hourly replay still fails. The `error` log line carries its `event_id` and the refusal; fix the cause and the next tick appends it |
| Config invalidation lag | `invalidation_lag{p99} > 1 s` for 5 min | P2 | Pub/sub is degraded; the refresh interval is the backstop. NFR-D3 is not being met |
| Cache eviction | `quotacore_config_cache_evictions_total` rising | P2 | NFR-T7 is undersized. Raise the bound or add memory |
| Clock skew | Skew above the threshold | P1 | Boundaries are wrong. Fix NTP |
| Audit write failure | Any audit insert error | P1 | The audit guarantee in NFR-D4 is broken. Investigate immediately |
| No traffic | `quotacore_http_requests_total` flat for 15 min | P3 | Probably correct. Check the customer's integration before assuming a fault |
| Re-anchors pending | `quotacore_reanchors_pending > 0` for 10 min | P2 | A committed re-anchor has not reached the data store, which happens only while the store cannot take the rewrite. Follow *Long outage*. Added 2026-10-21 (`IP-13`) |

> **Amended 2026-10-21 (`IP-13`).** Every row is a Prometheus rule in
> [`ops/alerts/quotacore.rules.yml`](../../ops/alerts/quotacore.rules.yml), named by the row's words
> run together (*Ceiling violated* is `CeilingViolated`), labelled with its `severity`, and, for
> every P0 and P1, annotated with its `runbook` in [`docs/runbooks/`](../runbooks/README.md). A test
> holds the file to this table and CI runs `promtool check rules` and `promtool test rules` on it, so
> a rule that cannot fire fails the build. Two rows had no metric of their own. *Clock skew* reads
> `quotacore_clock_skew_seconds` (section 1.3) against `Q-26`'s threshold of one second, for five
> minutes. *Audit write failure* reads `quotacore_errors_total{code="internal_error"}` on an admin
> endpoint, because an audit row commits in its mutation's transaction, so a failed insert is that
> write answered `500`. *Enforcement failing closed* is the share of `consume` and `refund` calls
> whose outcome is `rejected`; *Ledger queue depth* uses 80% of the default cap, 10,000, and moves
> with `QUOTACORE_EVENT_QUEUE_SIZE`. *Stale rollover refusal* is any increase over five minutes, and
> its runbook says it can lag the clock by a tenant's idle time, because a transition happens on the
> first request after a boundary.

> **Amended 2026-10-20 (`IP-11`).** *Reconciliation mismatch*: `quotacore reconcile-report` prints
> what the pass found, with its kind, tenant and feature, and `verify-tenant` reads that tenant's
> whole history; nothing is corrected automatically. The pass verifies only what changed, so an
> event dropped on a tenant-feature that then falls silent is found by its next event or by
> `verify-all`, not by the next pass. *Duplicate reversal issued*: a reversal the ceiling refused,
> and a duplicate refund, are not reversed; each is a `reconcile_findings` row (`reversal_refused`,
> `duplicate_refund`) for a person, and its dead letter is resolved `referred`.

**Two alert-design rules.** First, no alert fires on a business outcome: a spike in
`outcome="denied"` is a customer's usage pattern, not our problem, and alerting on it trains
operators to ignore the series that matters. Second, every P0 and P1 has a linked runbook, because
an alert without a procedure is a notification that generates anxiety instead of an action.

## 6. Operator runbooks

| Runbook | Trigger |
| --- | --- |
| Enforcement is failing closed | `outcome="rejected"` alert, or a customer report of `503` |
| Data store lost | `quotacore_balance_key_missing_total` after a restore |
| Postgres restore | Any restore from backup |
| Long outage | Any outage over one cycle interval |
| A cycle rolled over incorrectly | A customer reports the wrong reset time |
| A leaked key | Any suspected credential exposure |
| An incorrect cycle was shipped | A `rollover_stale` alert with an identified cause |

> **Amended 2026-10-21 (`IP-13`).** The runbooks are in [`docs/runbooks/`](../runbooks/README.md), one
> file per runbook, and they link to the sections below rather than copy them, because a rule is
> stated once. Each holds only what an operator does: the trigger, the impact, the checks as
> commands, the decision, the action, how to verify it, when to escalate, and the sources. Four
> more cover the P0 and P1 alerts the table above did not: *Double charge*, *Reconciliation
> mismatch*, *Ceiling violated* and *Audit write failure*; *An incorrect cycle was shipped* and the
> clock-skew alert share *Stale rollover or clock skew*. The paragraph below describes the copies
> this note replaces.

Each runbook is the relevant section of [consistency-and-recovery.md](consistency-and-recovery.md)
or [security-model.md](security-model.md) §9, copied into `docs/runbooks/` with the check commands
filled in. The two files are the source of truth; the runbook copies exist so an operator does not
have to know which document to open at 03:00.

## 7. What is deliberately not observed

| Not observed | Reason |
| --- | --- |
| Per-request tracing by default | Cost and volume. Sampled, with always-on for errors and rollovers |
| A dashboard in this repository | The metrics are the contract; the dashboard belongs to the customer's platform, and a dashboard that only we can read would make the customer dependent on us for their own service |
| Anomaly detection on business metrics | A sudden drop in consumption is a customer's success, not our regression. Alerting on it would be actively harmful |
| Client-side SDK telemetry | The SDKs are libraries in someone else's process. They send nothing (NFR-S3) |
| A self-hosted usage counter for the project | See the foundation. Usage data about the operator is not something this product should collect, and the one place it would matter is the one place it would be least trusted |
