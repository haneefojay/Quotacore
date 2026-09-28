# Deployment

How the service runs, what it needs, how it is sized, and what the operator is responsible for.
The reference deployment is one `docker compose up`, because the fastest path to a customer's
first enforced request is the difference between adoption and a repository they star and leave
(J-1).

---

## 1. Topology

```
┌──────────────── host ────────────────────────────────────┐
│                                                         │
│   quotacore:latest                                     │
│     :8080  HTTP (data + control plane)                  │
│     :9090  metrics, bound to 127.0.0.1                 │
│     non-root, read-only fs, no-new-privileges           │
│                                                         │
│   ┌── postgres:16 ──────────┐  ┌── valkey:8 ─────────┐  │
│   │ volume: pgdata          │  │ volume: vdata      │  │
│   │ scram auth, no trust    │  │ no password in the │  │
│   │ non-superuser app role  │  │ compose file       │  │
│   └─────────────────────────┘  └─────────────────────┘  │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

All three containers on one host is the reference and is adequate for the great majority of
deployments, including production for a single-tenant customer. The two datastores may be moved
to separate hosts; nothing about the design requires co-location, only that the data store is
close enough to meet NFR-L2.

## 2. The quickstart

The first-run experience is a product requirement, not a convenience (JTBD-7, NFR-OPS1).

```bash
git clone https://github.com/quotacore/quotacore.git
cd quotacore
docker compose up -d
docker compose exec quotacore quotacore init
```

`init` is interactive, takes about sixty seconds, and does the minimum: create a feature, a plan,
one entitlement, one tenant, and one runtime key. It prints each identifier as it is created, and
it prints the runtime key exactly once.

```
quotacore 0.1.0
  feature  llm.tokens.output  (metered, unit: tokens)
  plan     starter            (1 entitlement)
  tenant   local-dev          (plan: starter, timezone: UTC)
  key      qc_live_9f2c1a4e7b8d…   ← shown once

  cycle    2026-09-27T00:00:00Z → 2026-10-01T00:00:00Z
  try      curl -s localhost:8080/v1/balance \
             -H "Authorization: Bearer qc_live_9f2c1a4e7b8d…" \
             -d '{"tenant_external_id":"local-dev"}'
