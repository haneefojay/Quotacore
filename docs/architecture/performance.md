# Performance

Where the time goes on the enforcement path, what the budget is, and the specific ways this
design becomes slow. Written as an engineering document rather than a benchmark report, because
the useful content is the list of things that will make it slow and how to recognise each one.

Targets are stated in [non-functional-requirements.md](non-functional-requirements.md). This
document explains how to hit them and how to tell when they have been missed.

---

## 1. The budget

A `consume` that crosses a cycle boundary, in the reference deployment, warm:

| # | Component | Budget | Notes |
| --- | --- | --- | --- |
| 1 | Network, client to service | 0.2–1.0 ms | **The customer's network, not ours.** Frequently the largest single term (J-7) |
| 2 | TLS termination | 0.05–0.3 ms | The customer's proxy, or ours |
| 3 | HTTP parse, route, middleware | ~40 µs | Pure CPU |
| 4 | Authenticate | ~80 µs | Argon2id verification, cached hash |
| 5 | Validate | ~20 µs | Pure CPU |
| 6 | Snapshot resolve | ~15 µs | One map lookup plus a small slice scan |
| 7 | **Cycle compute** | ~5 µs | Logarithmic in elapsed intervals, resolved `*time.Location` |
| 8 | **Script round trip + execution** | **0.3–2.0 ms** | The dominant term, and mostly network |
| 9 | Serialise and write the response | ~60 µs | Pure CPU |
| 10 | Ledger enqueue | ~5 µs | Non-blocking send |
| 11 | Observe, metrics, log | ~20 µs | Buffered |
| | **Total, service-side** | **0.5–2.5 ms** | p50 target 1.0 ms, p99 target 5.0 ms |
| | **Total, end to end** | **1–5 ms** | p99 depends mostly on the customer's network |

**The single most useful fact in this table is that the script round trip dominates.** The
optimisation effort for this product therefore belongs in the script and in the round trip, not in
Go. A profiler-guided optimisation of the Go request path can plausibly save 200 µs; halving the
script round trip saves a millisecond. The corollary is that the design should not add round
trips: see section 3.

## 2. What the script costs

Three costs, in ascending order of importance, and the third is the one to watch.

**Parse and load.** Redis caches the compiled script and `EVALSHA` references it by SHA-1. The
client sends the script body only on `NOSCRIPT`, which happens after a restart or a flush. Two
clients racing a `NOSCRIPT` both send bodies; the second is a no-op. `quotacore_script_cache_hits_total`
must stay above 0.99 (section 5 of [observability.md](observability.md)).

**Transfer.** `EVALSHA` sends only the SHA and the arguments. This is a direct reason the script
bodies are static and not assembled per request: a dynamically composed script is a per-request
body transfer, which would be the largest term on the path. A feature that needs a
request-specific script is a design error here, not an optimisation opportunity.

**Execute.** The script's cost is proportional to the work it does, which is fixed and small:
a handful of `HGET`s, comparisons, one `HINCRBY`, one `SET`. It does **not** scale with the number
of tenants, the number of features, or the size of `metadata`. This is the property that makes
the latency predictable, and it is a property to defend: any loop over a collection inside the
script is a defect, because the collection's size is a customer-controlled number.

**A note on Redis internals, for the reader who will worry about it.** `SCRIPT LOAD` on Redis 7
stores the script in a shared, refcounted cache rather than per-connection, so script execution
does not require an implicit `EVAL` on every call. A connection-pooled client is therefore correct
and cheap. This was checked against the engine matrix rather than assumed, and the assertion lives
in the integration suite (A-09).

## 3. Round trips, and why the count is one

The single most consequential performance property of this design is that enforcement performs
**exactly one** round trip to the data store, plus optionally one `PUBLISH` from the control
plane.

| Attempt | Round trips | Verdict |
| --- | --- | --- |
| Read the balance, decide in Go, then write | 2+ | **Rejected.** The window between read and write is where a double charge is born. Not a performance question |
| `EVAL` with a body per request | 1, plus a large body | **Rejected.** Static scripts and `EVALSHA` |
| `HMGET` then `EVALSHA` | 2 | **Rejected.** The read is redundant; the script has the data |
| One `EVALSHA` for the whole decision | 1 | **Chosen** |

Two of the rejected options are also the ones that would be fastest in a microbenchmark, which is
worth stating plainly: a design that needs two round trips is not a performance problem to be
optimised, it is a correctness problem that happens to also be slow.

