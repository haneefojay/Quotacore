# Research: Redis Protocol Compatibility and Licensing

**Date:** 2026-09-27 · **Status:** complete · **Decision:** [ADR-0009](../decisions/0009-redis-protocol-valkey-default.md)

## Question

Can the data plane be written once against a Redis-compatible protocol, which engines qualify,
and what are the licence obligations of each?

## Why it mattered

Two independent reasons, either of which alone would justify the investigation.

**Technical.** Locking the product to one engine means every customer inherits that engine's
availability characteristics, its image's trust, and its upgrade cadence. Supporting a protocol
instead of a product is a materially better dependency posture, and it is only possible if the
commands this design needs are genuinely portable.

**Legal.** A self-hosted component that a customer vendors into a proprietary product drags its
licence into their legal review. Redis changed licence in 2024 and changed it again in 2025. A
metering service whose licence is a moving target is a procurement problem for the customer, and
discovering that during an upgrade is the worst possible time.

## Method

Enumerated the command set this design actually requires, checked each against the protocol
surface rather than against a particular engine, then reviewed the licence of each candidate
engine as of the research date. Where an engine's licence has changed, both the current state and
the version boundary are recorded, because "which licence applies" depends on which version a
customer is running.

## Findings

### The required command surface

This is the actual portability constraint. The list is short, and every entry was chosen
deliberately to stay inside the common subset.

| Command | Used for | Portable? |
| --- | --- | --- |
| `EVALSHA`, `EVAL`, `SCRIPT LOAD` | The atomic transition | Yes. Core to the protocol and the reason this design is portable at all |
| `HGET`, `HSET`, `HMGET`, `HINCRBY` | The balance hash | Yes. Long-standing, no module dependency |
| `GET`, `SET` with `NX` and `EX` | The idempotency record | Yes |
| `EXPIREAT`, `PTTL` | Cycle garbage collection | Yes |
| `PUBLISH`, `SUBSCRIBE` | Configuration invalidation | Yes |
| `PING`, `INFO`, `DBSIZE` | Health and status | Yes |
| `SCAN` | Keyspace inspection, CLI only | Yes |
| `CLUSTER KEYSLOT` | Cluster key-tag verification in tests | Engine-specific enough that it is used only in a test helper, never in the product |

**Commands deliberately not used anywhere in the product**, each because it is either
non-portable or a known operational hazard:

| Command | Why not |
| --- | --- |
| `KEYS` | Blocks the engine on a large key space. An outage waiting to happen, and it appears precisely when someone wants to debug |
| `MULTI`, `WATCH`, `EXEC` | Rejected on the merits in [atomicity-mechanism-options.md](atomicity-mechanism-options.md) |
| Lua library functions beyond the base set | Portability. `cjson` and `cmsgpack` are available in Redis and Valkey but are not a guarantee across the ecosystem |
| Any module or module-loading command | A module is an extension point with its own licence and its own failure modes |
| `SCRIPT DEBUG`, `SCRIPT KILL` | Operational tools that behave differently across engines and are not needed in the product |

### Engine candidates

| Engine | Protocol | Image | Licence (as of 2026-09-27) | Position |
| --- | --- | --- | --- | --- |
| **Valkey** | Redis-compatible | `valkey/valkey:8` | **BSD-3-Clause** | **Default.** A permissive licence maintained as a Linux Foundation project, with the required command surface |
| Redis 7.4–7.8 | Native | `redis:7.4` … | **RSALv2 and SSPLv1** | Supported. Not the default, because both licences are source-available rather than OSI-approved, and a customer vendoring this into a proprietary product would need counsel |
| Redis 8 and later | Native | `redis:8` | **AGPLv3** | Supported. AGPLv3 is OSI-approved and is the most permissive of the Redis licences, but the reciprocal network-use obligation is exactly the kind of question a customer's legal team will have, and it is not our call to make for them |
| KeyDB | Redis-compatible | `keydb/keydb` | Apache-2.0 | Supported where the client library is verified. Multithreaded, which is interesting and irrelevant to a design whose bottleneck is round-trip latency |
| Any Redis-protocol-compatible managed service | Redis-compatible | n/a | **Varies by provider** | Supported. The operator must confirm the provider's licence terms; a managed service's licence is the provider's, not Redis's |

**The Redis licence history, because it is the part that surprises people.** Redis was
BSD-3-Clause until 2024, when it moved to RSALv2 plus SSPLv1 — neither of which is OSI-approved,
and both of which are designed to prevent exactly the use case a hosted Redis competitor represents.
In 2025 Redis moved to AGPLv3, which *is* OSI-approved and includes the network-use reciprocity
that the previous licences approximated. So a customer who upgrades from 7.4 to 8 crosses a
licence boundary twice, and one of those transitions is from "unapproved" to "approved with
reciprocity". Anyone assessing this needs the *version*, not the vendor.

### Why Valkey is the default

1. **BSD-3-Clause is unambiguous.** No reciprocity, no source-available subtlety, no counsel
   required. For a component that will be vendored into someone else's product, that is worth more
   than any feature difference.
2. **The command surface is identical** for everything this design uses. Choosing Valkey costs
   nothing functionally.