```

No account, no signup, no email, no cloud console, no dashboard, no telemetry. The README's
quickstart is executed by a script in CI against a real stack, so it cannot rot
([mvp-scope.md](../product/mvp-scope.md) §3).

## 3. Image

| Aspect | Choice | Reason |
| --- | --- | --- |
| Build | Multi-stage, Go 1.27 | The builder never reaches the runtime image |
| Base | A pinned, minimal, distroless-style runtime, pinned **by digest** | NFR-S7. A tag is a moving target and a supply-chain surface |
| User | Non-root, fixed uid | NFR-S6 |
| Filesystem | Read-only except the data volume and `/tmp` | A writable image is a persistence mechanism nobody intended |
| Capabilities | All dropped, `no-new-privileges` | A Lua evaluation surface with capabilities is a bad combination |
| Size | Target under 30 MB | Not a vanity metric: a smaller image is a faster pull on the customer's connection |
| cgo | Disabled, `CGO_ENABLED=0` | Static binary, no libc surprises (NFR-OPS6) |
| Zones | `time/tzdata` embedded | A host that updates its zone database must not change a customer's cycle boundaries (NFR-OPS8) |
| Health | `HEALTHCHECK` using `/readyz` | `docker compose up` reports a working service rather than a running one |
| SBOM | Generated per build, published as an artifact | NFR-C3, NFR-C4 |
| Signature | Cosign or equivalent, documented | Supply-chain verification the customer can actually perform |

Migrations are embedded in the binary and applied at startup by `goose` (ADR-0011). There is no
migration container, no manual step, and nothing for the operator to install (NFR-OPS3).

## 4. Configuration

Every tunable, in one place, because "what can I configure" is a question that must not require
reading the source.

### 4.1 Core

| Variable | Default | Notes |
| --- | --- | --- |
| `QUOTACORE_LISTEN_ADDR` | `:8080` | — |
| `QUOTACORE_METRICS_ADDR` | `127.0.0.1:9090` | `0.0.0.0` warns |
| `QUOTACORE_DATABASE_URL` | *(required)* | `postgresql://quotacore:…@postgres:5432/quotacore?sslmode=disable` |
| `QUOTACORE_REDIS_URL` | *(required)* | `redis://valkey:6379/0` |
| `QUOTACORE_LOG_LEVEL` | `info` | `debug` in development only |
| `QUOTACORE_LOG_FORMAT` | `json` | `text` for a human at a terminal |
| `QUOTACORE_TLS_CERT`, `QUOTACORE_TLS_KEY` | *(none)* | Unset means plaintext, which logs a `warn` |
| `QUOTACORE_TRUSTED_PROXIES` | *(none)* | CIDR list. Without it, `X-Forwarded-For` is ignored, so a rate limiter behind a proxy sees the proxy |
| `QUOTACORE_STARTUP_MODE` | `normal` | `readonly` is used by the data-store recovery runbook and blocks every write to the fast store |
| `QUOTACORE_MIGRATE` | `true` | `false` runs no migration at start-up, for operators who gate deploys and run migrations separately (ADR-0011) |
| `QUOTACORE_BOOTSTRAP_ADMIN_KEY` | *(none)* | Used only when the `api_keys` table is empty (ADR-0012). Ignored, with a warning, once a key exists |
| `QUOTACORE_BOOTSTRAP_ADMIN_KEY_ACK` | *(none)* | NFR-S9. The service **refuses to start** if a bootstrap key is set without a non-empty acknowledgement. Any non-empty value acknowledges; the variable exists so that starting with a key is something the operator did on purpose rather than something a deployment inherited |

### 4.2 Limits and timeouts

| Variable | Default | Notes |
| --- | --- | --- |
| `QUOTACORE_REQUEST_TIMEOUT` | `2s` | The whole request. A `consume` never takes 2 s; if it appears to, it is an error |
| `QUOTACORE_DATASCRIPT_TIMEOUT` | `250ms` | NFR-L4. Non-negotiable, because it is what makes a dead datastore fast rather than a hang |
| `QUOTACORE_CONTROLPLANE_TIMEOUT` | `3s` | Deliberately longer than the data-plane budget, because it runs off the path |
| `QUOTACORE_SHUTDOWN_GRACE` | `30s` | NFR-A5 |
| `QUOTACORE_MAX_BODY_BYTES` | `65536` | 4 KiB of that is the `metadata` cap (NFR-T6) |
| `QUOTACORE_METADATA_MAX_BYTES` | `4096` | `413 payload_too_large` above it |
| `QUOTACORE_MAX_FEATURE_KEY_LEN` | `64` | Format-enforced |
| `QUOTACORE_MAX_ENTITLEMENTS_PER_PLAN` | `100` | NFR-T5 |
| `QUOTACORE_AMOUNT_MAX` | `9007199254740991` | `2^53 − 1`. The Lua double bound, enforced identically in the API, the DDL and the script |

### 4.3 Cache and worker