## 4. Throughput

| Quantity | Target | Bound by |
| --- | --- | --- |
| Single instance, sustained | 2,000 `consume`/s (NFR-T1) | Data store round-trip concurrency, not the process |
| Single instance, burst | 10,000/s for 60 s (NFR-T2) | Connection pool size and the data store's own limits |
| Tenants | 50,000 (NFR-T4) | Snapshot memory, which is capped and therefore evicting, not fatal |
| Features per plan | 100 (NFR-T5) | Validated. A larger plan is a validation error, not a slow request |
| Ledger appends | 2,000/s (NFR-T8) | Batched writes; a full queue drops rather than blocking |

**On the connection pool.** The pool must exceed the expected concurrent in-flight requests, or
requests queue at the client rather than at the server, which turns a throughput limit into a
tail-latency cliff. The default is sized for NFR-T2. The `datastore_latency_seconds` histogram
rises before throughput degrades, and it is the metric to watch when a customer reports slowness:
if the data store latency is flat and the p99 is rising, the problem is ours; if the data store
latency is rising, the problem is theirs and no amount of Go optimisation will help.

**On CPU.** At 2,000 requests/s, the process is not CPU-bound. Two cores is recommended because
GC and the ledger writer compete with the request path, not because request handling needs the
cores. If a profile ever shows request handling dominating, the design has grown something on the
path and that is the finding, not the allocation site.

## 5. The ways this becomes slow

An ordered list, most likely first, with the signature of each. This is the document to read
during a latency incident.

| Cause | Signature | Fix |
| --- | --- | --- |
| **A loop inside the script** over a collection | p99 grows with tenant or feature count; script execution time grows with data volume | Delete the loop. The script must be O(1) in customer-controlled data |
| **`KEYS` in a debug path left in production** | p99 seconds, a blocked data store, everything else fine | Remove it. Use `SCAN` with a count, in a CLI-only path |
| **`EVALSHA` cache miss storm** | `script_cache_hits` below 0.99, latency spikes every flush interval | Investigate what is flushing. A restart loop is the usual answer |
| **The script grew to cover a new case** | p50 and p99 rise together, script execution time rises | Split the script by operation. One script per endpoint, not one script with a mode flag |
| **Snapshot cache thrash** | `config_cache_evictions` rising, `cache_hit_ratio` falling, p99 intermittent | Raise `QUOTACORE_CONFIG_CACHE_ENTRIES` or `QUOTACORE_CONFIG_CACHE_MB` |
| **Cache miss storm on a cold cache** | `config_cache_miss_total` spiking, then `dbpool_wait` rising | The miss rate limit is doing its job. Pre-warm on startup, and investigate why the cache went cold |
| **Argon2id cost too high for the hardware** | `auth` dominating the profile, p99 rising with concurrency | Reduce the work factor, within a documented minimum. This is a deliberate trade: it weakens brute-force resistance to improve latency |
| **The ledger writer saturating a connection** | p99 rising while data-store latency is flat; `event_queue_depth` growing | Move the writer to a separate pool with a small size. It should never contend with the request path |
| **The queue being full and the non-blocking send spinning** | CPU at 100%, p99 high, `event_sink_dropped_total` rising | The send must be a single non-blocking attempt that drops. Spinning is a defect |
| **GC pressure from per-request allocations** | `gc_pause` rising, p99 correlated with allocation rate | Profile. The usual cause is a per-request map or a `fmt.Sprintf` in a path that should not format anything |
| **Logging the body** | CPU and I/O high, p99 high, `event_written` fine | Remove it. `metadata` was never to be logged (DR-043) |
| **The customer's network** | Data-store latency flat, service p99 flat, end-to-end p99 high | Nothing to fix here. This is J-7, and the deployment checklist's item 10 exists for it |
| **The customer's own contention** | Service p99 flat, their service slow | Nothing to fix here either |

## 6. Profiling and measurement

**What to measure, in order of usefulness**

1. **End-to-end from the customer's network.** The number the customer experiences, and the only
   one that matters to them. A service-side measurement on loopback is a self-congratulatory
   metric.
2. **The script round trip, separated from script execution.** The data store's own `latency`
   versus the client's observed round trip isolates network from execution.
