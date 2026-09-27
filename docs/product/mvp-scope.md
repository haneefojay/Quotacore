# MVP Scope

Authoritative statement of what is in each release, what is deliberately out, and what "done"
means for each capability. The release ordering rationale is ADR-0015. Anything listed as out of
scope is out of scope for the MVP release specifically, not necessarily forever; the two
categories are distinguished and both matter.

## 1. The cut line

The MVP is complete when a customer can, in one `docker compose up`:

1. define features and a plan,
2. provision a tenant onto that plan,
3. enforce a metered limit on a live request path, atomically and idempotently,
4. have that limit reset correctly on its own at a calendar boundary,
5. change the plan limit and see it take effect at the next boundary without a redeploy,
6. credit a mistaken deduction,
7. investigate a dispute from a CLI,
8. and operate the whole thing without an account, a cloud dependency, or a support ticket.

Everything in section 3 serves one of those eight. Everything in section 4 does not, however
attractive it is.

**The single most important scope decision:** the MVP has no `check`-then-`consume` split in the
customer's code path. The protected action calls `consume` and branches on the result. This is
why the MVP can ship without SDKs, and it is why the API is honest about `check` being advisory
(DR-024).

## 2. Release slicing

### v0.1 — MVP: authoritative metered enforcement

| Capability | Detail |
| --- | --- |
| Data plane | `POST /v1/consume`, `POST /v1/refund`, `POST /v1/check`, `GET /v1/balance` |
| Reset intervals | `hourly`, `daily`, `weekly`, `monthly`, `yearly`, `never` (DR-003 to DR-009) |
| Cadence model | Anchored calendar cycles only. Rolling and sliding windows are v0.2 (ADR-0008) |
| Control plane | Features, plans, tenants, tenant overrides, API keys |
| Cycle control | Lazy monotonic rollover on the request path plus an idempotent reset worker (ADR-0003) |
| Allocation caps | `consume 1` on create, `refund 1` on delete, same atomic path (DR-017, DR-019) |
| Idempotency | Required on `consume` and `refund`, 24 h window (DR-026 to DR-030) |
| Admin operations | `grant`, `set`, `force-rollover`, `suspend`, `resume`, `apply-now`, soft delete |
| Destructive bulk operations | `apply-now` above `QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD` returns `409 confirmation_required` with a single-use token and applies nothing (DR-046) |
| Plan impact preview | `GET /v1/admin/plans/{id}/impact` — read-only affected count and earliest affected `cycle_end` (UC-19) |
| Audit | `audit_log` for every administrative mutation (DR-041) |
| Ledger | Signed-delta `usage_events`, asynchronous, reconstructable (ADR-0016) |
| Auth | Named, scoped, rotatable API keys, hashed at rest (ADR-0012) |
| Observability | Prometheus metrics, structured JSON logs, health and readiness endpoints |
| API contract | `api/openapi.yaml`, served at `/openapi.json`, generated docs at `/docs` (ADR-0010) |
| CLI | `quotacore` admin CLI: init, plan, feature, tenant, key, grant, set, rollover, events |
| Deployment | Multi-stage Dockerfile, `docker compose up` with Postgres 16 and Valkey |

### v0.2 — entitlement expressiveness