| Variable | Default | Notes |
| --- | --- | --- |
| `QUOTACORE_CONFIG_CACHE_ENTRIES` | `50000` | NFR-T4. Eviction is a metric, not an OOM (NFR-T7) |
| `QUOTACORE_CONFIG_CACHE_MB` | `64` | Memory bound, independent of entry count |
| `QUOTACORE_CONFIG_REFRESH_INTERVAL` | `30s` | The backstop when pub/sub is unavailable |
| `QUOTACORE_CONFIG_REFRESH_MISS_RATE_LIMIT` | `20/s` | A cold cache must not become a database load amplifier |
| `QUOTACORE_KEY_CACHE_ENTRIES` | `10000` | Bounded credential cache; eviction is a metric |
| `QUOTACORE_WORKER_ENABLED` | `true` | Q-13. Correctness does not depend on it (INV-WK1) |
| `QUOTACORE_RESET_SCAN_INTERVAL` | `60s` | Q-13 |
| `QUOTACORE_RESET_BATCH_SIZE` | `500` | Bounded, so a large backlog cannot monopolise |
| `QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD` | `50` | DR-046. Assigned tenants above this require a confirmation token. Sourced and dated in [a-01-design-partner-validation.md](../research/a-01-design-partner-validation.md) |
| `QUOTACORE_CONFIRMATION_TTL` | `900s` | DR-046. Bounds abandoned tokens, which matters because the store cannot evict (DR-048). Same source |

### 4.4 Ledger and retention

| Variable | Default | Notes |
| --- | --- | --- |
| `QUOTACORE_IDEMPOTENCY_TTL` | `24h` | DR-029. **Pinned, not tunable.** The window is a customer-facing guarantee, so a deployment that tries to shorten it is refused at startup for the same reason a store running something other than `noeviction` is refused. Named here because the phase and the rules refer to it; it is not a knob |
| `QUOTACORE_EVENT_QUEUE_SIZE` | `10000` | Bounded. A full queue drops and counts (FS-10) |
| `QUOTACORE_EVENT_BATCH_SIZE` | `200` | — |
| `QUOTACORE_EVENT_FLUSH_INTERVAL` | `100ms` | The latency cost of a ledger row, paid off the path |
| `QUOTACORE_EVENT_RETENTION_DAYS` | `90` | Q-09 |
| `QUOTACORE_AUDIT_RETENTION_DAYS` | `400` | Q-09 |
| `QUOTACORE_RETENTION_INTERVAL` | `1h` | Batched, with a statement timeout |

### 4.5 Rate limiting

| Variable | Default | Notes |
| --- | --- | --- |
| `QUOTACORE_RATELIMIT_ENABLED` | `true` | Q-15 |
| `QUOTACORE_RATELIMIT_RUNTIME_RPS` | `2000` | Per key. Generous by default; the point is blast-radius bounding, not throttling |
| `QUOTACORE_RATELIMIT_RUNTIME_BURST` | `4000` | — |
| `QUOTACORE_RATELIMIT_ADMIN_RPM` | `60` | Per IP. The admin API is human-driven |
| `QUOTACORE_RATE_LIMITER_MAX_KEYS` | `100000` | Bounded, so a key-churn flood is not a memory attack |

## 5. Sizing

Derived from the envelopes in [non-functional-requirements.md](non-functional-requirements.md),
not from a benchmark on a laptop.

| Resource | Minimum | Recommended | Notes |
| --- | --- | --- | --- |
| CPU | 1 core | 2 cores | At 2,000 `consume`/s, the bottleneck is the data store round trip, not the process |
| Memory | 512 MiB | 1–4 GiB | Governed by the snapshot cache cap. A 64 MiB cache is the floor; the default `QUOTACORE_CONFIG_CACHE_MB` is sized for it |
| Data store memory | 24 GiB | 32 GiB+ | **A floor, not a recommendation.** The idempotency set alone is 20 GB at NFR-T1 and the store runs `noeviction` (DR-048), so running out is a `503` for every tenant rather than a silent eviction. Measured by `IP-15`; until then, stay above it |
| Postgres | 512 MiB | 2 GiB | Configuration is tiny. The ledger grows with volume and is the reason to move it to faster storage as it grows |
| Disk | 2 GiB | 20 GiB+ | Postgres is the growing component. The fast store is bounded by its own TTLs |

**Sizing the data store for idempotency.** This is the sizing step people miss, and it is the
reason the data-store memory row above is a 24 GiB floor rather than a 1–4 GiB range. At 2,000
`consume`/s, 24 hours of keys is roughly 170 million records. At ~120 bytes each that is about
20 GB, and the `qc:*:idem:*` space is the largest consumer in the store by a wide margin.

