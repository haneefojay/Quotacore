# Non-Functional Requirements

The measurable properties this system must hold. Each requirement has a number, a measurement
method, and a consequence if it is missed. A requirement without a number is a wish, so none
appear here.

Numbers are the v0.1 MVP targets on the reference deployment in
[deployment.md](deployment.md): 2 vCPU, 4 GiB RAM, and both datastores on the same host or on
latency-adjacent hosts.

## 1. Latency

| ID | Requirement | Target | Measured by | If missed |
| --- | --- | --- | --- | --- |
| NFR-L1 | `POST /v1/consume` p50, warm | ≤ 1.0 ms | Load test, in-region, 100 req/s | Look first at snapshot lookup, then at script size, then at TLS |
| NFR-L2 | `POST /v1/consume` p99, warm | ≤ 5.0 ms | Load test, 100 req/s, 10 min | A p99 problem is almost always a script problem: KEYS, unbounded loops, or a missing `EVALSHA` cache |
| NFR-L3 | `POST /v1/consume` p99.9 | ≤ 20 ms | Load test at 10× expected peak | GC pauses, snapshot refresh stalls, or a datastore hiccup. Each is separately addressable |
| NFR-L4 | Tail bound under no dependency | ≤ 250 ms, hard | Kill the datastore mid-request, measure the returned error | A script that blocks on a dead socket violates the core promise of DR-037 and is a P0 defect |
| NFR-L5 | `POST /v1/check` p99 | ≤ 3.0 ms | Load test | Lower than `consume` is expected: no mutation, no idempotency write |
| NFR-L6 | Control-plane p99 | ≤ 300 ms | Admin API load test | Irrelevant to correctness. Degradation here must not be visible on the data plane |
| NFR-L7 | Rollover adds | ≤ 1.0 ms to a request that crosses a boundary | Benchmark with the boundary artificially imminent | A rollover implemented as multiple round trips would show here immediately |
| NFR-L8 | Process startup to ready | ≤ 5 s | Container start to `/readyz` true | Migration execution is the variable; report it separately |

**The budget, in order.** Network RTT to the data store, script execution, snapshot lookup,
serialisation. NFR-L2 exists to keep the script honest. If the script is the problem, the answer
is a smaller script, not a faster machine: a Lua script that grows with the number of features or
with the request size will eventually dominate every number above.

## 2. Throughput and capacity

| ID | Requirement | Target | Measured by |
| --- | --- | --- | --- |
| NFR-T1 | Single instance, sustained | 2,000 `consume`/s | 30 min soak, p99 stable, no unbounded memory growth |
| NFR-T2 | Single instance, burst | 10,000 `consume`/s for 60 s | Burst test; queueing is acceptable, failure is not |
| NFR-T3 | Concurrent correctness | 200 racing for a balance of 100, exactly 100 allowed | Contention test, the primary correctness assertion |
| NFR-T4 | Tenants per instance | 50,000 | Fixture load; memory per tenant is bounded by the snapshot cap, not by tenant count |
| NFR-T5 | Features per plan | 100 | Validation limit; documented rather than silently enforced by a struct size |
| NFR-T6 | `metadata` size | 4 KiB | Rejected with `413 payload_too_large` above it |
| NFR-T7 | Snapshot memory | ≤ 64 MiB configured cap | Exceeding it evicts, and the eviction is a metric, not an OOM |
| NFR-T8 | Ledger throughput | ≥ 2,000 appends/s | Batched writer; a full queue drops and increments, per FS-10 |
| NFR-T9 | Idempotency working set retained | The full `QUOTACORE_IDEMPOTENCY_TTL` window at NFR-T1, with `maxmemory-policy noeviction` | DR-048. Measured in `IP-15` by the 24 h projected soak, not estimated. Capacity reached is `503 service_unavailable` (FS-21), never an eviction, because a displaced record is an uncorrectable double charge |
| NFR-T10 | Detected-duplicate reversal | Within one reconciliation cycle of the duplicate being written | DR-049. Bounded by the reconciliation interval rather than by an independent constant, so there is no second number to keep in step. The reversal is eventually consistent, and the cycle is the honest bound on how eventually |

