# Research: Go Runtime and HTTP Stack Selection

**Date:** 2026-09-27 · **Status:** complete · **Decision:** [ADR-0010](../decisions/0010-go-chi-spec-first-openapi.md), [ADR-0011](../decisions/0011-postgres-only-control-plane.md)

## Question

Which Go HTTP stack, spec-first code generation approach, database access library, migration tool
and data-store client should the service use, given that the correctness argument depends on the
data-store client and the API contract depends on the specification?

## Why it mattered

Two of these choices are load-bearing rather than conventional. The **data-store client** is on
the path where the entire atomicity argument lives: a client that retries a script automatically,
or that pipelines in a way that reorders commands, would invalidate the correctness reasoning.
The **specification toolchain** determines whether the served contract and the documentation can
diverge, which is the most likely long-term source of documentation rot.

## Method

Set hard requirements first, from the architecture, then evaluated each candidate against the
requirements rather than against popularity. Where a requirement eliminated all candidates for a
category, that is recorded as a finding rather than hidden.

## Findings

### Hard requirements

| # | Requirement | Source |
| --- | --- | --- |
| R1 | One round trip per enforcement request, with no automatic client-side retry of a script | [atomicity research](atomicity-mechanism-options.md) |
| R2 | Correct `EVALSHA` and `NOSCRIPT` handling, including under a connection pool | Same |
| R3 | A static binary, no cgo | NFR-OPS6 |
| R4 | Embedded time-zone data, no system zone database | NFR-OPS8, DR-001 |
| R5 | The served OpenAPI document and the generated validation come from one source | ADR-0010 |
| R6 | Migrations embedded in the binary, no separate step | NFR-OPS3 |
| R7 | A bounded credential cache with per-request constant-time verification | [security model](../architecture/security-model.md#3-authentication) |
| R8 | Per-request timeouts, including one shorter than the request timeout | NFR-L4 |

### HTTP stack

| Candidate | Verdict | Reasoning |
| --- | --- | --- |
| `net/http` + `go-chi/v5` | **Chosen** | The standard library's server is the reference implementation and has the best-known behaviour under load. `go-chi` adds exactly the routing and middleware composition this design needs — nested route groups for the plane split, per-route middleware, and a small dependency footprint — and nothing that gets in the way. No framework between the socket and the handler means the request path is inspectable |
| `gin` | Rejected | Popular, and built on `net/http`, so it is not faster. It adds a context-based API that makes it easy to accidentally pass a mutated context into a goroutine, which is a class of bug this design cannot tolerate |
| `echo` | Rejected | Similar profile to `gin`. No advantage found for this requirement set |
| `fiber` | Rejected | Built on `fasthttp`, not `net/http`. Faster in microbenchmarks, and therefore a different set of semantics for context values, `net/http` middleware, and anything in the standard library's ecosystem. Rejected because compatibility with the standard library is worth more than throughput we do not need — the bottleneck is a network round trip |
| A full framework (Fiber, Buffalo, Beego) | Rejected | Each substitutes its own request handling for the standard library's. This is a service whose correctness argument is about ordering and atomicity, and opaque request handling is the wrong place to accept that trade |

**The deciding argument is not performance, it is transparency.** The whole design is "one round
trip, one script execution, in a known order". A framework that inserts itself between the socket
and the handler makes that harder to verify, and the performance difference is irrelevant when the
dominant cost is a Redis round trip.

### Spec-first code generation

| Candidate | Verdict | Reasoning |
| --- | --- | --- |
| `ogen` | **Chosen** | Generates Go types and request validation from OpenAPI 3.1, produces no runtime dependency beyond the standard library, generates a matching specification for TypeScript and Python, and — decisively — can emit its own specification. That last property is what makes the "the served document is the source of truth" claim in ADR-0010 true rather than aspirational: the document at `/openapi.json` is byte-identical to the file in the repository, provably, in CI |
| `oapi-codegen` | Rejected | Also good, but it does not generate TypeScript and Python, which would mean two more hand-maintained clients for the SDK releases and therefore two more chances for the contract to diverge |
| Hand-written types plus a validation library | Rejected | Two hand-maintained representations of one contract. This is the most common way an OpenAPI-first project rots: the spec is the documentation, the types are the truth, and they disagree within two releases |
| `swagger-codegen` and similar | Rejected | Java-centric, heavyweight, and would add a JVM to a Go build for no benefit |

**R5 is the requirement that decided this.** It is easy to adopt spec-first tooling and still end
up with a document that has drifted from the binary, and then the drift is invisible until a
customer files a bug against documentation that was never accurate.

### Database access and migrations

| Candidate | Verdict | Reasoning |
| --- | --- | --- |
| `pgx/v5` | **Chosen** | The performance-oriented driver, with a native interface, a `pgxpool` for connection pooling, explicit statement timeouts, and first-class context cancellation. R7 and R8 need per-statement timeouts, and `pgx` makes that a connection parameter rather than a per-query escape |
| `database/sql` with `lib/pq` | Rejected | The interface is fine but `lib/pq` is in maintenance mode, and the context and timeout story is weaker |
| `sqlc` | Rejected for v0.1 | Genuinely attractive, and the right answer for a large query surface. The control plane's query count is small enough that generated query code adds a build step without removing much. Revisit if the query surface grows |
| GORM, Ent | Rejected | An ORM in the control plane means a schema-shaped API over a schema that is also a documented customer interface. Explicit SQL keeps the schema authoritative and reviewable |
| `goose` (embedded) | **Chosen** | Embeddable, forward-only capable, single-transaction by default, and it satisfies R6 with no extra process or container. A separate migration tool would add a step to the quickstart, and the quickstart is a product requirement (NFR-OPS1) |
| `atlas` or `migrate` (external) | Rejected | Both would require the operator to run something before the first start |

### Data-store client

| Candidate | Verdict | Reasoning |
| --- | --- | --- |
| `go-redis/v9` | **Chosen** | `Script.Run` handles `EVALSHA`, the `NOSCRIPT` reload and the one retry, correctly and without a global hook. `redis.Nil` is a distinct error, which is what makes "key absent" expressible in the script-calling code. Context cancellation propagates. It is the most widely deployed client for this protocol, so the compatibility surface is broad |
| `rueidis` | Rejected, with a reservation | Reported to have better tail latency and lower allocation, which is attractive. Rejected for v0.1 because the correctness argument depends on the client's script and retry behaviour being demonstrably correct, and `go-redis` is the more thoroughly exercised implementation of exactly those semantics. **This is the most likely candidate for a future performance revisit**, and it should be re-evaluated once a real benchmark exists rather than on reported figures |
| `redigo` | Rejected | Older API, no context in its core signatures, and the ecosystem has moved on |
| A hand-written RESP client | Rejected | It would be small, and it would be entirely ours to get wrong on the one path where being wrong costs money |

**R1 and R2 are what make this the most carefully evaluated choice in the document.** A client that
transparently retried a failed `EVALSHA` would be retrying a mutation, and a client that pipelined
the idempotency write separately from the script would have broken the atomicity guarantee
invisibly. Both behaviours are now asserted in tests, so a future client upgrade that changes
them fails the build rather than production.

**On `EVALSHA` and connection pooling, specifically.** Because the script is stored in a
server-side cache and — since Redis 7 — in a shared, refcounted cache rather than per connection,
a pooled client does not pay a re-`EVAL` per connection. This was verified against the engine
matrix rather than assumed, because getting it wrong would mean paying a full script transfer on a
large fraction of requests, which would be the single largest performance defect available
([performance.md](../architecture/performance.md#2-what-the-script-costs)).

### Standard library only, elsewhere

| Need | Decision | Reason |
| --- | --- | --- |
| Logging | `log/slog`, structured JSON | In the standard library since 1.21, structured, and one fewer dependency. Meets the log requirements in [observability](../architecture/observability.md#2-structured-logs) |
| Time zones | `time/tzdata`, embedded | NFR-OPS8 |
| Randomness | `crypto/rand` | API keys are credentials; a predictable generator would be a P0 |
| Password hashing | `argon2.IDKey` from `x/crypto` | The maintained Argon2 implementation, and ADR-0012 requires Argon2id |
| Metrics | Prometheus `client_golang` | The ecosystem expects it, and the exposition format is de facto |
| Tracing | OpenTelemetry, optional | Off by default; the `request_id` is the correlation key and tracing is not a dependency |
| UUID | `github.com/google/uuid` or a small local v7 implementation | UUIDv7 for index locality. Trivial enough that a dependency may not be worth it |
| Configuration | Environment variables with a small typed reader | A configuration library would be larger than the configuration |

## What was rejected, and why

| Rejected | Reason |
| --- | --- |
| `fiber`/`fasthttp` | Microbenchmark throughput is irrelevant when the dominant cost is a Redis round trip, and it forfeits `net/http` compatibility |
| `gin`, `echo` | No performance advantage over `chi` with `net/http`, and a more error-prone request model |
| `oapi-codegen` | Does not generate the TypeScript and Python clients the SDK releases need |
| Hand-written types with a validation library | Two representations of one contract, and therefore a guaranteed drift |
| `database/sql` with `lib/pq` | Maintenance mode; weaker timeout and context story |
| GORM, Ent | An ORM over a schema that is also a customer-facing documented interface |
| `rueidis` | Possibly faster, less thoroughly exercised for the script and retry semantics the correctness argument depends on. Revisit with a real benchmark |
| `redigo` | Older API, no context in core signatures |
| An external migration tool | Adds a step to the quickstart, which is a product requirement |
| `sqlc` | Not enough query surface in v0.1 to justify the build step |

## Decision impact

- [ADR-0010](../decisions/0010-go-chi-spec-first-openapi.md) and
  [ADR-0011](../decisions/0011-postgres-only-control-plane.md).
- The single-round-trip and `NOSCRIPT` behaviours are asserted in the test suite, so a dependency
  upgrade that changes them fails CI ([testing-strategy](../architecture/testing-strategy.md#4-script-tests)).
- The specification round-trip test is a contract test, so the served document and the repository
  document cannot differ.
- `slog` gives the structured-log requirements with no additional dependency, and
  `time/tzdata` gives the time-zone guarantee with no host dependency.
- The `rueidis` reservation is recorded so a future performance investigation has a starting point
  and does not re-derive the client's script semantics from scratch.
- The dependency count is small and every entry is justified by a requirement in the table above,
  which is the standard that keeps a Go service's supply chain reviewable (NFR-S7).

## Confidence and what would change this

**High confidence** on `net/http` + `chi`, `pgx`, embedded `goose` and the standard-library logging
choice. These are settled, widely used, and each maps to a specific requirement.

**Medium confidence** on the code generator. The decisive property — round-tripping the
specification and proving the served document matches the repository — is what should be tested
first, before any other generator feature is relied on. If `ogen` cannot round-trip a document that
uses the OpenAPI 3.1 constructs this API needs, that is a finding to establish in the first week of
implementation, not in the third month.

**Would change the data-store client:** a benchmark showing a p99 materially above NFR-L2 that
traces to client overhead rather than to the round trip. That is the specific evidence that would
justify re-evaluating `rueidis`, and it should be gathered before the client is ever described as
a bottleneck.

**Would change the HTTP stack:** nothing foreseeable. If a requirement ever demanded sub-millisecond
server-side processing on a burst several times the sustained 2,000 `consume`/s in NFR-T1, `fasthttp`
would become worth evaluating - and that requirement would deserve more scepticism than the
performance figure suggesting it.

## Sources

- Go 1.27 standard library documentation for `net/http`, `log/slog`, `time`, `time/tzdata`,
  `crypto/rand` and `embed`.
- `go-chi/v5`, `gin`, `echo` and `fiber` documentation, for middleware composition, context model
  and `net/http` compatibility.
- `ogen` documentation for OpenAPI 3.1 support, generated-dependency surface, and specification
  round-tripping.
- `pgx/v5` and `pgxpool` documentation, for context cancellation, statement timeouts and pooling.
- `goose` documentation for embedded migrations and transactional behaviour.
- `go-redis/v9` documentation for `Script`, `EvalSha`, `NOSCRIPT` handling and `redis.Nil`; and
  `rueidis` documentation for the comparison, including its own compatibility statement.
- `x/crypto/argon2` documentation.
- Prometheus `client_golang` and OpenTelemetry Go documentation.