Two things follow, and the second is the one that changed the design.

**First, the figure is a projection and must be measured.** 120 bytes per record is an
order-of-magnitude estimate that ignores the engine's own per-key overhead, its expires dictionary
and the hash table's load factor. The real figure is plausibly two to three times higher. `IP-15`
runs the 24-hour projected soak and replaces this number with a measurement. Until it does, the
table states a floor to stay above, because under-provisioning costs an availability incident and
over-provisioning costs only money.

**Second, the store must be big enough that it never needs to evict, and must be configured so it
cannot.** The `maxmemory-policy allkeys-lru` this guide previously specified was cheaper, and it
was wrong. At the sizing recommended there, the idempotency set did not fit, so that policy would
have discarded idempotency records *inside* the 24-hour window, and a client retrying inside the
window would have been charged twice. The record is the only thing standing between a retry and a
second deduction, so a policy that may discard it is not a cheaper version of the right answer; it
is a different and worse one. `noeviction` converts a silent billing error into a visible,
alertable availability event (FS-21), which is the better of the two failures even though it is
the louder one.

**What `noeviction` costs, stated plainly.** A full store refuses writes for everyone, not only
for the tenant that happened to be unlucky. That is a real availability trade, made deliberately.
It is bounded by the sizing floor, watched by `quotacore_datastore_memory_used_ratio`, and alerted
on before it is reached. Under `allkeys-lru` the same memory pressure would instead have produced
a customer-visible double charge with no counter to show for it, which is the failure mode that is
expensive to detect and expensive to explain afterwards.

**What `noeviction` does not fix.** It closes the capacity hole and leaves the restart hole open.
The store is rebuilt rather than backed up (NFR-OPS4), and an idempotency record is not derivable
from the ledger, because the ledger records usage and not keys. A total store loss inside a window
therefore still admits a double charge, and the answer is reversal rather than prevention
(DR-049, FS-22). This is the honest shape of the second claim: charged once, and a double charge
caused by store loss is reversed.

**One consequence for DR-045, in its favour.** Under `allkeys-lru` a balance hash could be evicted
by ordinary memory pressure, and each such loss was a fail-closed `503` for one tenant-feature
until the rebuild ran. Under `noeviction` a balance hash is lost only through total store loss,
which is rarer, detected, and rehearsed. The rule is unchanged and the exposure is smaller.

## 6. The data store

**Valkey by default. Redis is supported. Both are wire-compatible** (ADR-0009).

| Aspect | Decision |
| --- | --- |
| Default image | `valkey/valkey:8`, BSD-3-Clause |
| Redis support | Any engine implementing the Redis protocol, verified against a test matrix: Redis 7.4+, Valkey 7.2+, KeyDB where the client library is verified |
| Required commands | `EVALSHA`, `EVAL`, `SCRIPT LOAD`, `HGET`, `HSET`, `HINCRBY`, `HMGET`, `GET`, `SET … NX EX`, `EXPIREAT`, `PTTL`, `PUBLISH`, `SUBSCRIBE`, `PING`, `INFO`, `DBSIZE`, `SCAN` |
| Forbidden | `KEYS` anywhere in the product. A `KEYS` call on a large key space is an outage, and the temptation appears precisely when someone wants to debug |
| Persistence | **None required.** The store is rebuildable and is not backed up (NFR-OPS4). Persistence is optional and only reduces recovery time after an unclean restart. It is deliberately *not* the mechanism protecting the idempotency window: an append on every `consume` would put NFR-L1 and NFR-T1 at risk, and the window is protected by sizing plus reversal instead (DR-048, DR-049) |
| Eviction | `noeviction` on the fast store, per section 5. A write that cannot fit is refused `503 service_unavailable` (FS-21). The compose file ships this policy, and the startup check refuses to run against a store configured otherwise, because a silently downgraded policy turns a capacity limit into an uncorrectable double charge |
| Replication | Optional. Point `QUOTACORE_REDIS_URL` at a sentinel or a managed endpoint. The product is unchanged. Replication is a latency and availability choice, not a durability one, because no write is acknowledged as durable |
| Cluster | Supported by the key design. The per-tenant hash tag makes every multi-key script single-slot (A-11) |
| TLS | `rediss://` supported. A plaintext URL outside a private network logs a `warn` at startup |

