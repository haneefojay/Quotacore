<div align="center">

# Quotacore

**Authoritative quota enforcement for usage-priced software.**

One call on your request path answers "may this action proceed, and deduct the cost" — atomically,
in single-digit milliseconds, and without charging a customer twice.

[Apache-2.0](LICENSE) · self-hosted · no account · no telemetry · no egress

</div>

---

## What it is

You sell something whose cost scales with use. You need to know, on every request, whether the
customer is still within what they paid for — and you need that answer to be exactly right, because
being wrong costs real money in one direction and real goodwill in the other.

Quotacore holds the rules. Your application makes one call per metered action.

```
your service ──▶ POST /v1/consume ──▶ Quotacore ──▶ 200, balance 419 left
                 {"tenant_external_id":"acme",          {"allowed":true,
                  "feature_key":"llm.tokens",           "balance":419,
                  "amount":81}                           "consumed":81}
```

The check and the deduction are one indivisible operation. A monthly allowance resets on a calendar
boundary in the tenant's own time zone, computed on demand — so nothing depends on a job that did
not run, on a server that was down at midnight, or on adding 30 days to a timestamp across a
daylight-saving change.

## The three claims, and the tests behind them

| Claim | Enforced by | Proven by |
| --- | --- | --- |
| Check and deduct are one indivisible operation | One Lua script execution | 200 concurrent requests against a balance of 100, exactly 100 allowed |
| A retrying client is charged once; a double charge caused by store loss is reversed | Idempotency recorded in the same atomic execution, and a no-eviction store sized for the window | 50 concurrent retries, one deduction, byte-identical replies — then the T-14 store-loss case, one reversal |
| A missed cycle boundary cannot deny or over-grant | Lazy, monotonic, atomic rollover | Idle across three boundaries: one allowance, one event |

Full test list: [testing-strategy.md](docs/architecture/testing-strategy.md).

## Not this

