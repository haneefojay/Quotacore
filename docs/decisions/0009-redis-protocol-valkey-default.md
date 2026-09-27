# ADR-0009 — Speak the Redis protocol, ship Valkey by default

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [deployment](../architecture/deployment.md), [SECURITY.md](../../SECURITY.md), [research: licensing](../research/licensing-redis-vs-valkey.md)

## Context

The data plane requires a Redis-compatible store with Lua scripting (ADR-0002). Choosing which
one to build and distribute against is a decision with two independent axes: technical
compatibility, and licence compatibility. The previous project history got the licence
narrative right for 2024 and then carried it forward unchanged, so it is worth stating
precisely where things stand.

**Licence history.** Redis was BSD-3-Clause up to and including 7.2. In March 2024 Redis Ltd.
moved 7.4 onwards to a dual RSALv2/SSPLv1 licence, neither of which is OSI-approved. The
Linux Foundation fork **Valkey** was created from 7.2.4 and remains BSD-3-Clause. In May 2025
Redis returned to OSI-approved open source by adding **AGPLv3** as a third option for **Redis
8 and later**; 7.4 through 7.8 remain RSALv2/SSPLv1 only. As of this writing Valkey is at 9.x.

**Technical reality.** Valkey is a fork of Redis 7.2.4. It speaks the same RESP protocol,
supports the same Lua scripting engine, and the Go client is protocol-level. Redis 8 and
Valkey 9 have both added features, but none of them are features this product uses — the data
plane needs `EVALSHA`, `SCRIPT LOAD`, hash and string operations, and optionally sorted sets
from v0.2. Every one of these has been stable since Redis 5.

## Options considered

**A — Hard-require Valkey.** Reject any non-Valkey store.
- Pros: one supported target, one licence story, one image in the quick start.
- Cons: refuses customers who already have Redis or ElastiCache, or who cannot adopt a new
  component because of internal policy. Rejects a working deployment for a licensing question
  that protocol compatibility already dissolves. Rejected.

**B — Hard-require Redis.** Refuse Valkey.
- Cons: the licence situation for Redis 7.4–7.8 is worse for many enterprises than the BSD
  Valkey fork, and recommending Redis is recommending a licence that some corporate policies
  explicitly ban. Rejected.

**C — Protocol-compatible, Valkey by default (chosen).** The application uses a standard Redis
  client and makes no brand check. The shipped `docker-compose.yml` and documentation use
  `valkey/valkey`, with a one-line comment showing how to substitute `redis:8`.

## Decision

**Option C.**

- **No brand detection and no brand check in code.** The application connects to whatever is at
  the configured address and verifies at startup that script execution works, by loading a
  probe script and executing it. If scripts do not work, the instance refuses to become ready
  rather than serving enforcement requests it cannot make atomic. This turns a licence and
  configuration mistake into a startup failure instead of a silent correctness hole.
- **Default distribution is Valkey**, in the quick-start compose file, the Dockerfile
  documentation and every recommendation, because BSD-3-Clause is the most permissive option
  available and cannot be banned by a corporate policy.
- **Redis 8+ is documented as fully supported.** Redis 8 is OSI-approved under AGPLv3, and the
  feature set this product uses is identical. The documentation notes the AGPLv3 obligations
  for customers whose policy requires a review, rather than leaving them to discover it.
- **Redis 7.4–7.8 is supported but flagged**, with an explicit note that those versions carry
  RSALv2/SSPLv1 and are not OSI-approved, so the customer's licence review may reject them.
- **The Go client is `github.com/redis/go-redis/v9`**, the official Redis client, chosen over
  `rueidis` for ecosystem familiarity. See the validation gate in
  [performance-budget](../architecture/performance.md): if the p99 gap at our actual
  traffic profile exceeds the margin stated there, the client is revisited.

## Rationale

The application only needs a subset of the protocol that has been stable for years, and both
implementations provide it. Treating the fork choice as an architectural commitment buys
nothing and costs adoption, because the customer almost certainly already runs one of these and
will not add a second one for us. The previous history's conclusion — build for the protocol,
not the brand — was right, and this ADR keeps it while correcting the stale licence reasoning
and adding two things the previous version lacked: an explicit supported-version matrix, and a
startup probe that turns an unsupported store into a startup error.

Using the official client rather than the faster alternative is a judgement about maintenance
and review burden over microseconds of client-side overhead, since the dominant cost on this
path is a network round trip. The decision is fenced with a measurement so it is revisited on
evidence rather than on taste.

## Consequences

**Positive**
- Customers use the store they already have, including ElastiCache, MemoryDB, and serverless
  offerings that speak the protocol.
- The default distribution has the most permissive licence available, so the project never
  asks a customer to take on a licence their legal team will reject.
- An unsupported or misconfigured store fails at startup, loudly, before any enforcement
  request is served.

**Negative**
- Every supported combination is a compatibility surface. The feature set is deliberately tiny
  to keep it small, and a startup probe catches the rest, but the CI matrix has to cover at
  least Valkey and Redis 8.
- Customers on 7.4–7.8 carry a licence we have flagged as problematic. Documented, with a
  recommended version, rather than refused.
- Supporting two implementations means an operator can choose a version whose script semantics
  differ in a corner we have not tested. The startup probe plus a CI matrix across both
  implementations is the mitigation, and it is a permanent CI cost.

## Revisit when

- The data plane needs a feature that only one implementation provides, which would end the
  protocol-agnostic stance.
- A store with a materially better consistency or durability story appears that still supports
  `EVALSHA`, in which case the default would change while protocol compatibility is retained.
