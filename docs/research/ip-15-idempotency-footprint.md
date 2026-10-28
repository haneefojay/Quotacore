# Research: What an Idempotency Record Costs the Store

**Date:** 2026-10-27 · **Status:** measured · **Decision:** the data-store memory floor in
[deployment.md](../architecture/deployment.md#5-sizing) §5, and the ledger's per-row figure

## Question

How much memory does the 24-hour idempotency window take in the fast store at NFR-T1's sustained
2,000 `consume`/s, and how much disk does a ledger row take in Postgres? Deployment §5 carried a
projection of about 120 bytes a record and a 24 GiB floor, and said itself that the real figure was
plausibly two to three times higher. `IP-15` DoD 6 asks for the measurement.

## Why it mattered

The store runs `noeviction` (DR-048, ADR-0017), so a store too small for the window is a `503` for
every tenant, not a quietly evicted record. Under-sizing costs an availability incident;
over-sizing costs only money. The floor has to be a number someone measured.

## Method

`tools/idemsize` drives the real consume script, through `internal/script`, against a scratch
`valkey/valkey:8` at the digest the reference Compose file pins, with no persistence and
`noeviction`. One tenant-feature has a balance that cannot run out, and every call carries a fresh
`Idempotency-Key`, so every call writes exactly one record; the tool checks that the key count is
the record count plus the one balance hash at every reading.

It reads `used_memory` from `INFO memory` at 1.5 × 2^20 records and three doublings after. Every
reading is the same fraction past a power of two, so the engine's hash tables are at the same point
of their growth each time, and the marginal cost between two readings carries each record's share
of the main and expiry tables as well as its key and value. It runs twice: a UUID's 36 characters,
which is typical, and the contract's maximum of 255.

The projection is the larger marginal cost × 172.8 million records (2,000/s for 24 hours) × 1.25 for
headroom, rounded up to a whole GiB. This measures directly the quantity NFR-T9's 24-hour soak would
measure, without a day on a machine with a hundred GiB.

For the ledger, the tool migrates a scratch Postgres database and writes a million `consumed` rows
shaped as the writer writes them (a request id and a key of a UUID's length, empty metadata), then
reads `pg_total_relation_size`, which counts the heap, every index and TOAST.

## Result

Host: 4 CPUs, linux/amd64, in the `IP-15` build container. Engine `valkey 8.1.11`, allocator
`jemalloc-5.3.0`, policy `noeviction`.

| Key length | Records | `used_memory` | Marginal bytes per record |
| --- | --- | --- | --- |
| 36 | 1,572,864 | 421,468,872 | — |
| 36 | 3,145,728 | 841,204,640 | 266.9 |
| 36 | 6,291,456 | 1,680,737,176 | 266.9 |
| 36 | 12,582,912 | 3,359,801,880 | 266.9 |
| 255 | 1,572,864 | 824,137,136 | — |
| 255 | 3,145,728 | 1,646,508,592 | 522.8 |
| 255 | 6,291,456 | 3,291,329,840 | 522.9 |
| 255 | 12,582,912 | 6,581,060,184 | 522.9 |

`MEMORY USAGE` of one record, which counts its key and value and not the tables, is 240 bytes at 36
characters and 496 at 255. The marginal cost is flat across three doublings, so the projection is a
straight line.

| Window at 2,000/s | Typical keys (36) | Longest keys (255) |
| --- | --- | --- |
| 172.8 million records | 43.0 GiB | 84.1 GiB |
| With 25% headroom | 53.7 GiB | 105.2 GiB |
| **Floor** | 54 GiB | **106 GiB** |

**Ledger.** Postgres 16.15: a million rows take 475,447,296 bytes with indexes and TOAST, 475.4
bytes a row, 0.44 GiB per million events. At 2,000/s that is 76 GiB a day of ledger before
retention, which is what `QUOTACORE_EVENT_RETENTION_DAYS` bounds.

## What it changed

- Deployment §5's data-store floor is **106 GiB**, the longest-key figure, because the contract
  lets a client send 255-character keys and the floor must hold whatever clients send. A deployment
  that controls its clients' keys and knows they are UUIDs may size to 54 GiB, and must alert on
  `quotacore_datastore_memory_used_ratio` either way.
- The old projection's 120 bytes was a value and key alone at a short key; at the contract's
  limits a record is four times that.
- The store's size scales linearly with sustained throughput: at 200/s every figure is a tenth.

## How to repeat it

```text
docker run -d --rm --name idemsize -p 127.0.0.1:16399:6379 valkey/valkey:8@sha256:8f836008e0986c36adf50da714c7060cf689dbb335f61fe077939d90fa650809 \
  --maxmemory 11g --maxmemory-policy noeviction --save "" --appendonly no
createdb quotacore_measure
go run ./tools/idemsize -redis redis://127.0.0.1:16399/0 \
  -database 'postgresql://…/quotacore_measure?sslmode=disable'
docker rm -f idemsize && dropdb quotacore_measure
```

Re-run it on an engine upgrade, and when the record's format changes.

## Sources

- `tools/idemsize`'s output from the run on 2026-10-27, transcribed in the two tables above.
- [NFR-T1](../architecture/non-functional-requirements.md) (2,000 `consume`/s sustained) and
  NFR-T9 (the window measured, not estimated); DR-029 (the 24-hour window).
- The 25% headroom is the rule IP-15's plan set for the floor (D7), not a measurement.