3. **The default image is not a legal question for the customer.** A container that pulls
   `valkey/valkey:8` and a container that pulls `redis:8` are the same two lines of YAML, and the
   two images have completely different implications for a legal review. Choosing the permissive
   one by default makes the common case a non-event.
4. **Linux Foundation governance** means the licence and the governance are a matter of public,
   durable record rather than one company's commercial decision.
5. **It is a fork, so it will track Redis's command set**, including anything added later. The
   portability this design depends on is maintained by the fork's existence.

### The cost of supporting a protocol rather than a product

Stated honestly, because "we support anything Redis-compatible" is a promise that can be broken:

| Cost | Reality |
| --- | --- |
| "Redis-compatible" means compatible with *which version* of *which fork* | Real. The engine matrix in the test suite is what makes this claim true, and it must be run on every engine the README mentions |
| An engine can add a command with subtly different semantics | Rare for the base data types, which are frozen by the protocol's history. Possible for Lua behaviour and for eviction timing |
| A managed service can differ from the open-source engine | Real. `INFO` fields, eviction behaviour under pressure, and cluster proxying all vary |
| Failure modes can differ in production despite passing the suite | The main residual risk. Mitigated by a documented support statement that names the tested versions |

The mitigation is a **version matrix in CI** rather than a claim: the integration suite runs
against every engine and version listed as supported, and dropping one from the list is a
deliberate, visible act (A-09).

## What was rejected, and why

| Rejected | Reason |
| --- | --- |
| Redis as the default image | The licence is version-dependent and has changed twice. A metering service's licence must survive an upgrade cycle without a procurement conversation |
| Valkey as the *only* supported engine | The user base has Redis in production, often already managed. Refusing to work with an engine someone already runs is a self-inflicted adoption problem |
| An embedded store (bbolt, SQLite-backed) | Revisited in [ADR-0011](../decisions/0011-postgres-only-control-plane.md). It would remove the Redis licensing question entirely, and it was rejected because atomic scripting is the correctness foundation of the whole design. Worth revisiting only if atomicity requirements change |
| "Any Redis-compatible store, no version list" | An unbounded support claim. A named, tested matrix is a promise; "anything compatible" is a hope |
| KeyDB as default | Apache-2.0 is fine and the multithreading is interesting, but the ecosystem and the long-term governance signals are weaker than Valkey's |

## Decision impact

- [ADR-0009](../decisions/0009-redis-protocol-valkey-default.md): write against the protocol, default
  the image to Valkey, test against a named matrix.
- `QUOTACORE_REDIS_URL` accepts `redis://` and `rediss://` for every supported engine, with no
  engine-specific configuration in the product ([deployment.md](../architecture/deployment.md#6-the-data-store)).
- The `allkeys-lru` recommendation and the idempotency-set memory projection in the deployment
  guide are engine-agnostic but were validated against the default image.
- `SECURITY.md` carries a licence-posture section so the dependency's licence is part of the
  security and compliance review, where it belongs, rather than a line in a container file.

**Update, 2026-09-27 — the `allkeys-lru` recommendation has been reversed.** The memory projection
survives and is engine-agnostic as written, but the eviction policy it assumed does not:
[ADR-0017](../decisions/0017-noeviction-and-duplicate-reversal.md) sets
`maxmemory-policy noeviction` for the whole key space, because the idempotency record inside the
24-hour window is a money guarantee rather than a cache entry (DR-048, DR-049). Nothing in the licence
analysis changes, and the Valkey default is unaffected: `noeviction` is a policy both engines
implement identically, and the engine matrix in [testing-strategy.md](../architecture/testing-strategy.md)
now asserts it rather than asserting eviction behaviour.

## Confidence and what would change this

**High confidence** on the command-portability analysis. The base data types and the scripting
interface are among the most stable parts of the protocol, and this design deliberately avoids
everything that is not.

**Medium confidence, and explicitly time-sensitive, on the licence position.** Licences change;
Redis has changed twice in eighteen months. This document carries a date for that reason, and the
rule is that the licence position is re-verified before any release that changes the default image
or adds an engine to the support list. **Do not rely on this section without re-checking it.**

**Would change the decision:** an engine shipping a script-execution change, a licence change on
any supported engine, or a customer requirement for an engine outside the tested matrix. The first
would undermine the atomicity foundation and force a re-evaluation of
[atomicity-mechanism-options.md](atomicity-mechanism-options.md); the second is a legal event, not
a technical one, and the response would be a documented recommendation to migrate rather than a
silent image change.

## Sources

- Redis licensing announcements and the `LICENSE` file in the `redis/redis` repository, for the
  BSD → RSALv2/SSPLv1 → AGPLv3 sequence and the version at which each applies.
- The Valkey project `LICENSE` and governance documentation, for BSD-3-Clause and Linux Foundation
  governance.
- Redis protocol and command documentation for `EVALSHA`, hash commands, `SET` options, and
  `EXPIREAT`.
- Valkey documentation for the same commands, to confirm the common subset.
- The OSI licence list, for whether a licence is OSI-approved.
- `go-redis/v9` documentation for the `Script` helper, `EVALSHA` and `NOSCRIPT` handling, and for
  its compatibility statement.
