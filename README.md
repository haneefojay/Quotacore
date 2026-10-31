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

## Quickstart

Docker with Compose v2, and nothing else: no account, no signup, no egress.

```bash
git clone https://github.com/haneefojay/Quotacore.git quotacore
cd quotacore
export QUOTACORE_BOOTSTRAP_ADMIN_KEY="$(docker compose run --rm --no-deps quotacore key mint)"
export QUOTACORE_BOOTSTRAP_ADMIN_KEY_ACK=yes
docker compose up -d --wait
docker compose exec quotacore quotacore init
```

The first line after `cd` builds the image and mints an admin key into your shell, never onto disk;
the second says you meant to start with it. `init` creates a feature, a plan, a tenant and a runtime
key, prints the key once, and ends with a `curl` you can paste. The API listens on
`127.0.0.1:8080` only. CI runs this block on a clean runner, so it cannot rot; the deployment guide
explains each line and everything after it ([deployment.md](docs/architecture/deployment.md#2-the-quickstart)).

## The three claims, and the tests behind them

| Claim | Enforced by | Proven by |
| --- | --- | --- |
| Check and deduct are one indivisible operation | One Lua script execution | 200 concurrent requests against a balance of 100, exactly 100 allowed |
| A retrying client is charged once; a double charge caused by store loss is reversed | Idempotency recorded in the same atomic execution, and a no-eviction store sized for the window | 50 concurrent retries, one deduction, 49 replays that answer the recorded state — then the T-14 store-loss case, one reversal |
| A missed cycle boundary cannot deny or over-grant | Lazy, monotonic, atomic rollover | Idle across three boundaries: one allowance, one event |

Full test list: [testing-strategy.md](docs/architecture/testing-strategy.md).

## Not this

No invoicing, no payments, no feature flags, no API-gateway rate limiting, no analytics, no PII, no
accounts, no phone-home. The reasoning is in
[the foundation](docs/product/foundation-and-personas.md#3-what-the-product-is-not) and it is
permanent, not a roadmap item.

## Status

**v0.1 complete.** Metered enforcement on the request path with a calendar reset is built,
tested and documented: phases `IP-00` to `IP-16` of
[the roadmap](docs/roadmaps/roadmap-index.md) are `COMPLETE`, and each of the three claims, each
measured NFR and each line of [mvp-scope.md](docs/product/mvp-scope.md) is proved in
[the v0.1 evidence](docs/release/v0-1-evidence.md), with its named gaps. The runbooks and the
restore drill were followed on the reference stack and approved on 2026-10-29, and a code review of
the whole tree followed before the release. The specification behind it is 81 documents, 17
accepted decisions, 50 domain rules, 36 error codes and 16 named correctness tests. What remains
open is in [the register](docs/product/assumptions-and-open-questions.md#2-open-questions), and none
of it blocks. Later releases, `IP-17` to `IP-27`, start from v0.1.

**A TypeScript client** is built and tested in [sdk/typescript/](sdk/typescript/README.md)
(`IP-26`): typed errors, safe retries, and an idempotency key per call that every retry reuses. It
is published to npm once v0.2's contract has landed, so the published API does not move under its
users ([Q-32](docs/product/assumptions-and-open-questions.md#2-open-questions)).

Start with [PROJECT.md](PROJECT.md), then follow the reading order it gives you.

## Documentation map

| Area | Count | Start here |
| --- | --- | --- |
| Root | 11 | [PROJECT.md](PROJECT.md) — orientation, status, and what to read next |
| Product | 9 | [foundation-and-personas.md](docs/product/foundation-and-personas.md) |
| Architecture | 12 | [overview.md](docs/architecture/overview.md) |
| Decisions | 18 | [TECHNICAL-DECISIONS.md](TECHNICAL-DECISIONS.md) |
| Research | 8 | [competitive-landscape.md](docs/research/competitive-landscape.md) |

The complete index, with what each document settles and who should read it, is
[docs/README.md](docs/README.md).

```
.
├── README.md              this file
├── PROJECT.md             what this is, status, reading order
├── PRD.md                 requirements, metrics, scope boundary
├── ARCHITECTURE.md        system shape, components, runtime flow
├── TECHNICAL-DECISIONS.md index of all 18 ADRs
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
│   ├── openapi.yaml       THE contract: 41 operations, 67 schemas, 35 codes
│   ├── docs.html          the generated reference, served at /docs
│   └── oas_*_gen.go       generated types, codecs and validators. Do not edit
├── cmd/quotacore/         the service entry point
├── internal/api/          the router, the probes, and the served contract
├── internal/db/           embedded up-only migrations
├── sdk/typescript/        the TypeScript client, generated from the contract (IP-26)
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
