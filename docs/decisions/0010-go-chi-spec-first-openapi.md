# ADR-0010 — Go, net/http with chi, and a spec-first OpenAPI contract

- **Status:** Accepted
- **Date:** 2026-09-27
- **Deciders:** project owner
- **Affects:** [api conventions](../architecture/api-conventions.md), [research: Go stack](../research/go-stack-and-dependency-selection.md), [ARCHITECTURE.md](../../ARCHITECTURE.md)

## Context

Quotacore is a network service on a latency-sensitive path that must be distributable as a
single binary, embeddable UI, and understandable to outside contributors. Three choices follow
from that: the language, the HTTP layer, and how the API contract is defined.

The previous project history named "fiber/gin" as alternatives and "swaggo/swag" for OpenAPI
generation, without reasoning about any of them.

## Language

Go is not a close call. It compiles to a single static binary with no runtime, embeds static
assets with the standard library, ships excellent profiling and tracing support, has a
first-class concurrency story suited to a service that is almost entirely waiting on a socket,
and is the language the surrounding infrastructure ecosystem — Kubernetes, Docker, Prometheus,
Envoy — is written in, which matters for an infrastructure product's contributors.

Node.js and Python were considered and rejected for the binary and startup characteristics.
Rust was considered and rejected for a different reason: it is a fine choice technically, but
the marginal latency advantage over Go here is dominated by the datastore round trip, while the
barrier to outside contribution is materially higher. This product's credibility depends on
being auditable and patchable by its users.

## HTTP layer

**Options considered**

- **A — `net/http` with the standard library's Go 1.22+ method-pattern routing, plus `go-chi`
  for grouping and middleware composition (chosen).** Full compatibility with every
  `net/http` middleware in existence, including the two that matter most here:
  `promhttp` for metrics and `otelhttp` for tracing. Least machinery, no allocation surprises,
  and stdlib routing has been adequate since 1.22.
- **B — Gin.** The most popular third-party option, familiar to nearly every Go developer, good
  ergonomics. Built on `net/http`, so ecosystem compatibility is retained.
- **C — Fiber.** Built on `fasthttp`, not `net/http`. Faster on benchmarks by a margin that is
  irrelevant when the request is waiting on a socket, but it is not compatible with
  `otelhttp`, `promhttp`, `httptest`-based tooling, or the standard library's context
  cancellation semantics. Rejected on ecosystem grounds, which is the only ground that matters.
- **D — `net/http` alone, no router.** Sufficient at this endpoint count.

**Decision: A.** Chi over D because middleware composition and route grouping will be needed
once the admin surface grows, and chi is a thin wrapper that adds no constraint. The distinction
between A and B was close; chi was chosen because it adds nothing to the dependency surface
that is not already there and keeps request handling visibly standard-library.

## API contract

**Options considered**

- **A — Annotations in Go source, generating OpenAPI (the previous proposal, via `swaggo/swag`).**
  Pros: no separate file, familiar tooling.
  Cons: the schema is derived from comments and struct tags, so a comment and the contract can
  disagree and CI has no independent way to know which is right. Validation still has to be
  written by hand somewhere, which means the derived schema and the real validator can drift.
  Rejected.
- **B — Hand-written OpenAPI 3.1 as the source of truth, with types and validation generated
  from it (chosen).** Request decoding, response encoding and validation all come from one
  artifact. The generated validator is the only validator, so it cannot disagree with the
  published contract. The spec is embedded in the binary and served.
- **C — Protobuf with gRPC.** Rejected: the customer base is Node, Python and Go application
  developers integrating an HTTP API, not gRPC shops. HTTP is also inspectable with `curl`,
  which is a real adoption factor for a self-hosted sidecar.

## Decision

**Go with `net/http` + `chi`. `api/openapi.yaml` (OpenAPI 3.1) is the single source of truth for
the API contract; server types, request validation and response encoding are generated from it;
the spec is embedded and served at `/openapi.json`.**

- **OpenAPI 3.1**, not 3.0, for its correct JSON Schema semantics and its status quo in the Go
  generator ecosystem.
- **The spec is the contract, and CI diffs it.** Any change to a handler that changes a request
  or response shape fails CI unless the spec changed too. This is the specific mechanism that
  prevents the annotation-drift problem in option A.
- **`/docs` is generated from the spec at build time** into a single self-contained HTML page
  with no third-party JavaScript. The alternative, vendoring the Swagger UI distribution, adds
  roughly 1.5 MB of vendored JavaScript to a binary whose entire value proposition is being a
  small, auditable artifact. For the ~20 endpoints in this product a generated reference table
  is more useful than an interactive explorer.
- **Every response is validated against the spec in a round-trip test.** Generated types plus
  golden JSON fixtures.

## Rationale

The generated-validator approach means validation cannot drift from the contract, which is the
only property that makes an OpenAPI spec worth having. Generating the documentation page from
the same spec means there is a third artifact to keep consistent and there isn't.

Chi is chosen over Gin on the principle that the least-surprising implementation wins for a
project whose main risk is nobody being able to maintain it. Stdlib-shaped code is
stdlib-shaped forever; a fasthttp-based framework is a standing compatibility liability.

## Consequences

**Positive**
- One artifact defines the contract, the validation, and the documentation.
- Standard-library HTTP means every middleware, test helper and profiling tool works.
- The binary contains its own contract, so a self-hosted instance is self-documenting with no
  network access and no version-matching problem.
- A small binary with no vendored JavaScript or framework runtime.

**Negative**
- Generated code in the repository, and a build step that must run before the code compiles.
  Mitigated by committing generated output and verifying it in CI.
- Contributors must edit the spec, not the handlers, to change the API. This is a deliberate
  friction that pays for itself the first time a spec and a handler would otherwise disagree.
- OpenAPI 3.1 support in tooling is less universal than 3.0. The spec is also published as
  generated JSON, which is what SDK generators consume.
- No interactive "try it" API console. A deliberate trade for a small, dependency-free binary.

## Revisit when

- The endpoint count makes hand-maintaining the spec painful enough that a designer-first tool
  is warranted.
- gRPC or a streaming interface becomes a real customer requirement, for example a
  high-throughput event-ingestion path.
