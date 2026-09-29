# Request Lifecycle

The enforcement path, step by step, with the work each step performs and the work each step is
forbidden to perform. This is the document a reviewer reads to check that a change has not put
something blocking on the critical path.

Latency targets referenced here are in [non-functional-requirements.md](non-functional-requirements.md).

---

## 1. The path, annotated

`POST /v1/consume` with `Authorization: Bearer …`, `Idempotency-Key: …`.

### Step 1 — Accept  · target ≤ 50 µs

Read the body with a hard cap of 4 KiB for `metadata` and 64 KiB overall (NFR-T6). Read the
headers. Adopt an inbound `X-Request-Id` if it is a valid ULID or UUID, otherwise mint one
(NFR-S10). Reject an over-cap body with `413 payload_too_large` before parsing it.

**Forbidden:** any lookup, any parsing that can be deferred, any allocation proportional to
request volume.

### Step 2 — Authenticate  · target ≤ 100 µs

Extract the bearer token, take its public prefix, and look up the key in the **bounded in-process
key cache** by prefix. Verify the Argon2id hash in constant time. Update `last_used_at`
asynchronously, off the path.

**Why the hash is verified per request and not just at cache fill.** A cached *verified* key is
what a 24-hour key TTL, a revoked key, or a copied key would defeat. Verification is the
mechanism; the cache only avoids the database. Argon2id is deliberately expensive, which is
precisely why the cache is bounded and eviction is a metric.

**Forbidden:** a Postgres query to re-read the key on the request path. This is the single most
common way the two-plane boundary is eroded, by a well-meaning "let me look up the key's scope
fresh" change.

**Failure:** `401 unauthorized`, `401 api_key_invalid`. Both are fail-closed, and both are
indistinguishable in timing from each other.

### Step 3 — Authorise  · target ≈ 0

The `runtime` scope is required. The data plane requires exactly one scope, so this is an
integer comparison.

**Failure:** `403 insufficient_scope`.

### Step 4 — Validate  · target ≤ 20 µs

Decoded-and-validated values, not raw strings: `feature_key` against the format rule, `amount` as a
non-negative `int64` (DR-022, DR-023), `metadata` as a bounded object, and the presence of
`Idempotency-Key` (DR-026).

`window` is validated against the feature's cadence: rejected in the MVP (ADR-0008), and in v0.2
rejected if it disagrees with the configured cadence, because a silent mix would make the reported
balance a lie.

**Forbidden:** consulting configuration to validate. The format checks are pure.

**Failure:** `400 validation_failed`, `400 invalid_amount`, `400 invalid_window`,
`400 missing_idempotency_key`. All aggregate, naming every offending field.

### Step 5 — Resolve configuration  · target ≤ 20 µs

A single lookup in the bounded in-process **snapshot**, keyed by tenant, returning the effective
entitlement for the feature: limit, interval, plan, tenant state, feature state, time zone,
anchor.

**This step performs no I/O on a cache hit, which is every tenant the instance has served before.
It is the reason the design has two planes.**

| Snapshot miss | Action |
| --- | --- |
| Tenant present in Postgres, absent in snapshot | Refresh the entry, then proceed. Counted as a refresh, not an error |
| Tenant absent in Postgres too | `404 tenant_not_found` (DR-038) |
| Postgres unreachable, tenant not cached | `503 control_plane_unavailable` (DR-039) |

A refresh triggered by a miss is a synchronous read, and it is bounded: a miss storm is rate-limited
internally so a cold cache cannot turn into a database load amplifier. This is the one place where
the data plane may touch the control plane, it happens only on a miss, and it is bounded by DR-039.

**Forbidden:** reading configuration per request, inlining a per-tenant database query "just for
this one feature", or resolving a time zone from the host's zone database.

### Step 6 — Gate  · target ≈ 0