**Licence.** Redis 7.4 to 7.8 is RSALv2 and SSPLv1. Redis 8 is AGPLv3. Valkey is BSD-3-Clause.
The default is BSD because a component a customer vendors into a proprietary product must not drag
a licence question into their legal review, and the Redis change is documented in ADR-0009 so an
upgrade does not surface as a surprise.

## 7. The control-plane database

| Aspect | Decision |
| --- | --- |
| Version | PostgreSQL 16 or later (NFR-OPS3) |
| Authentication | `scram-sha-256`. The reference compose file has no `trust` and no password in the image |
| Roles | A non-superuser application role owning its own schema. The service never needs superuser and must not have it |
| Schema | One schema, one owner. No multi-tenant schema-per-customer, because one customer per instance (ADR-0013) |
| Migrations | Embedded, forward-only, applied at startup, idempotent ([data-model.md](data-model.md#29-migrations)) |
| `search_path` | Set explicitly on connect. Relying on the default makes the service sensitive to a `DATABASE_URL` change |
| Statement timeouts | Set per connection. A hung admin query must time out rather than hold a pool slot |
| Backups | `pg_dump` on a schedule, or the platform's managed backups. **This is the customer's responsibility and the procedure is documented**, not automated (A-19) |

**Backup procedure, in the deployment guide, because it must work for someone who did not write
it:** `pg_dump -Fc` for consistency, restore into a **new** database, point the service at it,
start, then reconcile (§4.2 of [consistency-and-recovery.md](consistency-and-recovery.md)). Never
restore over a live database.

## 8. High availability

| Tier | What it costs | What it needs | Is it supported |
| --- | --- | --- | --- |
| Single instance, datastores on one host | Nothing | — | Yes. The reference |
| Single instance, datastores on separate hosts | Little | Nothing in the product | Yes. Recommended above a certain scale |
| Managed Postgres with failover, single Quotacore | Little | `QUOTACORE_DATABASE_URL` pointing at an endpoint | Yes. Nothing in the product changes |
| Managed Redis with failover, single Quotacore | Little | `QUOTACORE_REDIS_URL` pointing at a sentinel or cluster endpoint | Yes, and **this is the recommended production shape**, because the data store is the availability dependency |
| Two Quotacore instances against one data store | Little | Nothing in the product | Yes. The data plane is stateless; the worker must run on one only, or with `QUOTACORE_WORKER_ENABLED=false` on the other, since the transition is idempotent either way (INV-C2) |
| Multi-region | A great deal | Everything in section 8 of [overview.md](overview.md) that we excluded | **No** |

**The two-instance case deserves a note, because it looks like it should be easy and mostly is.**
The data plane holds no local authoritative state, so a second instance is a valid configuration
and a load balancer in front of both is sufficient. The two things that need care are the reset
worker (idempotent, so running it on both is merely wasteful) and the snapshot cache (per
instance, so propagation is per instance and the documented bound is per instance). No sticky
sessions are needed, and no session state exists to require them.

**Not supported: a rolling upgrade of a single instance.** There is no versioned state format in
the fast store, so an in-place mixed-version deployment would mean two processes with two
different script versions against one key space. The documented procedure is a drain and restart,
which is safe (NFR-D6) and takes seconds.

## 9. Platform support

| Platform | Support | Notes |
| --- | --- | --- |
| Linux x86-64, arm64 | Native | The reference |
| Docker, Podman, any OCI runtime | Yes | — |
| Windows (WSL2, or containers) | Yes, via a container | **Valkey has no native Windows build.** Stated up front so it is not discovered on day one (J-10) |
| macOS (Docker Desktop) | Yes, for development | — |
| Kubernetes | Not shipped | A community chart is welcome with a named maintainer. No first-class manifest in v0.1 |
| systemd, without containers | Not shipped | The binary runs, but the packaging is not maintained |

## 10. Operational responsibilities

Stated explicitly, because an ambiguous responsibility becomes an incident.

| Responsibility | Owner | Enforced how |
| --- | --- | --- |
| Postgres backups and restore testing | **Customer** | Documented procedure; the restore drill is in CI for the *data-store* case and documented for Postgres |
| Host patching, firewall, TLS certificates | **Customer** | Out of scope for this model |
| NTP, and therefore clock accuracy | **Customer** | NFR-D8, DR-001. A clock alert is provided because we cannot fix the host clock |
| Data store availability and memory | **Customer** | Documented sizing, including the idempotency set |
| Migrations | **Quotacore** | Applied automatically at startup |
| Event and audit retention | **Quotacore** | Background job, configurable |
| Ledger completeness | **Quotacore**, to the documented bound | Drop counter, and an honest statement that gaps are possible |
| Key rotation | **Customer** | Supported operation; no forced expiry that would surprise anyone |
| Reconciliation after an incident | **Shared** | The tooling is ours; the decision about a specific tenant is theirs |

## 11. Upgrade and rollback

```bash
# upgrade
docker compose pull
docker compose up -d          # migrations apply, drain, restart
docker compose exec quotacore quotacore verify --sample 50

# rollback
docker compose down
# restore Postgres to a new database from the pre-upgrade dump
# pin the image to the previous tag or digest
docker compose up -d
# reconcile per consistency-and-recovery.md §4.2
```

Migrations are forward-only in production, so a code rollback against a newer schema is expected
to work: additive columns and enum values are ignored by the older binary, and a migration that
removes something is a two-release change for exactly this reason
([data-model.md](data-model.md#6-migration-policy)). The state format in the fast store has no
version, because nothing in it needs one — a key that the older binary does not understand is
still a key it can safely leave alone, and the fast store is rebuildable regardless.

## 12. Deployment checklist

For a production rollout, in order. The first three are the ones people skip.

1. **Clock.** NTP is synchronised and the skew alert is wired. Every boundary is a contract term.
2. **Data store memory.** Sized for the 24-hour idempotency set. A `noeviction` rejection is a `503`
   on the hot path, which is not acceptable; an `allkeys-lru` eviction of an idempotency record is
   a lost replay, which trades a hard `503` against the "charged once" guarantee. Which of those
   two the product is willing to accept at production throughput is
   [Q-21](../product/assumptions-and-open-questions.md#2-open-questions), and the answer changes this
   line. An evicted *balance* hash is neither, and is a P0 requiring the rebuild (DR-045).
3. **Backup and restore.** A restore has actually been performed, not merely configured. The
   procedure is followed by someone who has not read the code.
4. **Key scoping.** A dedicated runtime key per application, not one key for everything, so a leak
   is attributable and revocable (ADR-0012).
5. **TLS.** A real certificate, or a documented decision to terminate elsewhere.
6. **Egress.** Blocked by default, with an explicit exception only for v0.3 webhooks.
7. **Alerts.** The P0 and P1 alerts from [observability.md](observability.md) are wired, with
   runbook links.
8. **Retention.** Event and audit retention set to values the operator has chosen deliberately
   rather than accepted by default (Q-09).
9. **Backpressure policy decided and written down.** What the customer's application does on a
   `503` is their decision, and it should be a decision rather than an accident (DR-037).
10. **Latency verified in situ.** A load test from the customer's network, not from the host. The
    network is frequently the slowest part of the path (J-7).