| Capability | Detail |
| --- | --- |
| Boolean entitlements | Feature present or absent, with no counter. Answers `feature_not_in_plan` (DR-012) vs `quota_exceeded` distinctly (UC-02) |
| Rolling windows | `rolling_5m` to `rolling_30d`, requested-duration lookback inside the current cycle (ADR-0008) |
| Mixed cadences | A boolean entitlement alongside metered entitlements in one plan, per feature |
| Allocation cap reporting | Which resource type the cap counts, and the current count, in `GET /v1/balance` |
| Per-tenant runtime API keys | A runtime key bound to one tenant, which cannot address another; closes gap 1 in [SECURITY.md](../../SECURITY.md#6-known-gaps) |

### v0.3 — outbound notification

| Capability | Detail |
| --- | --- |
| Webhooks | Signed outbound POST on threshold crossings, plan changes, cycle rollover |
| Delivery guarantees | At-least-once, exponential backoff with jitter, bounded attempts, dead-letter table |
| Idempotent receivers | A stable `event_id` in the payload and in the `Quotacore-Delivery` header, so a customer's handler can be idempotent |
| Configuration | Per-tenant endpoint, secret, and event-type subscription set |

### v0.4 — operator interface

| Capability | Detail |
| --- | --- |
| Embedded admin UI | Single-page app served by the same binary, authenticating with admin-scoped keys |
| Views | Tenant list and detail, plan editor with cycle-boundary preview, feature list, event and audit history, live balance inspector |
| Read-heavy first | Write actions confirm and show the exact effect, including the cycle end that a change will first apply at |

### v0.5 — client libraries

| Capability | Detail |
| --- | --- |
| TypeScript and Python SDKs | Generated from `api/openapi.yaml`; hand-written thin wrappers |
| Automatic idempotency keys | Generated per logical operation and reused across retries, which is the correct pattern the API requires (DR-026) |
| Retry policy | Typed errors, bounded retry with jitter, retry only on `5xx` and `429 rate_limited` |

## 3. Definition of done, per capability

This is the checklist the implementation is held to. It is deliberately specific, because
"working" is not a definition anyone can agree on.

### Metered consumption
- [ ] A single Lua script execution performs the idempotency check, the cycle rollover, the
      ceiling check and the deduction. Verified by reading the script: no interleaving is
      possible.
- [ ] Under 200 concurrent requests for a balance of 100 and a cost of 1, exactly 100 succeed and
      exactly 100 are denied. Asserted automatically.
- [ ] The same idempotency key sent 50 times concurrently deducts once and returns one stored
      response for all 50.
- [ ] A reused key with a different amount returns `409 idempotency_key_reuse` and changes no
      state.
- [ ] `check` performs no mutation, proven by asserting the balance and the event count are
      unchanged across a `check` call.

### Cycle and rollover
- [ ] A tenant idle across three boundaries is placed in the current window, not reset three
      times, and does not receive three allowances.
- [ ] The request path and the worker, both racing on the same boundary, produce the same result
      regardless of the interleaving. Asserted with a deterministic race harness.
- [ ] A rollover that is already behind is rejected rather than applied, so an out-of-order
      worker can never re-grant a stale allowance.
- [ ] Boundaries match a table of expected wall-clock times for: month-end clamping, 29 February,
      a spring-forward gap, a fall-back repeat, and `never`.
- [ ] `GET /v1/balance` returns `cycle_start` and `cycle_end` as RFC 3339 in the tenant's zone,
      with the offset included.

### Money-touching operations
- [ ] A refund above the DR-019 ceiling is refused with `409 refund_exceeds_grant` and leaves
      the balance unchanged.
- [ ] A refund on a `never` entitlement is applied and never expires.
- [ ] Every applied mutation appends exactly one event whose delta reconciles to the balance.

### Control plane
- [ ] A plan limit edit changes no current balance and takes effect at each tenant's next
      boundary.
- [ ] `apply-now` re-anchors every assigned tenant, is audited with a required reason, and is
      visible in the audit log with before and after values.
- [ ] `apply-now` above the threshold applies nothing, returns `409 confirmation_required` with a
      token, and applies on the single use of that token; a replayed token is rejected (T-12).
- [ ] `GET /v1/admin/plans/{id}/impact` reports the affected tenant count and the earliest affected
      `cycle_end` without mutating anything, and an empty plan returns zero rather than `404`.
- [ ] A plan change on an individual tenant hard-resets that tenant's cycle (DR-013) and emits a
      `plan_changed` event.
- [ ] A suspended tenant is denied with `403 tenant_suspended` and resumes with its balance
      intact.
- [ ] An archived feature denies new consumption with `403 feature_archived` and preserves
      balances.
- [ ] A delete is refused with `409 plan_in_use` while any tenant references the plan.

### Auth
- [ ] No API key is recoverable in plaintext from the database, logs, or metrics.
- [ ] `runtime` scope cannot call any `/v1/admin` path; `admin` scope can call the data plane.
- [ ] A revoked key is rejected on the next request with no cache delay.
- [ ] Bootstrap admin key creation is logged at `warn` with the key's prefix, never its value.

### Operations
- [ ] `docker compose up` yields a working service with no manual step, no account, no signup.
- [ ] The data plane is unaffected by a control-plane outage for tenants already in the snapshot
      cache.
- [ ] A Redis outage produces `503` inside the latency budget, with no partial deduction and no
      silent allow.
- [ ] A Postgres outage does not affect enforcement for cached tenants.
- [ ] `/healthz` and `/readyz` are correct and distinct; readiness is false when enforcement
      cannot serve.
- [ ] Every metric listed in [observability](../architecture/observability.md) is emitted.

### Contract and documentation
- [ ] `api/openapi.yaml` is the single source of truth; the binary serves the same document at
      `/openapi.json`.
- [ ] Every error code in [error-catalog.md](error-catalog.md) exists in the OpenAPI document
      with a documented status, code, and remedy.
- [ ] The README's quickstart is executed by a script in CI against a real stack.

## 4. Out of scope for the MVP

### 4a. Explicitly out, and intended to stay out

| Excluded | Reason |
| --- | --- |
| Invoicing, payment methods, cards, dunning, revenue recognition | Not a business; deliberately absent. See foundation, section 3 |
| Multi-plan composition, add-ons, proration on plan change | Composition is a billing-platform concern. Proration deferred (DR-013) |
| Usage rollover / accumulation of unused allowance | A real feature and a materially more complex model. Deferred, not forgotten (DR-010) |
| Fractional amounts and decimal units | `int64` in the feature's unit. The alternative is a decimal type, deferred (DR-022) |
| Per-second / sliding-window rate limiting | Requires sliding-window counters in the hot path. Rejected in favour of a truthful calendar model (ADR-0008) |
| A server-side fail-open configuration | Converting a dependency outage into a revenue outage. The customer decides (DR-037) |
| Customer-facing billing portal or dashboard | Out of product scope. The customer embeds the data in their own application |
| PII, user records, identity | The customer's domain model (ADR-0013) |
| Multi-region active-active data plane | See [architecture overview](../architecture/overview.md) for the single-region posture and its explicit cost |
| Horizontal scale-out of the data plane | Single-region, single-primary design. See that document |

### 4b. Deferred to a named later release

Boolean entitlements and rolling windows (v0.2) · outbound webhooks (v0.3) · embedded admin UI
(v0.4) · official SDKs (v0.5) · per-feature metering weights · quota reservations and holds ·
promotional credit that survives a plan change · tenant-level rate limits · saved filters and
alerting in the UI · Terraform provider.

### 4c. Unknown, deliberately undecided

These are recorded in [assumptions-and-open-questions.md](assumptions-and-open-questions.md) with
a proposed default and a decision deadline, rather than quietly assumed:

- Quota reservation / hold semantics for expensive multi-step operations. Proposed default: not
  built; `consume` the estimate up front, refund the difference.
- Whether a customer's own application should ever be allowed to hold a long-lived stream to
  Quotacore. Proposed default: no; request-response only.
- A durable event export format. Proposed default: none; the ledger table is the interface and
  documented as such.

## 5. What the MVP deliberately does not optimise for

Stated because unstated non-goals are the ones that surprise people in production.

- **Maximum throughput.** The design is correct-under-concurrency, not maximum-concurrency.
- **Multi-tenant-per-instance economics.** One customer per instance is the model. Horizontal
  cost scaling is a deployment choice, not a feature (ADR-0013).
- **Cross-region latency.** A single region, with the latency characteristics documented in
  [non-functional-requirements](../architecture/non-functional-requirements.md).
- **Maximum configurability.** Six reset intervals and one override mechanism is the whole
  configuration surface in the MVP. A configuration surface that must itself be documented and
  supported is a liability.
