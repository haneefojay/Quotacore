# ADR-0002 — Server-side Lua scripts are the atomicity primitive

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [request lifecycle](../architecture/request-lifecycle.md), [research: atomicity](../research/atomicity-mechanism-options.md)

## Context

The central guarantee of the product is: *if a tenant's balance is 1 unit, then among any
number of concurrent requests to consume 1 unit, exactly one succeeds.* This must hold across
multiple Quotacore processes sharing a datastore, and it must hold without the customer writing
distributed-locking code.

Atomicity has to be established somewhere. The candidates are the datastore, the application
process, or a lock service. Each places the guarantee differently and each has a different
failure mode under process death and network partition.

## Options considered

**A — In-process mutex in the Go application.** Guard a `map[tenant]int64` with a `sync.Mutex`
or a sharded lock.
- Pros: lowest possible latency, no network, trivially correct in a single process.
- Cons: correctness is per-process. Two replicas double-spend immediately. Horizontically
  scaling the service — which the latency requirements practically demand — makes it unusable.
  Rejected outright.

**B — PostgreSQL conditional update.**
`UPDATE tenant_balances SET balance = balance - $amount WHERE tenant_id = $id AND balance >= $amount`
is genuinely atomic and is the correct answer for a system without a latency budget.
- Pros: one store, durable, transactional with everything else, trivially queryable history.
- Cons: a single hot tenant row serialises all its traffic. Latency under contention is
  dominated by lock waits and, on any synchronous commit setting, by a disk round trip. This
  conflicts directly with the stated p99 budget. Retained only as the control-plane store, which
  has no latency budget.

**C — Redis `MULTI`/`EXEC`.**
- Pros: one round trip, atomic.
- Cons: `MULTI` cannot branch. Reading the balance, deciding, and writing the decremented value
  inside the transaction is impossible, so the read must happen *before* `MULTI`, which
  reintroduces the race; or the client must use `WATCH` and retry, which costs additional round
  trips under exactly the contention the product must survive. Rejected: it optimises away the
  wrong part of the problem.

**D — Server-side Lua script via `EVALSHA` (chosen).** One script receives the current state,
applies the conditional decrement, and returns the outcome. Redis/Valkey executes scripts
atomically: no other command interleaves for the duration.

**E — Redis Functions (7.0+).** Named, library-organised, persisted in the keyspace and
replicated with the dataset.
- Pros: survives restarts and failovers without the client reloading; scripts can call each
  other; libraries are versioned in the store.
- Cons: the function's lifecycle and version then live in the datastore, not in the Quotacore
  binary. During a rolling deploy, old and new replicas can be executing different function
  bodies, and a mixed fleet cannot be reasoned about. Redis's own documentation states the
  complementary assumption for `EVAL`: that scripts are part of the application, not maintained
  by the server. Rejected in favour of application-owned scripts.

**F — Distributed lock service (etcd, Redlock).** Acquire a lock, mutate, release.
- Pros: works with any backing store.
- Cons: adds a third datastore, multiplies the round trips, and Redlock's safety under clock
  drift is disputed. Vastly more machinery for a strictly weaker guarantee than a single atomic
  script. Rejected.

## Decision

**Option D.** Enforcement state lives in a Redis-compatible store and is read and written only
by server-side Lua scripts invoked with `EVALSHA`, loaded at process start and reloaded on
`NOSCRIPT`.

The keyspace uses a per-tenant hash tag so that every key a script touches hashes to one slot
and the script therefore remains valid under Redis Cluster:

```
qc:{t:<tenant_id>}:bal:<feature_key>    HASH  bal, lim, bonus, cs, ce, thr
qc:{t:<tenant_id>}:idem:<idempotency_key>  STRING
qc:{t:<tenant_id>}:f:<feature_key>     STRING   (boolean entitlement, v0.2)
qc:{t:<tenant_id>}:w:<feature_key>     ZSET     (rolling window, v0.2)
```

The hash tag is the literal `t:<tenant_id>` segment. A single enforcement script therefore
touches at most two keys — the balance hash and the idempotency key — both inside the tenant's
slot, which keeps the operation a single round trip and cluster-legal.

Scripts are kept deliberately small and bounded: no loops over unbounded collections, no
external I/O, no reliance on non-determinism. A script that blocks the datastore is a
site-wide outage, so this is treated as a correctness constraint, not a style preference. The
v0.2 rolling-window script is the only one permitted to touch a collection, and it is bounded by
a hard cap on limit size.

## Rationale

The guarantee the product sells is a *shared-state* guarantee across processes, so the
primitive must live in shared state. Among the shared-state options, a conditional read-modify-
write expressed as a server-side script is the only one that gets the decision and the write
into the same atomic unit with a single round trip. `MULTI`/`EXEC` cannot express the decision.
Functions can, but they move the version boundary away from the application, which is
unacceptable for a service that must run mixed-version replicas during a rolling deploy.

Choosing Lua over an in-process design is the decision that makes horizontal scaling possible
at all, and it is what allows the product's headline claim to be true regardless of how many
replicas the customer runs.

## Consequences

**Positive**
- The no-double-spend guarantee is a property of the datastore, not of deployment topology.
- One round trip per enforcement request; the datastore never blocks on disk.
- Key layout is cluster-legal, so Redis Cluster remains a scaling option without a redesign.
- A contention test becomes trivially assertable: N concurrent requests against a balance of 1
  must yield exactly one success.

**Negative**
- The atomicity logic lives in Lua, a second language in the repository, and cannot share types
  or tests with the Go code around it. Mitigation: keep the scripts to a handful of lines each,
  and test them through the public HTTP API rather than by unit-testing Lua in isolation.
- Correctness depends on a single Redis primary. Cluster failover, replica promotion, or a
  `SCRIPT FLUSH` all require a documented response. `SCRIPT FLUSH` is handled by the
  `NOSCRIPT` reload path; failover is covered by the failure matrix in
  [ARCHITECTURE.md](../../ARCHITECTURE.md).
- Every key touched by one script must live in one hash slot. This deliberately concentrates a
  single very large tenant on one shard. Accepted and documented: a tenant large enough to
  saturate a shard should be modelled as multiple tenants.
- An operator running a non-script-capable Redis-compatible service will find enforcement
  broken. The supported-configuration check in [deployment.md](../architecture/deployment.md)
  validates script execution at startup.

## Revisit when

- Enforcement must span regions, where a Lua script cannot straddle stores.
- The customer base is dominated by single-instance, low-traffic deployments, in which case a
  single-writer embedded store would remove an entire dependency.
- Redis/Valkey deprecates or changes script semantics in a way that breaks the guarantee.