NFR-T3 is the requirement that matters. Everything else is capacity; this one is correctness.

## 3. Availability

| ID | Requirement | Target | Notes |
| --- | --- | --- | --- |
| NFR-A1 | Service uptime, reference deployment | 99.9% monthly | Measured as successful `/readyz`; excludes customer host failure |
| NFR-A2 | Data-plane availability during a control-plane outage | 100% for cached tenants | The point of the two-plane split. A Postgres outage must not be visible to a `consume` |
| NFR-A3 | Data-plane availability during a datastore outage | 0% | Fail-closed by design. This is a deliberate availability sacrifice, stated as a number so it is not mistaken for a defect |
| NFR-A4 | Time to recover enforcement after the datastore returns | ≤ 1 s | Requires no restart, no reindex, no manual step |
| NFR-A5 | Graceful shutdown | 30 s, in-flight requests complete | Requests are not cancelled mid-script; `SIGTERM` drains, `SIGKILL` is safe because every write is atomic |
| NFR-A6 | Restart with no state loss | Balance unaffected | All runtime state is in the datastore; the process holds no authoritative state |

**A3 is the requirement most likely to be argued with.** It is correct, and the argument to have
with a customer is not "make it fail open" but "what does your application do on a 503", because
that decision is theirs. The system's job is to fail fast, fail typed, and fail without corrupting
anything (DR-037).

## 4. Durability and consistency

| ID | Requirement | Target | Notes |
| --- | --- | --- | --- |
| NFR-D1 | Balance consistency | Strong, per tenant and feature | One atomic script execution; a request never observes a partial mutation |
| NFR-D2 | Idempotency window | 24 h | DR-029. Beyond it, a key is a new operation. Guaranteed against eviction and capacity (DR-048); across **total store loss** the window does not survive, because an idempotency record is not derivable from the ledger, and the resulting double charge is reversed instead (DR-049) |
| NFR-D3 | Configuration propagation | ≤ 1 s p99, published distribution | Open question Q-08; the number is measured, not assumed |
| NFR-D4 | Audit durability | Same transaction as the mutation it records | An audit row that could be lost is not an audit log |
| NFR-D5 | Ledger durability | Best-effort, with a visible drop counter and a visible gap | Deliberate. At-least-once enforcement, best-effort history (FS-10) |
| NFR-D6 | Zero-loss restart | No balance, cycle or idempotency state lost | The process owns no authoritative state |
| NFR-D7 | Full datastore loss recovery | Rebuildable from configuration + ledger | Valid for a full-allowance-recovery case; flagged approximate. Drill in [consistency-and-recovery.md](consistency-and-recovery.md). **Does not extend to idempotency records**, which are not derivable from the ledger; see NFR-T10 |
| NFR-D8 | Clock skew tolerance | None; NTP is a hard requirement | DR-001. A skew shifts a boundary, and a boundary is a contract term |

## 5. Security

Requirements here are elaborated in [security-model.md](security-model.md).

| ID | Requirement |
| --- | --- |
| NFR-S1 | No API key is recoverable in plaintext from any persistent store, log, or metric |
| NFR-S2 | No personally identifiable information is required, accepted, or stored. `external_id` excludes `@` |
| NFR-S3 | The service makes no outbound connection in the MVP. No telemetry, no update check, no phone-home |
| NFR-S4 | All state-changing operations require authentication and authorisation, with no anonymous admin path after bootstrap |
| NFR-S5 | Admin actions are attributable to a key and auditable with before and after values |
| NFR-S6 | The service runs unprivileged, on a non-root port, with a read-only filesystem outside its data volumes |
| NFR-S7 | Dependencies are pinned, and the build produces an SBOM with licence information |
| NFR-S8 | Lua scripts are static, shipped in the binary, and never assembled from customer input. A scripting injection vulnerability is a P0 class defect |
| NFR-S9 | The service refuses to start rather than run with a default bootstrap key that has not been acknowledged |
| NFR-S10 | Every response carries `X-Request-Id`; every log line carries it; it is quoteable in a bug report |

## 6. Observability

