# Quotacore

> Authoritative quota enforcement for usage-priced software. One call on your request path
> answers "may this action proceed, and deduct the cost" — atomically, in single-digit
> milliseconds, and without charging a customer twice.
>
> Self-hosted. No account, no cloud dependency, no telemetry. Apache-2.0.

## Status

| Field | Value |
| --- | --- |
| Stage | Specification complete; `IP-01`, `IP-02`, `IP-03` and `IP-04` closed |
| Version target | v0.1.0 (MVP) |
| Canonical name | Quotacore. "Threshold" is a deprecated alias and must not appear in code, docs or marketing |
| Licence | Apache-2.0 — [LICENSE](LICENSE), decided in [ADR-0014](docs/decisions/0014-apache-2-0-license.md) |
| Specification frozen | 2026-09-27 |
| Owner | Project owner |
| Language and stack | Go 1.27, PostgreSQL 16+, Redis-compatible store (Valkey by default) |
| Decisions | 17 accepted, none superseded |
| Open questions | 21 logged, none blocking — [register](docs/product/assumptions-and-open-questions.md) |

**Current phase.** The specification is complete and internally consistent, and every blocking
question is now answered: [Q-01, Q-02, Q-04, Q-14](docs/product/assumptions-and-open-questions.md#2-open-questions)
and [Q-21](docs/product/assumptions-and-open-questions.md#2-open-questions). Implementation has
started, and deliberately at the bottom rather than the top:
[`IP-01`](docs/roadmaps/v0-1-enforcement-path.md#ip-01--repository-toolchain-and-ci-foundation) built
the repository, the toolchain and the pipeline, contains no product behaviour, and closed on
2026-09-27. [`IP-02`](docs/roadmaps/v0-1-enforcement-path.md#ip-02--the-openapi-contract) then wrote
the contract the code is written against — 39 operations, 67 schemas, 34 error codes, served at
`/openapi.json` with a generated reference at `/docs` — and closed on 2026-09-28.
[`IP-03`](docs/roadmaps/v0-1-enforcement-path.md#ip-03--cycle-engine-and-boundary-matrix), the cycle
engine and its boundary matrix, started and closed on 2026-09-28.
[`IP-04`](docs/roadmaps/v0-1-enforcement-path.md#ip-04--data-plane-skeleton-keyspace-and-snapshot-cache),
the data-plane skeleton, the keyspace and the snapshot cache, started and closed on 2026-09-28 with
all six Definition-of-Done items met; it owns `internal/store`, `internal/snapshot` and
`internal/observability`, and the integration pair that proves NFR-D3 against the real datastore.
The four atomic scripts (`IP-05`) come next. A market assumption is carried as a dated,
owner-accepted release risk
([A-01](docs/product/assumptions-and-open-questions.md#accepted-risks)) rather than a code-start
blocker, because no amount of further specification work would settle it.

## What it is, and what it is not

Quotacore answers one question on someone else's critical path, for money:

> May this quota holder perform this action, and if so, deduct the cost?

It is **not** a billing system, **not** a feature-flag service, **not** an API gateway, and
**not** an analytics product. Those exclusions are load-bearing, permanent, and argued in
[the foundation](docs/product/foundation-and-personas.md#3-what-the-product-is-not) and
[ADR-0005](docs/decisions/0005-refund-as-first-class-operation.md).

## The three claims

Each is a testable property with a test behind it, not a slogan.

| Claim | How it is enforced | How it is proven |
| --- | --- | --- |
| **Atomic by construction** — check and deduct are one indivisible datastore operation | One Lua script execution — [ADR-0002](docs/decisions/0002-lua-scripts-for-atomicity.md) | 200 concurrent requests against a balance of 100, exactly 100 allowed — [T-01](docs/architecture/testing-strategy.md#t-01--atomic-decrement-under-contention) |
| **Correct under retry** — a client that retries is charged once, and a double charge caused by store loss is reversed ([ADR-0017](docs/decisions/0017-noeviction-and-duplicate-reversal.md)) | Required idempotency key, recorded in the same atomic execution — [ADR-0004](docs/decisions/0004-idempotency-prevention-and-detection.md) | 50 concurrent retries, one deduction, byte-identical replies — [T-02](docs/architecture/testing-strategy.md#t-02--idempotent-replay) |
| **Correct under failure** — a missed cycle boundary cannot deny or over-grant | Lazy, monotonic, atomic rollover — [ADR-0003](docs/decisions/0003-lazy-monotonic-cycle-rollover.md) | Idle across three boundaries: one allowance, one event — [T-06](docs/architecture/testing-strategy.md#t-06--missed-boundaries) |

## Reading order

| If you are | Read, in this order |
| --- | --- |
| Evaluating whether to adopt it | [Foundation](docs/product/foundation-and-personas.md) → [competitive landscape](docs/research/competitive-landscape.md) → [J-10, the sceptical evaluator journey](docs/product/user-journeys.md#j-10--migrating-in-p-2-evaluating-seriously) |
| Integrating against it | [API conventions](docs/architecture/api-conventions.md) → [error catalogue](docs/product/error-catalog.md) → [use cases](docs/product/use-cases.md) |
| Building it | [Architecture overview](docs/architecture/overview.md) → [domain rules](docs/product/domain-rules.md) → [data model](docs/architecture/data-model.md) → [cycle engine](docs/architecture/cycle-engine.md) → [request lifecycle](docs/architecture/request-lifecycle.md) |
| Reviewing a change | [Domain rules](docs/product/domain-rules.md) (what must be true) → [testing strategy](docs/architecture/testing-strategy.md) (how it is proven) |
| Assessing risk | [Security model](docs/architecture/security-model.md) → [SECURITY.md](SECURITY.md) → [consistency and recovery](docs/architecture/consistency-and-recovery.md) |
| Deciding what to build next | [MVP scope](docs/product/mvp-scope.md) → [ROADMAP.md](ROADMAP.md) → [ADR-0015](docs/decisions/0015-release-slicing.md) |

## Root documents

| Document | What it is |
| --- | --- |
| [PRD.md](PRD.md) | Requirements, personas, success metrics, scope boundary |
| [ARCHITECTURE.md](ARCHITECTURE.md) | System shape, components, data model, runtime flow |
| [TECHNICAL-DECISIONS.md](TECHNICAL-DECISIONS.md) | Index of all 17 ADRs with rationale and revisit triggers |
| [SECURITY.md](SECURITY.md) | Threat model, controls, gaps, incident response |
| [ROADMAP.md](ROADMAP.md) | Release sequencing and what gates each phase |
| [AGENTS.md](AGENTS.md) | How to work in this repository: reading order by task, change recipes, traps, definition of done |
| [CLAUDE.md](CLAUDE.md) | The same entry point, for agent harnesses that look for that filename |
| [CHANGELOG.md](CHANGELOG.md) | What changed in the specification, and when |
| [LICENSE](LICENSE) | Apache-2.0, canonical text |

## Documentation map

63 documents. `docs/README.md` is the full index with reading paths. `docs/roadmaps/roadmap-index.md`
is the execution plan: 28 phases, with the specification gate now passed.

```
docs/
├── product/       9 documents   what must be true, and for whom
├── architecture/ 12 documents   how it is built and proven
├── decisions/    17 documents   why each choice was made
└── research/      8 documents   what was investigated before deciding

tools/check-docs.ps1  one command, see below
```

Three conventions hold throughout, and violating them is a defect:

1. **A claim with no test is a marketing claim.** Every "verified by" in this repository resolves
   to a named test in [testing-strategy.md](docs/architecture/testing-strategy.md).
2. **Rules are stated once.** Behaviour lives in [domain-rules.md](docs/product/domain-rules.md)
   with a stable `DR-nnn` identifier, and every other document references it rather than
   restating it.
3. **Decisions are immutable.** An accepted ADR is superseded, never edited, and the record of
   what was believed and when is itself evidence.

## Getting started

The first four tasks, in order, each one a phase in
[roadmap-index.md](docs/roadmaps/roadmap-index.md#4-phase-map):

1. ~~Close the four blocking open questions in
   [the register](docs/product/assumptions-and-open-questions.md#2-open-questions).~~ Done on
   2026-09-27, along with the eviction question that sat beside them. `IP-00` is `COMPLETE`.
2. ~~`IP-01`: the repository, the toolchain and the CI pipeline.~~ Closed on 2026-09-27 with all six
   Definition-of-Done items met, after item 4 was amended by its owner so that the base-image half
   of the SBOM obligation moved to `IP-15`, which owns the image. `go.mod` with the module graph
   pinned, the `Makefile` whose targets are the documented steps, the two health probes in
   `internal/api`, embedded up-only migrations in `internal/db`, the service entry point, the licence
   gate and SBOM, and a pipeline that runs the checks in this file. There is no product behaviour in
   it, which a test asserts rather than claims. The steps and the evidence are in
   [the phase section](docs/roadmaps/v0-1-enforcement-path.md#ip-01--repository-toolchain-and-ci-foundation).
3. ~~`IP-02`: the OpenAPI contract.~~ Closed on 2026-09-28 with all seven Definition-of-Done items
   met: `api/openapi.yaml` written from the specification rather than ahead of it, covering 39
   operations, 67 schemas and all 34 catalogue codes; the types, JSON codecs and validation generated
   from it, with no router and no unimplemented handler; the contract served at `/openapi.json`; and
   a self-contained generated reference at `/docs`. The generator, the page and the served document
   are all checked for drift, and the catalogue, matrix, example, header, scope and schema claims in
   [api-conventions.md](docs/architecture/api-conventions.md#13-contract-tests) are enforced by named
   tests. It contains no product behaviour either, and `TestNoProductBehaviour` fails if that ever
   stops being true. The steps and the evidence are in
   [the phase section](docs/roadmaps/v0-1-enforcement-path.md#ip-02--the-openapi-contract).
4. ~~`IP-03`, `COMPLETE` (closed 2026-09-28): the cycle engine and its boundary
   test table — the component most likely to contain a silent permanent defect, which verification
   proved it did: rows 4–7 of the table were wrong and are corrected —
   [cycle-engine.md](docs/architecture/cycle-engine.md). The atomic script and T-01, T-02 and T-09
   follow it; if those three pass, the central argument of the product holds.~~
5. ~~`IP-04`, `COMPLETE` (closed 2026-09-28): the data-plane skeleton, the keyspace and the snapshot
   cache — `internal/store` for every key [data-model.md](docs/architecture/data-model.md) section 3.1
   names, `internal/snapshot` for the bounded cache that enforces against a snapshot, and
   `internal/observability` for the seven collectors. NFR-D3 (a control-plane change visible within
   a second) is proven in CI against a real datastore, NFR-T7 (the snapshot within its memory cap) is
   proven by a runtime measurement, and the two source checks pin the data plane off the control
   plane and off `KEYS`/`SCAN`. The phase contains no product behaviour and no routes; the chaos
   verdicts that make it observable arrive with `IP-07`.~~
6. `IP-05`, `NOT STARTED`: the four atomic scripts (`balance`, `check`, `consume`, `refund`) that
   make a decision and a mutation one indivisible step, embedding the IP-03 cycle transition inside
   the keyspace `IP-04` defined. T-01, T-02 and T-09 sit here, and with them the central argument of
   the product.

Run the specification's own consistency checks at any time. One command verifies every link and
anchor, every identifier reference, that nothing is defined twice, that the inventory in
[docs/README.md](docs/README.md#inventory) matches the documents, and that no file is unindexed:

```powershell
pwsh -File tools/check-docs.ps1        # powershell -File tools/check-docs.ps1 is equivalent
```

It exits 0 when the specification is internally consistent and 1 with a list of defects when it is
not. The rules it enforces are in [docs/README.md](docs/README.md#consistency-rules); the reasons
they exist, and the traps, are in [AGENTS.md](AGENTS.md).