3. **Stage-level spans.** `auth`, `resolve`, `cycle`, `evalsha`, `respond`, from
   [observability.md](observability.md) §3. Enough to identify which of the five budget lines is
   wrong, without a per-line span tax.
4. **Allocation profile.** Only when the p99 correlates with GC.
5. **CPU profile of the process.** Only when a stage span is already known to be hot. A CPU profile
   on a service that is waiting on a socket teaches nothing.

**Measurement rules.** Warm measurements only for latency, with a stated warm-up, because a cold
snapshot produces a different number and a customer will never see it. Report p50, p99 and p99.9
together, because a good p50 with a bad p99.9 is the shape of almost every real problem. Never
average percentiles. And measure the case that crosses a cycle boundary, because it is the slow
path and it is the path a support conversation is about to be about.

## 7. The boundary-crossing case, specifically

It is the slowest legitimate request, so it gets its own treatment.

| Step | Added cost |
| --- | --- |
| Cycle compute finds `target_index > stored_index` | ~5 µs |
| Script performs the transition: four field writes, `bonus = 0`, `EXPIREAT` | ~50 µs |
| One extra `cycle_rolled_over` event, enqueued like any other | off-path |
| `EXPIREAT` instead of `EXPIRE` | zero |
| **Total** | **~55 µs, so about 3% of the p50 budget (NFR-L7)** |

The reason this is cheap is that the transition is four field writes inside an execution that was
already happening. There is no second round trip, no lock, no transaction, and no coordination
with the worker. A design that rolled the cycle in a separate `MULTI` would pay a round trip and
would need a lock; a design that rolled it in a `CRON` would pay a race.

**The one case that is genuinely slower** is a tenant that has not been seen in a long time, where
`currentIndex` must search a wide range. It is logarithmic (section 3 of
[cycle-engine.md](cycle-engine.md)), so a five-year-old hourly tenant is about 22 iterations of
cheap date arithmetic. A benchmark asserts the worst case, because the alternative — a linear scan
from the anchor — is the kind of thing that is fast in testing and pathological in production for
exactly one tenant.

## 8. Latency-sensitive configuration

| Setting | Effect on latency | Note |
| --- | --- | --- |
| `QUOTACORE_DATASCRIPT_TIMEOUT` | The hard tail bound | 250 ms. Raising it makes failures slower, which is worse than a larger p99 |
| `QUOTACORE_REQUEST_TIMEOUT` | An outer backstop | 2 s. A `consume` should never approach it |
| `QUOTACORE_METADATA_MAX_BYTES` | Serialisation cost | 4 KiB. Larger values cost on every request for every customer |
| `QUOTACORE_MAX_ENTITLEMENTS_PER_PLAN` | Snapshot memory and the resolve scan | 100. The resolve scan is O(entitlements) and is a reason to keep plans small |
| `QUOTACORE_CONFIG_CACHE_ENTRIES` | Hit ratio, hence p99 | The single most effective latency setting available |
| `QUOTACORE_EVENT_QUEUE_SIZE` | Drop rate under burst | Drops are better than blocking (FS-10) |
| `QUOTACORE_EVENT_FLUSH_INTERVAL` | Ledger durability latency, not request latency | 100 ms, off the path |
| Argon2id work factor | Auth cost, on every request | The one security setting with a direct latency cost. A documented minimum, and a decision the operator makes knowingly |

## 9. Performance claims, and what backs each one

| Claim | Backing | Where verified |
| --- | --- | --- |
| p50 ≤ 1 ms, p99 ≤ 5 ms warm | The budget in section 1, dominated by one round trip | Load test in [testing-strategy.md](testing-strategy.md) §5 |
| 2,000 `consume`/s sustained | One round trip, O(1) script, bounded snapshot | 30-minute soak |
| 200 racing requests, exactly 100 allowed | Atomic script | The contention test, treated as a **correctness** test |
| A cycle boundary costs ~3% extra | Section 7 | Benchmark with an imminent boundary |
| No round trips beyond the script | Section 3 | Asserted by a test that counts client commands per request |
| O(1) in customer-controlled data | No loops in the script | A fuzz test asserting script execution time is flat as metadata and feature counts grow |
| A dead data store is fast, not a hang | Script timeout, NFR-L4 | The test that kills the data store mid-request |
| 50,000 tenants | Bounded, evicting snapshot | Fixture load with a memory ceiling |
| Recovery adds no latency | The worker uses the same path as a request | Comparative measurement, asserted in CI |
