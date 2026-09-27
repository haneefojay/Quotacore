# Research: Atomicity Mechanisms

**Date:** 2026-09-27 · **Status:** complete · **Decision:** [ADR-0002](../decisions/0002-lua-scripts-for-atomicity.md)

## Question

How can a read-decide-write sequence be made atomic in a store that is not a transactional
database, and what is actually guaranteed by each available mechanism?

## Why it mattered

This is the load-bearing decision in the product. If the mechanism is not genuinely atomic, then
under concurrency the service oversells, and every downstream guarantee — the idempotency story,
the ledger, the reconciliation, the "we never charge twice" claim — is decoration on top of a
race condition. It was worth investigating properly rather than reaching for the obvious tool.

## Method

Enumerated the available mechanisms in the Redis/Valkey family, then for each one identified: the
primitive it relies on, whether the guarantee is unconditional, what it costs, and how it fails.
Where a guarantee depends on a runtime property (a Redis configuration, a cluster topology, a
failover mode), that dependency was recorded explicitly rather than assumed away.

## Findings

### The candidate mechanisms

| Mechanism | Primitive | Atomic? | Cost | Failure mode |
| --- | --- | --- | --- | --- |
| `MULTI`/`EXEC` with `WATCH` | Optimistic lock | **Conditionally.** Only if no other client modified the watched keys in between | Two extra round trips: `WATCH` and `UNWATCH`/`EXEC` as a pair, plus a `MULTI`/`EXEC` | `EXEC` returns nil on a conflict. The caller retries. Under contention this becomes a retry storm, and starvation is possible |
| `MULTI`/`EXEC` without `WATCH` | Transaction | **Yes for the batch**, but there is no conditional. It runs the queued commands regardless | One extra round trip, and every command in the queue runs | Cannot express "deduct only if sufficient". A read cannot be used to make a later command conditional, so the check must happen before the `MULTI`, which reintroduces the race |
| Lua `EVAL` / `EVALSHA` | Script execution | **Yes, unconditionally.** A script runs to completion with no interleaving | One round trip, no extra | A long-running script blocks the store. A bug blocks the store |
| `SETNX` on a derived key | Compare-and-set on one key | Yes for that key only | Two round trips, and the second is not conditional on the first's result | Works only for a single key's state, and the second step still needs a read |
| Distributed lock (Redlock or a lock service) | Lock acquisition | **Not really.** A lock is a lease, and a lease can expire while the holder is still working | At least three round trips, plus a renewal loop | Clock skew, a paused process, or a slow operation between acquisition and release produces a split brain. The correctness of the system then depends on the correctness of the lock, which is a distributed-systems problem in its own right |
| Postgres transaction | ACID | Yes | 10–50 ms, and it puts the database on the hot path | Connection exhaustion, and the control plane's outage becomes enforcement's outage |
| Optimistic database versioning | Compare-and-swap on a row | Yes | Same | Same |

### The decisive finding

**Lua is the only mechanism in the list that makes the whole operation indivisible in one round
trip with no external coordination.** Every other option is either conditional, or expensive, or
introduces a new component whose own failure modes become the product's failure modes.

The `WATCH` case is the instructive one, because it is what most implementations reach for first.
It is genuinely atomic — but only if the caller retries correctly, and the retry loop is where
starvation, duplicate side effects and unbounded latency appear. For a service that must never
oversell, "atomic if the client retries properly" is a weaker guarantee than "atomic".

### Properties of the chosen mechanism that had to be confirmed

| Property | Confirmed | Note |
| --- | --- | --- |
| A script runs atomically with respect to other clients | Yes | Single-threaded execution per store, no interleaving |
| Replica execution does not break atomicity | Yes | In Redis 7+, scripts may replicate as effects rather than as source. The atomicity guarantee holds; the determinism of the script body is no longer required, which is a change from older versions and is worth knowing |
| A script that errors mid-way leaves no partial writes | **No. It is a requirement on the script, not a guarantee from the store.** | Redis does not roll back, so any write already performed stands. The only mitigation is to validate every argument and every invariant *before* the first write, which makes an erroring script a script that wrote nothing. Asserted by the script tests |
| `NOSCRIPT` handling | Yes | `EVALSHA` fails with a specific error; the client loads with `SCRIPT LOAD` and retries. A correct client never surfaces this to a caller |
| Cost is per-execution, not per-key | Yes | A script touching 3 keys costs roughly what one touching 1 costs, plus the per-key work |
| `allkeys-lru` eviction can remove a key mid-logic | **Yes, and it matters** | A script reading a key that a concurrent eviction removed sees a nil. The script must handle a missing key as "uninitialised" rather than as an error, and this is why the balance key can be absent after a flush and still behave correctly |
| Failover can lose a write | **Yes** | An asynchronous replica promoted without the write acknowledged loses recent writes. This does not break atomicity; it loses a deduction, and a lost deduction is a customer complaint rather than a correctness violation, which is why the ledger-based reconciliation exists |

