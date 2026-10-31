# v0.5 — client libraries

Quotacore · 2026-09-27

Two phases, `IP-26` and `IP-27`. Generated from the contract, with a hand-written thin layer on top
whose only real job is to make the retry pattern correct by default.

Read [roadmap-index.md](roadmap-index.md) first.

## Phase map

| ID | Phase | Depends on | Status |
| --- | --- | --- | --- |
| `IP-26` | TypeScript SDK | `IP-16` | `COMPLETE`, unpublished until v0.2 ([Q-32](../product/assumptions-and-open-questions.md#2-open-questions)) |
| `IP-27` | Python SDK | `IP-16` | `BLOCKED` |

---

## The job of these libraries

The [mvp-scope.md](../product/mvp-scope.md) §2 description is short and the work is not: an SDK that
generates a client is an afternoon, and an SDK that makes claim 2 true for its users is the reason
this release exists.

| The user must not | Therefore the SDK must |
| --- | --- |
| Write their own retry loop and duplicate a charge | Generate an idempotency key per logical operation and reuse it across retries (DR-026) |
| Retry a `400` forever | Retry only on `5xx` and `429 rate_limited`, with bounded attempts and jitter |
| Guess why a call failed | Expose typed errors matching the catalogue, each carrying the code's client action |
| Abandon a request on a timeout and never learn its outcome | Treat a transport failure as unknown, and resolve it with a `check` on the same key, because the answer may already be recorded (DR-027). Read as the same request on the same key, by the 2026-10-31 amendment to `IP-26` item 6 |
| Hand-roll pagination | Implement the contract's pagination and follow `next` |
| Lose the trace in a bug report | Surface `X-Request-Id` on every response and every error (NFR-S10) |

The fourth row is the one that is easy to get wrong and expensive to get wrong. A client that
generates a **new** idempotency key on each retry has defeated the entire mechanism, while looking
like it implements it.

---

## IP-26 — TypeScript SDK

**Status** `COMPLETE`. Opened and closed 2026-10-31, 10 of 10 DoD items met, with item 6 read as
amended below. The package is in [sdk/typescript/](../../sdk/typescript/README.md) and is not
published until v0.2's contract has landed ([Q-32](../product/assumptions-and-open-questions.md#2-open-questions)).

> **Opened 2026-10-31, before v0.2.** The phase map gates `IP-26` on `IP-16` alone, while
> [ADR-0015](../decisions/0015-release-slicing.md) places v0.5 after v0.2 because a package published
> against a moving API is permanent maintenance. The ADR's objection is to publication, so the owner
> chose to build and test the SDK now and publish it after v0.2 (Q-32). The ADR stands unchanged.

**Objective.** A published package that a Node or browser customer can install, authenticate with,
and use correctly without reading the documentation twice.

**Scope.**
- A client generated from `api/openapi.yaml`.
- A hand-written layer: authentication, the retry policy, typed errors, pagination, and
  `X-Request-Id` capture.
- Idempotency key generation and reuse across retries, per logical operation.
- Runtime and type definitions, a README that compiles, and runnable examples.

**Specification references.**
- [mvp-scope.md](../product/mvp-scope.md) §2, v0.5
- [api-conventions.md](../architecture/api-conventions.md)
- [error-catalog.md](../product/error-catalog.md), for the typed-error surface
- [ADR-0010](../decisions/0010-go-chi-spec-first-openapi.md), the contract as the source
- [DR-026](../product/domain-rules.md), [DR-027](../product/domain-rules.md),
  [DR-040](../product/domain-rules.md), NFR-S10
- [idempotency-and-retries.md](../research/idempotency-and-retries.md)

**Dependencies.** Blocked by `IP-16`. Blocks nothing; `IP-27` does not depend on it.

**Definition of Done.**

1. The generated client and the hand-written layer are separate directories, and the boundary is
   documented, so a contract change regenerates without a merge conflict in hand-written code.
2. A retry reuses the same idempotency key, and a test fires a transport failure after the server
   has applied the mutation, retries, and asserts one deduction and one stored response
   (DR-027, and the T-02 property).
3. A test fires a retry where the key **would** have been regenerated, and asserts the mechanism is
   defeated. The test proves the SDK cannot be misused by accident, which is the actual risk.
4. Retries happen only on `5xx` and `429 rate_limited`, never on `4xx`, with bounded attempts and
   jitter, and a test asserts a `400` is returned on the first attempt (DR-040).
5. Every catalogue code maps to a typed error carrying the code and the catalogue's client action,
   and a test asserts the mapping is complete in both directions.
6. A transport failure surfaces as an explicitly unknown outcome, and the SDK offers the `check` on
   the same key that resolves it. Tested against a server that applied the mutation and then dropped
   the connection.

   > **Amended 2026-10-31 (`IP-26`).** `POST /v1/check` takes no `Idempotency-Key` (DR-024,
   > [api-conventions.md](../architecture/api-conventions.md#72-check) §7.2), so no `check` can be
   > on the same key, and the events filter on `idempotency_key` needs an `admin` key a runtime caller
   > does not hold. What resolves the outcome with the caller's own key is the job table's own
   > citation, DR-027: the identical request with the identical `Idempotency-Key`, which replays an
   > applied mutation and applies an unapplied one once. `UnknownOutcomeError.resolve()` sends it,
   > and refuses once 23 hours have passed since the first attempt, because after DR-029's window
   > the same key is a new operation. The test is unchanged.
7. Pagination follows the contract, and a test pages to exhaustion and reassembles the set.
8. `X-Request-Id` is on every response object and on every error, so it can be pasted into a bug
   report (NFR-S10).
9. The package's examples in the README are executed as tests in CI, so a stale example fails the
   build.
10. The package installs and passes its own test suite on the current long-term-support version of
    Node, and the version range is stated.

**Exit criteria.** A customer who follows the quickstart gets a correct retrying client without
having thought about idempotency at all.

**Deferred.** A browser bundle with a different fetch layer, and edge-runtime compatibility.

---

## IP-27 — Python SDK

**Status** `BLOCKED`, gated on `IP-16`.

**Objective.** The same guarantees, with the ergonomics a Python customer expects, and the same
resistance to being misused.

**Scope.**
- A client generated from `api/openapi.yaml`.
- A hand-written layer mirroring `IP-26`'s guarantees.
- Idempotency key generation and reuse, including for a retried call in a `with` block.
- Both synchronous and asynchronous clients, from the same generated core.
- Type hints, a README that is executed as a test, and runnable examples.

**Specification references.**
- [mvp-scope.md](../product/mvp-scope.md) §2, v0.5
- [api-conventions.md](../architecture/api-conventions.md)
- [error-catalog.md](../product/error-catalog.md)
- [DR-026](../product/domain-rules.md), [DR-027](../product/domain-rules.md),
  [DR-040](../product/domain-rules.md), NFR-S10

**Dependencies.** Blocked by `IP-16`. Nothing depends on it.

**Definition of Done.**

1. Every guarantee in `IP-26`'s DoD holds here too, and the tests are the same tests ported rather
   than re-thought. A shared list of the guarantees exists, and both SDKs' test suites reference it,
   so a guarantee cannot be added to one and forgotten in the other.
2. The synchronous and asynchronous clients behave identically, and a test runs the same guarantee
   set against both.
3. An idempotency key survives a retry inside a `with` block, including when the exception is raised
   by the transport rather than by the server. Tested.
4. The package installs from a wheel on the current supported Python versions, and the versions are
   stated.
5. No third-party runtime dependency beyond what the generated client needs, and the dependency
   list is reviewed for licence compatibility with NFR-C4 before publication.
6. The README examples are executed as tests in CI.

**Exit criteria.** The same five guarantees as `IP-26`, with the same tests, in the language a
Python customer uses.

**Deferred.** Framework-specific integrations, a Django management command, and an IPython helper.