No invoicing, no payments, no feature flags, no API-gateway rate limiting, no analytics, no PII, no
accounts, no phone-home. The reasoning is in
[the foundation](docs/product/foundation-and-personas.md#3-what-the-product-is-not) and it is
permanent, not a roadmap item.

## Status

**Specification complete; implementation started.** 63 documents, 17 accepted
decisions, 49 domain rules, 34 error codes, 15 named correctness tests. Every blocking question is
answered — the four former blockers plus
[Q-21](docs/product/assumptions-and-open-questions.md#2-open-questions), the eviction question that
turned out to be a trade-off between two of the three claims. What remains open is listed in
[the register](docs/product/assumptions-and-open-questions.md#2-open-questions) and none of it blocks;
one of them, the market bet behind the whole product, is a dated owner-accepted release risk. The
order of the work is [28 phases in `docs/roadmaps/`](docs/roadmaps/roadmap-index.md).
[`IP-01`](docs/roadmaps/v0-1-enforcement-path.md#ip-01--repository-toolchain-and-ci-foundation) built
the repository, the toolchain and the CI foundation and closed on 2026-09-27: the module graph
pinned, the two health probes, embedded up-only migrations, a service that starts and reports
not-ready, a licence gate and an SBOM. It contains no product behaviour, which a test asserts rather
than claims. [`IP-02`](docs/roadmaps/v0-1-enforcement-path.md#ip-02--the-openapi-contract) then
wrote the contract and closed on 2026-09-28: 39 operations, 67 schemas and all 34 catalogue codes,
the Go types generated from them, the contract served at `/openapi.json` and a generated reference at
`/docs`. It also contains no product behaviour — an enforcement call still returns `404`, because
`IP-07` owns the routes — and a test asserts that too. [`IP-03`](docs/roadmaps/v0-1-enforcement-path.md#ip-03--cycle-engine-and-boundary-matrix),
the cycle engine and its boundary matrix, started and closed on 2026-09-28: the pure calendar
function that every cycle boundary in the product rests on, with the Go time-zone database embedded
in the binary and the 20-row boundary table, the daylight-saving pair, a property test and the
index-monotonicity tests as its proof. Verification caught and fixed two defects, both recorded in
the phase section: rows 4–7 of the boundary table were wrong, and the committed `go.mod`/`go.sum`
were never tidy. [`IP-04`](docs/roadmaps/v0-1-enforcement-path.md#ip-04--data-plane-skeleton-keyspace-and-snapshot-cache),
the data-plane skeleton, the keyspace and the snapshot cache, started and closed on 2026-09-28: the
key layout from [data-model.md](docs/architecture/data-model.md) as tested builders, the bounded
snapshot cache with a token-limited miss path, and the invalidation channel that makes a committed
control-plane change visible within one second (NFR-D3, proven in CI against a real datastore).
Still no product behaviour and no route: the data plane is the skeleton, and `IP-07` owns the
enforcement calls.

Start with [PROJECT.md](PROJECT.md), then follow the reading order it gives you.

## Documentation map

| Area | Count | Start here |
| --- | --- | --- |
| Root | 11 | [PROJECT.md](PROJECT.md) — orientation, status, and what to read next |
| Product | 9 | [foundation-and-personas.md](docs/product/foundation-and-personas.md) |
| Architecture | 12 | [overview.md](docs/architecture/overview.md) |
| Decisions | 17 | [TECHNICAL-DECISIONS.md](TECHNICAL-DECISIONS.md) |
| Research | 8 | [competitive-landscape.md](docs/research/competitive-landscape.md) |

The complete index, with what each document settles and who should read it, is
[docs/README.md](docs/README.md).

```
.
├── README.md              this file
├── PROJECT.md             what this is, status, reading order
├── PRD.md                 requirements, metrics, scope boundary
├── ARCHITECTURE.md        system shape, components, runtime flow
├── TECHNICAL-DECISIONS.md index of all 17 ADRs
├── SECURITY.md            threat model, controls, known gaps
├── ROADMAP.md             v0.1 → v0.5, gates, and what is deliberately late
├── AGENTS.md              how to work in this repository, and the traps
├── CLAUDE.md              the same entry point, for other agent harnesses
├── CHANGELOG.md           what changed in the specification, and when
├── LICENSE                Apache-2.0
├── Makefile               the documented steps, and the ones CI runs
├── docker-compose.yml     the fast store and Postgres, and nothing else
├── go.mod, go.sum         the module graph, pinned
├── api/                   the contract, and everything generated from it
│   ├── openapi.yaml       THE contract: 39 operations, 67 schemas, 34 codes
│   ├── docs.html          the generated reference, served at /docs
│   └── oas_*_gen.go       generated types, codecs and validators. Do not edit
├── cmd/quotacore/         the service entry point
├── internal/api/          the router, the probes, and the served contract
├── internal/db/           embedded up-only migrations
├── tools/
│   ├── check-docs.ps1     links, anchors, identifiers, inventory
│   └── gendocs/           renders openapi.yaml into docs.html
├── .github/workflows/     the pipeline that runs the checks below
└── docs/
    ├── README.md          full index and conventions
    ├── product/           what must be true, and for whom
    ├── architecture/      how it is built and proven
    ├── decisions/         why each choice was made
    ├── research/          what was investigated before deciding
    └── roadmaps/          the phases, and what proves each one finished
```

## Contributing

The specification is the product right now, so the rules are about consistency rather than style.
Read [AGENTS.md](AGENTS.md) first; it has the reading order by task, the change recipes, and the
checks below in full.

- **Behaviours are stated once**, in [domain-rules.md](docs/product/domain-rules.md) with a stable
  `DR-nnn` identifier. Reference it; do not restate it.
- **A claim with no test is a marketing claim.** Every assertion resolves to a named test.
- **Accepted ADRs are immutable.** Supersede them. The record of what was believed, and when, is
  evidence.
- **State the sharp edges.** If a design has a residual risk, it goes in
  [SECURITY.md](SECURITY.md#6-known-gaps), not in a footnote.
- Link with relative paths and a `DR-`, `UC-`, `FS-`, `NFR-` or `ADR-` identifier so it can be
  checked mechanically.
- **Run the check** before and after every change:

  ```powershell
  pwsh -File tools/check-docs.ps1        # powershell -File tools/check-docs.ps1 on Windows
  ```

  It resolves every link and anchor, resolves every identifier reference, rejects a double
  definition, and recomputes the inventory in [docs/README.md](docs/README.md#inventory).

## Licence

Apache-2.0, including the patent grant. See [LICENSE](LICENSE) and
[ADR-0014](docs/decisions/0014-apache-2-0-license.md). The default data-store image is
BSD-3-Clause; the Redis licensing position, which is time-sensitive, is in
[the licensing research](docs/research/licensing-redis-vs-valkey.md).