That last row is the important caveat and it was not obvious in advance: **atomicity and durability
are independent properties, and choosing an in-memory store trades the second for the first.** A
script-based design gives an absolute atomicity guarantee and a weaker durability guarantee than
Postgres would. For this product that is the correct trade, because the atomicity is what protects
revenue at request rate and the durability gap is recoverable from the ledger. A product whose
primary value were audit-grade historical accuracy would choose the other way.

## What was rejected, and why

| Rejected | Reason |
| --- | --- |
| `WATCH`/`MULTI` | Conditional atomicity, a retry loop on the hot path, and starvation risk. Two extra round trips for a worse guarantee |
| `MULTI` without `WATCH` | Cannot express a conditional deduction, so the check has to move outside the transaction and the race returns |
| Redlock or a lock service | Turns a local correctness problem into a distributed lease problem, with a new component to operate and a new class of split-brain failure |
| Postgres transactions | 10–50 ms on a path that must be single-digit milliseconds, and it couples enforcement to the control plane's availability |
| Optimistic database versioning | Same objection, plus a round trip per attempt |
| A single-key "bucket" scheme that avoids multi-key operations | Does not avoid the problem; it moves it, because the balance and the idempotency record are genuinely two things that must change together |

## Decision impact

- [ADR-0002](../decisions/0002-lua-scripts-for-atomicity.md) selects Lua, on the grounds above.
- Scripts are static, shipped in the binary, and receive all input as `ARGV`. A dynamically composed
  script would be a per-request body transfer on the hot path, and an injectable script would be a
  remote-code-execution surface (NFR-S8).
- Scripts must validate before they write, because there is no rollback.
- The atomic script is the **only** place a balance is written. No Go code path mutates a balance
  field directly, which is what makes the invariant checkable by reading one file.
- The `2^53` bound on all amounts exists because Lua 5.1 numbers are doubles, so integer
  arithmetic above that value is not exact. Discovered here, and it is the reason the DDL, the API
  and the script all bound the value identically.
  - Key eviction being a real possibility is why the "uninitialised" path exists and why the
    recovery runbook can rebuild the store from the ledger.

**Update, 2026-09-27 — ordinary eviction is gone; the path it justified is still required.** The
row above treats an `allkeys-lru` eviction removing a key mid-script as a real possibility, and draws
the right conclusion from it: a script must read a missing key as *uninitialised*, not as an error.
[ADR-0017](../decisions/0017-noeviction-and-duplicate-reversal.md) removes the possibility — the
fast store runs `maxmemory-policy noeviction` for the whole key space (DR-048) — so the script
requirement is now mostly about **total** store loss and about failover, not about memory pressure.
It is not deleted. A script that treats a nil balance as an error returns `500` where it should
return `503 service_unavailable` and queue a rebuild (DR-045), and a script that initialises from
`ARGV` in that state hands the tenant a full allowance. `noeviction` changed when the nil arrives,
not what the nil means.

## Confidence and what would change this

**High confidence.** The mechanism is well understood, widely used for exactly this purpose, and
its limits are documented by the engine vendor. The main risk is not that Lua is insufficient but
that a future contributor writes a script with a loop, which is a review-enforced rule
([performance.md](../architecture/performance.md#2-what-the-script-costs)) rather than a
structural one.

**Would change the decision:** a requirement for durability stronger than the ledger can provide,
or evidence that the engine's script execution model changes — for example if a future engine
version allowed concurrent script execution for latency reasons. Both would be a genuine
architectural change, and both are worth watching for in engine release notes.

## Sources

- Redis `EVAL` / `EVALSHA` / `SCRIPT` command documentation, including the atomicity guarantee and
  the replication-of-effects change in Redis 7.
- Redis transactions documentation, specifically the documented limitations of `WATCH` under
  contention.
- Redis key eviction documentation, for the interaction between eviction and script reads.
- The `go-redis` client documentation for `Script`, `EVALSHA` and `NOSCRIPT` handling.
- Lua 5.1 reference manual, §3.1, for the number type and its consequences.