The fixed resolution order from
[state-machines.md](../product/state-machines.md#5-tenant-entitlement-the-runtime-binding), in
order: feature archived → tenant suspended → feature not in plan → override → plan value. Then
derive the target window with the cycle engine.

**Order matters and is not negotiable.** Archived before suspended, so an archived feature reports
the archival regardless of tenant state; plan membership before override, so an override can never
grant a feature the plan excludes (INV-TE1, DR-015).

**Failure:** `403 feature_archived`, `403 tenant_suspended`, `403 feature_not_in_plan`. These are
decisions, not failures: nothing is written, nothing is counted as an error in a way that implies a
defect.

### Step 7 — The atomic script  · target ≤ 1.5 ms

One `EVALSHA`, passing tenant, feature, key, amount, fingerprint, and the Go-computed
`target_index`, `window_start`, `window_end`, `limit`, `bonus`, and `now_ms`. The script performs,
in this order and without yielding:

1. **Idempotency check.** `GET` the record. Match → return the stored response with
   `replayed: true`. Mismatch → `409 idempotency_key_reuse`. This precedes everything else so a
   replay is a pure read and cannot be affected by a rollover that happened in between
   (INV-I1, DR-027, DR-028).
2. **Require the provisioned state.** An absent balance hash is a fault, not an initialisation
   opportunity: the script returns a state-missing marker, the service answers
   `503 service_unavailable`, increments `quotacore_balance_key_missing_total`, and the
   tenant-feature joins the rebuild queue. Balances are materialised at provisioning and at
   plan-assignment time, and at no other moment (DR-045, NFR-D7).
3. **Cycle transition.** The four-step transition in
   [cycle-engine.md](cycle-engine.md#5-the-transition), including the monotonicity guard, the reset
   to `limit`, `bonus = 0`, and the `EXPIREAT` at `window_end + 24h`.
4. **Denial check.** `balance < amount` → return a denial carrying the balance, limit, bonus and
   cycle end. No write, no event (DR-025, DR-017).
5. **Deduct.** `balance -= amount`, using `HINCRBY` so the arithmetic cannot overflow silently.
6. **Store the idempotency record.** `SET … EX 86400`, in the same execution.
7. **Return.** The new balance, the effective limit, the bonus, the cycle window, the cycle index,
   and whether a rollover occurred in this call.

**Everything that must be atomic is inside this one execution.** Splitting it into a read, then a
conditional write, then a record is what produces double charges, and the design exists to make
that split unrepresentable.

**Failure:** `503 service_unavailable` on a script error, with no state change, and a hard
timeout of 250 ms (NFR-L4).

### Step 8 — Respond  · target ≤ 100 µs

Write the response, including `X-Request-Id`. For a `consume`, this is a `200` with `balance`,
`limit`, `bonus`, `cycle_end`, `cycle_start`, `replayed: false`. For every error, the envelope in
[error-catalog.md](../product/error-catalog.md).

**The response is written before the ledger append is attempted.** See step 9.

### Step 9 — Enqueue the ledger append  · off-path

A non-blocking send on a bounded channel. On success, a batched writer appends one `usage_events`
row. On a full queue or a write error, the event is dropped, `quotacore_event_sink_dropped_total`
increments with the reason, and **the request is not failed**.

**Why failing here would be worse than dropping.** The deduction has already been applied
atomically. Failing the response tells the client the operation did not happen, so the client
retries, and the client is charged twice. A dropped ledger row is a visible, counted, recoverable
gap; a double charge is neither (FS-10, A-16).

**Forbidden:** waiting for the write, making the write part of the response, or silently
swallowing the failure without a counter.

### Step 10 — Observe  · off-path

Emit the metrics, one structured log line, and an optional trace span. Cardinality rules in
[observability.md](observability.md) apply here; in particular, a tenant identifier is a label
only where the label set is bounded.

---

## 2. Variations by endpoint

| Endpoint | Differences |
| --- | --- |
| `POST /v1/refund` | Same path. The script reverses direction, adds the ceiling check `balance + amount <= limit + bonus` (DR-019), and can return `409 refund_exceeds_grant`. No `quota_exceeded` is possible on this path |
| `POST /v1/check` | Steps 1 to 6 identical. The script performs the transition (so the reported window is current) and the denial check, then **stops**. No deduction, no idempotency record, no ledger append. A denial is reported as `allowed: false` in a `200`, not as a `429` (DR-024) |
| `GET /v1/balance` | Steps 1 to 6, then a single `HMGET` of the balance hash. No script, no mutation, no rollover. A tenant in a cycle that has ended reports the **stored** window until a write or a `check` advances it; the response therefore always includes the `limit` that *will* apply, and the docs state that the window is refreshed by any write or `check` |
| Admin endpoints | No snapshot, no script. A Postgres transaction, an audit row in the same transaction, then an invalidation publish (NFR-D3) |
| `POST /v1/admin/plans/{id}/apply` | The count of assigned tenants is taken **before** any write, from the same transaction that would perform the re-anchor. Above the threshold the transaction writes the audit row for the refusal and nothing else, and returns `409 confirmation_required` with the token (DR-046). The confirming call repeats the count, re-checks the plan's `resource_version`, consumes the token and then re-anchors. Postgres unreachable means no count and no token, so the call is `503 control_plane_unavailable` and the operator retries; there is no partial application to reason about |
| `GET /v1/admin/plans/{id}/impact` | A single indexed count plus a `MIN` over the assigned tenants' boundaries. No write, no audit row, no token, and no invalidation publish, because nothing changed. It is a read of the same two numbers the `409` path computes, which is why the two cannot disagree |
| `DELETE /v1/admin/tenants/{id}` | The soft-delete commit is the ordering point for concurrent requests (DR-047). A `consume` that entered the script before the commit completes and its deduction stands; one that resolves the tenant afterwards is denied. The delete itself is a Postgres transaction like any other admin mutation, so the same unreachable-Postgres behaviour applies |
| `/healthz`, `/readyz` | No auth, no dependency detail in the body, no I/O beyond the in-process state |

## 3. The snapshot, in detail

Because step 5 is the only place the data plane can be slow, it gets the most attention.

**Contents.** A bounded map of `tenant_id → { plan, state, timezone, anchor, entitlements[] }`,
where each entitlement is `{ feature_key, limit, interval, override? }`. Plans are interned
separately, so ten thousand tenants on three plans store three plan objects, not thirty thousand.

**Population.** Populated by a periodic refresh (default 30 s) and by an invalidation message
(§3 of [data-model.md](data-model.md#34-invalidation-channel)). A miss triggers a synchronous
single-tenant fetch, rate-limited.

**Bound.** A configured maximum entry count with LRU eviction, default sized from measured memory
per entry (NFR-T7). Eviction is a metric, because an eviction rate above zero on a steady-state
workload means the bound is too small and the propagation delay is worse than it looks.

**Freshness contract.** Configuration becomes visible within the documented propagation bound
(NFR-D3, Q-08). The bound is measured and published, not asserted. Crucially, staleness can only
cause a **stale limit**, never a wrong boundary, because boundaries are computed from the anchor
inside the script at request time (INV-X8).

**What is deliberately not cached.** Balances. They are always read from the store, because a
cached balance is a wrong answer, and the entire point of the strong-consistency claim is that it
is never served from memory.

## 4. Configuration propagation, end to end

```
  admin request
      │
      ▼
  transaction: mutate + audit row          ← NFR-D4, same transaction
      │  commit
      ▼
  PUBLISH qc:cfg:invalidate {version}      ← after commit, best-effort
      │
      ▼
  subscriber: bump local version
      │
      ▼
  next request, or the 30 s tick: refetch the changed tenants
```

Three deliberate properties:

1. **Publish after commit, never inside the transaction.** Publishing inside would let a
   subscriber refresh and read a configuration that then rolls back.
2. **The version is a single integer.** A subscriber that misses a message still converges on the
   next tick, so a lost publish degrades latency, never correctness.
3. **The publish is best-effort and the tick is the backstop.** A dead pub/sub channel is a slower
   system, not an incorrect one.

For a **plan edit**, propagation carries the plan; tenants on that plan are re-resolved lazily, so
no per-tenant work is needed. For a **plan assignment or an override**, the affected tenant is
refetched directly.

## 5. Concurrency and the shapes it takes

| Shape | What happens | Why it is correct |
| --- | --- | --- |
| Two requests, one balance | One succeeds, one is denied with the post-deduction balance | One script execution (NFR-T3) |
| The same key, 50 times at once | One deduction; 49 replays, each answering the recorded state with `replayed: true` | The record is written in the same execution as the mutation (INV-I1) |
| A request and the worker at a boundary | Both compute the same target; one applies, the other is `STALE` | Monotonic transition, one implementation (INV-C2) |
| An admin apply-now and a `consume` | The `consume` sees the pre- or post-change limit, never a partial one | The limit is one field read in one script execution |
| A suspension and a `consume` | The `consume` may complete if it entered the script first | Suspension is not retroactive; both actions are audited |
| A delete and a `consume` | Same rule, documented as Q-16 | An in-flight operation is not cancelled, because cancelling mid-script is not possible |
| 50,000 tenants, one hot tenant | The hot tenant is unaffected by its neighbours | Per-tenant keys, per-tenant hash tags (ADR-0002) |

## 6. What must never appear on this path

The list a reviewer checks for, each with the failure it causes:

| Forbidden | Failure it causes |
| --- | --- |
| A Postgres query | A control-plane outage stops enforcement. The two-plane split is void (DR-039) |
| A non-cached balance read | Served stale numbers, and the strong-consistency claim is false |
| A second round trip before the script | The atomicity argument no longer holds; the window between check and deduct is the bug |
| An unbounded loop or `KEYS` in Lua | Tail latency grows with tenant count, and NFR-L2 is unreachable |
| A wait for the ledger write | Response latency becomes the database's latency, and a saturated ledger stops enforcement |
| A blocking send on a full queue | Backpressure from history becomes backpressure from enforcement |
| A `time.Now()` read more than once | Two reads can straddle a boundary, so the reported window disagrees with the one that was enforced |
| String-concatenated keys from unvalidated input | Key injection, and a hash-tag split that silently breaks atomicity |
| Any `fmt.Sprintf` inside a hot loop | Allocation churn; a p99 problem that looks like a hardware problem |
| A panic that escapes to the HTTP layer mid-script | A dropped connection looks to the client like a timeout, and the client will retry |

That last row is why the script is evaluated through a `redis.NewScript` wrapper that converts
script errors into `503`, and why the HTTP server has a recovery middleware whose only job is to
turn a panic into an envelope with a `request_id` and a `500` that changed nothing.