| ID | Requirement | Target |
| --- | --- | --- |
| NFR-O1 | Metrics | Full catalogue in [observability.md](observability.md), including a counter for every error code |
| NFR-O2 | Metrics cardinality | No unbounded label values. Tenant identifiers are labels only when the tenant count is bounded and the label is documented as costly |
| NFR-O3 | Logs | Structured JSON, one line per request and per state change, no secrets, no `metadata` free text by default |
| NFR-O4 | Health probes | `/healthz` liveness and `/readyz` readiness, distinct, unauthenticated, no dependency detail leaked in the body |
| NFR-O5 | Status endpoint | `GET /v1/admin/status` reports dependency health, version, zone-data version, current time, and snapshot size, for `admin` keys only |
| NFR-O6 | No PII in telemetry | No tenant names, no `external_id` values, no free-text `metadata` |
| NFR-O7 | Alertable | Every P0 condition in [observability.md](observability.md) has a documented signal and a first response |

## 7. Operability and portability

| ID | Requirement | Target |
| --- | --- | --- |
| NFR-OPS1 | First run | `docker compose up` and a working enforcement path, with no manual step and no account |
| NFR-OPS2 | Configuration | Every tunable is an environment variable or a documented config file key, listed in one place |
| NFR-OPS3 | Migrations | Applied automatically at startup by an embedded binary, forward-only, with a documented downgrade-by-restore procedure |
| NFR-OPS4 | Backups | Postgres is the backup responsibility. The Redis-compatible store is a cache-plus-runtime store and is rebuilt, not backed up. Stated plainly so nobody backs up the wrong thing. The asymmetry that matters: **balances are rebuildable from the ledger and idempotency records are not**, so a total store loss inside a window is corrected by reversal rather than by reconstruction (NFR-T10) |
| NFR-OPS5 | Platforms | Linux on x86-64 and arm64. Windows and macOS via containers, because Valkey has no native Windows build |
| NFR-OPS6 | Portability | No platform-specific syscall use, no cgo, so the binary is static and the container is minimal |
| NFR-OPS7 | Upgrade | Rolling upgrade is not supported in the MVP. The documented procedure is a drain, upgrade, restart, with NFR-A5 and NFR-D6 making it safe |
| NFR-OPS8 | Time zones | Full IANA database embedded via `time/tzdata`. No system zone database dependency, because a host that updates its zone database must not change a customer's cycle boundaries retroactively |

## 8. Compliance and licensing posture

| ID | Requirement |
| --- | --- |
| NFR-C1 | Apache-2.0, with the patent grant. Compatible with being vendored into a proprietary product |
| NFR-C2 | Default datastore image is BSD-3-Clause licensed. The Redis licence change from RSALv2/SSPLv1 to AGPLv3 at version 8, and its implications, are documented in ADR-0009 rather than discovered during an upgrade |
| NFR-C3 | SBOM generated at build time, including the Go module graph and container base image |
| NFR-C4 | No copyleft dependency in the runtime path without an explicit decision record |
| NFR-C5 | Data residency is the customer's, because the data is on the customer's host. No data leaves the host in the MVP |
| NFR-C6 | Audit and event retention are configurable, bounded by default, and applied by a background job rather than by an API call (DR-044) |

## 9. Explicitly not required

Stated so that nobody spends effort on them, and so nobody assumes they are oversights.

| Not required | Reason |
| --- | --- |
| 99.99% or 99.999% availability | A self-hosted single-instance service cannot honestly promise it. Promising it would make the SLA a fiction |
| Multi-region durability | Out of scope; data lives on the customer's host |
| Zero-downtime upgrades | A single-instance service cannot provide it without introducing the complexity that section 8 of [overview.md](overview.md) excludes |
| Horizontal scale-out of the data plane | Out of scope, deliberately. Adding an application-level sharding scheme would be a worse product than not having one |
| Formal compliance certification | Out of scope. The controls in [security-model.md](security-model.md) are documented and verifiable; a certification claim would not be |
| Accessibility conformance beyond API semantics | A v0.4 UI concern. The API itself is machine-readable and has documented status codes for every outcome |
