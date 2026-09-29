# API Conventions

Everything a client needs in order to integrate correctly, and nothing that belongs in the
generated reference. The reference itself is `api/openapi.yaml`, served at `/openapi.json`, with
generated HTML at `/docs` (ADR-0010).

**This document is normative.** Where it and the OpenAPI document disagree, that is a defect in
one of them, and a contract test enforces that they do not.

---

## 1. Transport

| Aspect | Convention | Rationale |
| --- | --- | --- |
| Scheme | HTTPS only, enforced by the deployment | A bearer token in clear text is a bearer token that has leaked |
| Port | 8080 in the container, behind the customer's proxy | Not privileged (NFR-S6) |
| Content type | `application/json` for requests and responses | One parser, one set of error semantics |
| Method semantics | `POST` for anything that mutates or carries an amount, including `check` | A `GET` with a body is not portable, and a `GET` that means "would this work" invites caching (Q-06) |
| Character encoding | UTF-8 | — |
| Chunked requests | Rejected with a size cap | No legitimate client needs it, and it is a slow-loris surface |
| Compression | Response compression supported; request compression not accepted | Request bodies are already tiny and bounded |
| Versioning | Path prefix `/v1`; the version is the major contract, not the media type | Simple, greppable, and the standard choice for a self-hosted service |
| Deprecation | A deprecated endpoint is served for at least one minor release with a `Deprecation` and `Sunset` header | A self-hosted customer upgrades on their own schedule |

**Base paths.** `/v1/*` is the data plane. `/v1/admin/*` is the control plane. `/docs`,
`/openapi.json`, `/healthz`, `/readyz` and `/metrics` are outside both.

## 2. Identifiers and naming

| Element | Type | Format | Notes |
| --- | --- | --- | --- |
| `tenant_id` | string | UUIDv7 | Opaque. Never parsed by a client |
| `plan_id` | string | UUIDv7 | Opaque |
| `feature_id` | string | UUIDv7 | Opaque. Clients use `feature_key` |
| `feature_key` | string | `^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$` | Immutable, customer-chosen, reverse-DNS style (INV-F3) |
| `plan_key` | string | `^[a-z0-9][a-z0-9_-]{0,62}$` | Immutable |
| `tenant.external_id` | string | `^[A-Za-z0-9._:-]{1,128}$` | Customer-chosen, unique, excludes `@` (DR-034) |
| `unit` | string | `^[a-z][a-z0-9_]{0,31}$` | Immutable once assigned (INV-F2) |
| `Idempotency-Key` | string | 8–255 chars, `[A-Za-z0-9._:-]` | Client-generated, stable per logical operation |
| `X-Request-Id` | string | ULID or UUID | Adopted if valid, otherwise minted (NFR-S10) |

Every identifier is opaque. A client that parses a `tenant_id` to extract meaning from it is
relying on an implementation detail, and the ID type is chosen for index locality, not
comprehensibility.

## 3. Time

**All timestamps in responses are RFC 3339 with an explicit offset.**

```
"cycle_start": "2026-10-01T00:00:00+02:00"
"cycle_end":   "2026-11-01T00:00:00+01:00"
```

The offset is included even when it is `Z`, except where `Z` is the whole value. The rule is
uniform: **an offset is always present**, so no client has to branch on whether the zone is UTC.

Why this matters more than it looks: the difference between those two timestamps is not 31 days.
It is 31 days minus one hour, because the zone moved. A client that assumes 24-hour days will
display a cycle that ends an hour early or late, and the customer will notice.

| Rule | Value |
| --- | --- |
| Request-side timestamps | None accepted on the data plane. The server is the sole time authority (DR-001) |
| `cycle_end: null` | A `never` entitlement. Clients must handle the null; it is the documented shape (INV-C6) |
| `never` semantics | No boundary, no expiry, no rollover. A lifetime allowance (DR-008) |
| Precision | Milliseconds in storage, RFC 3339 to the second in responses unless sub-second precision was configured |
| Display advice | Documented in the reference: render in the tenant's zone, and say which zone |

## 4. Amounts

```json
{ "feature_key": "llm.tokens.output", "amount": 4200, "metadata": { "request_id": "…" } }
```

- JSON **integers only**. `4200.0` is rejected with `400 validation_failed` naming the field.
  Accepting a float and rounding it silently is how a customer loses money without noticing.
- Range: `0` to `2^63 − 1`, as `int64` (DR-022).
- `0` is valid and is a no-op that records no event (DR-023, and the `delta <> 0` constraint).
- Negative is `400 invalid_amount`. Direction is the endpoint, never a sign (DR-023, ADR-0005).
- The unit is the feature's unit, documented on the feature, and is never inferred. A client that
  sends requests where the unit is "tokens" when it is "requests" produces a plausible-looking
  wrong answer, which is why `unit` is immutable (INV-F2).
- No fractional amounts in the MVP. A feature needing fractional accounting uses a scaled integer
  unit such as `millisecond` (DR-022).

## 5. Authentication and authorisation

```
Authorization: Bearer qc_live_9f2c1a4e7b8d…
```

| Aspect | Rule |
| --- | --- |
| Format | `qc_live_<prefix>.<secret>` for runtime keys, `qc_admin_<prefix>.<secret>` for admin keys. The scope is visible in the token, so a mismatch is a 400-line debugging session avoided |
| Storage | Only an Argon2id hash and the public prefix are stored (ADR-0012) |
| Recovery | None. A lost key is replaced. There is no endpoint that returns a key, and none will be added (INV-K3) |
| Scopes | `runtime` for `/v1/*` data plane; `admin` for `/v1/admin/*`. An `admin` key also works on the data plane |
| Expiry | Optional at issue time. Enforced on every request |
| Revocation | Immediate. No cache delay, because the cache stores the hash and the state, and revocation bumps the version (K-2) |
| Bootstrap | One admin key on first start, printed once at `warn`. The service **refuses to start** if it would run with an unacknowledged default key (NFR-S9) |
| Cross-tenant | A runtime key is **not** bound to one tenant in the MVP. A key is a credential for the whole instance, and `external_id` in the request selects the tenant. Consequence, stated plainly: possession of a runtime key gives access to every tenant on the instance. This is why key issuance is an admin action, is audited, and is a per-customer decision |
| Logging | Only the prefix appears, never the secret (DR-043) |

That last row is the most security-relevant decision in the API surface, so it is documented as a
consequence rather than buried: this is a single-tenant-per-instance product (ADR-0013), and a
runtime key is scoped to the instance, not to a customer of the instance's owner's.

### 5.1 How the contract states security

`bearerAuth` is declared once, globally. The per-operation `security` array is the whole statement
of what an operation accepts, and it takes these forms:

| Operation | `security` | `x-required-scope` | Why |
| --- | --- | --- | --- |
| Data plane, `/v1/*` | `[ { bearerAuth: [] } ]` | `runtime` | An `admin` key also works here, so the scope is a minimum, not an equality |
| Control plane, `/v1/admin/*` | `[ { bearerAuth: [] } ]` | `admin` | An `admin` action, and the scope is what the router checks before the handler |
| Operational, probes and `/openapi.json` and `/docs` | `[]` | absent | Unauthenticated by design, because a liveness probe that needs a credential is a liveness probe that fails when credentials break (NFR-S9) |

Three things about that table are deliberate:

- **The requirement arrays are empty.** A scope in the array would mean the token must carry
  exactly that scope, which excludes an `admin` key from the data plane — the opposite of the row
  above in section 5. `[]` means "bearer, any scope"; `x-required-scope` names the minimum the
  implementation enforces.
- **`x-required-scope` is a vendor extension, not OpenAPI.** OpenAPI 3.1 has no vocabulary for "this
  operation needs at least this scope", and inventing one inside the standard would make the
  document non-conformant. A named extension is discoverable by a reader and ignored by a tool that
  does not know it, which is the correct failure: an unknown tool is not wrong, it is just not
  enforcing this yet.
- **Operational routes state `security: []` explicitly** rather than relying on the global
  declaration. Inheriting a global requirement and then opting out is how an operational route
  accidentally requires a credential in one release and not the next.

`TestSecurityRequirementsCarryNoScopes` asserts the empty arrays, and
`TestEveryOperationDeclaresItsRequiredScope` asserts that every operation outside
`/v1/admin/*` and the operational set declares `runtime` and that every `/v1/admin/*` operation
declares `admin`. A route added without thinking about its scope fails the build.

## 6. Idempotency

Required on `consume` and `refund`. Not required on `check`, because it mutates nothing (Q-07).

```http
POST /v1/consume
Idempotency-Key: 01JCQ9W3K2M9XQ3F8V6T0B5NDE
```

| Rule | Behaviour |
| --- | --- |
| Generation | One key per **logical operation**, reused across every attempt including retries. Derive it from the customer's own durable operation identifier where one exists (J-7) |
| Fingerprint | A hash of `operation | tenant | feature | amount`. A repeat with a matching fingerprint replays the stored response with `replayed: true` |
| Mismatch | `409 idempotency_key_reuse`, no state change, original record preserved (DR-028, INV-I2) |
| Window | 24 h from application. A key first seen after that is a new operation (DR-029) |
| Scope | Per tenant, not per key, not per instance. The same key used for two tenants is two operations |
| Storage | Authoritative in the data store, written in the same atomic execution as the mutation (DR-030) |
| Response header | `Idempotent-Replay: true` on a replay, so a proxy or a client can tell without parsing the body |
| Failure | `400 missing_idempotency_key` when absent. There is no server-generated fallback, because a generated key silently defeats the purpose |

**The one case a server cannot protect against** is documented in the reference, in the use case,
and in the failure catalogue: a client that retries with a *new* key is charged twice (FS-03). No
server-side scheme detects that, and pretending otherwise would be worse than saying so.

## 7. Request bodies

### 7.1 `consume` and `refund`

```json
{
  "feature_key": "llm.tokens.output",
  "amount": 4200,
  "tenant_external_id": "acme-prod-01",
  "metadata": { "model": "claude-opus-5", "conv": "c_9f3a" }
}
```

| Field | Required | Notes |
| --- | --- | --- |
| `feature_key` | yes | A single key, never an array. One feature per call, by design: a bundle would have to attribute its cost across features, which breaks the per-feature event model and the idempotency fingerprint, and a partial bundle failure is a state the caller cannot reason about. A bundle is two calls. *Answers* [Q-05](../product/assumptions-and-open-questions.md#2-open-questions) |
| `amount` | yes | Integer, ≥ 0 (DR-023) |
| `tenant_external_id` | yes on the data plane | The customer never handles a `tenant_id` |
| `metadata` | no | ≤ 4 KiB. Correlation only. Not a data store, not indexed, excluded from logs (DR-043), and not returned by any endpoint |
| `window` | no | v0.2 only. Omitted means the anchored cycle. A window that disagrees with the feature's cadence is `400 invalid_window` |

Response:

```json
{
  "tenant_external_id": "acme-prod-01",
  "feature_key": "llm.tokens.output",
  "balance": 41250,
  "limit": 1000000,
  "bonus": 0,
  "consumed": 4200,
  "cycle_start": "2026-09-01T00:00:00+02:00",
  "cycle_end": "2026-10-01T00:00:00+02:00",
  "replayed": false
}
```

`balance` is after the deduction. `consumed` is the amount the *original* call applied, and it is
returned unchanged on a replay, because it is read from the record rather than recomputed
(DR-026, DR-027). A retry is not byte-identical to the original — the first response carries
`"replayed": false` and the retry `"replayed": true` — but the seven state values are, so a client
that needs to know whether this particular call moved anything reads `replayed` or the
`Idempotent-Replay` header, a one-field test rather than an inference from arithmetic. `balance`
minus `consumed` is the balance before the original deduction, which is why both are returned.

### 7.2 `check`

Same body, no `Idempotency-Key`. Response:

```json
{
  "allowed": false,
  "balance": 50,
  "limit": 1000,
  "bonus": 0,
  "requested": 100,
  "cycle_end": "2026-10-01T00:00:00+02:00"
}
```

A `200`, always, when the question could be answered. A denial is `allowed: false`, **not** a
`429` (DR-024). A `429` from `/v1/check` means the *service* is rate-limiting, which is a
different thing with a different remedy (DR-040).

`check` is advisory and is stated as such in three places in the reference, because the failure
mode of ignoring it - two requests both passing `check` and one being denied at `consume` - looks
like a bug the first time a customer hits it (J-2).

`POST` rather than `GET`, because the body carries an `amount` and a `window`, and a `GET` with a
body is not portable. The verb is `check` and not `quote`, because it reserves nothing.
*Answers:* [Q-06](../product/assumptions-and-open-questions.md#2-open-questions) and
[Q-07](../product/assumptions-and-open-questions.md#2-open-questions), which asked whether the verb
should be `POST` or `GET` and whether an idempotency key is required. It is not: `check` mutates
nothing, so a duplicate is harmless (DR-024), and the endpoint never returns
`idempotency_key_reuse`.

### 7.3 `balance`

`GET /v1/balance?tenant_external_id=…&feature_key=…`

With a `feature_key`, one entry. Without, the full set for the tenant, which is bounded by the
plan's entitlement count (NFR-T5), so the unbounded case does not exist. Always includes
`cycle_start` and `cycle_end` with offsets, and `null` for both on a `never` entitlement.
*Answers:* [Q-10](../product/assumptions-and-open-questions.md#2-open-questions), which asked
whether the endpoint returns every feature for a tenant or requires a feature key. `feature_key` is
optional.

### 7.4 Nullable-but-required control-plane fields

A control-plane request body may carry a field that is **required and nullable**, where `null` means
"not specified by this request" as opposed to the field being absent. A nullable field states an
intent the caller has formed; an absent one states nothing, and those are different facts.

`POST /v1/admin/tenants/{id}/overrides` is the case in point. UC-13 sets "a limit, a reset
interval, or both", so `limit_value` and `interval` are both required, both nullable, and at least
one is non-null:

```json
{
  "feature_key": "llm.tokens.output",
  "limit_value": 10000000,
  "interval": null,
  "apply_at": "next_boundary"
}
```

`null` on `interval` inherits the plan's cadence. Omitting `interval` entirely is a
`400 validation_failed`, and requiring the key rather than allowing absence is the point: absence
would have two readings, that the aspect is inherited and that the operator left it out, and a
quota system should not have a wire format with two readings for the same omission.

**The both-null case is enforced by the data store, not by the schema.** It is
`400 validation_failed` when it happens, and the constraint that catches it is
`tenant_overrides_not_empty`. This is stated here rather than left implicit because the schema
genuinely cannot express it: the JSON Schema for "at least one of two properties is non-null" is an
`anyOf` over object branches, and the generator named in
[ADR-0010](../decisions/0010-go-chi-spec-first-openapi.md)
cannot generate one. A schema that appeared to enforce this and did not would be worse than one
that says it does not, so the division of labour is stated: the schema enforces presence, type and
range; the store enforces non-nullness.

## 8. Error handling

Full catalogue in [error-catalog.md](../product/error-catalog.md). Conventions:

| Aspect | Rule |
| --- | --- |
| Envelope | One shape, everywhere, non-negotiable |
| `code` | The only field to branch on. Stable, permanent, lower snake case |
| `message` | Human-readable, may improve. Never parse it |
| `request_id` | Always present, and equal to the `X-Request-Id` header |
| `details` | Code-specific, additive across versions |
| `retry_after_ms` | Present only when retrying is correct |
| Status codes | The status is a class hint; the code is the meaning. `429` is two different things (DR-040) |
| Validation | Aggregated. Three bad fields produce one `validation_failed` naming all three |
| No `404` for authorisation | A valid key with the wrong scope gets `403`, because the caller is already authenticated and hiding the endpoint helps nobody (FS-18) |
| No existence disclosure | On the data plane, an unknown feature and a feature not in the plan both produce `403 feature_not_in_plan`, so feature existence is not leaked (FS-17) |

## 9. Pagination and filtering

Used by `GET /v1/admin/tenants`, `/events` and `/audit`.

| Aspect | Rule |
| --- | --- |
| Style | Opaque cursor. `?cursor=…&limit=…` |
| Default limit | 50. Maximum 200 |
| Ordering | Newest first, always, with a stable tiebreaker so a cursor is never ambiguous |
| Cursor | Opaque and signed. A tampered cursor is `400 validation_failed`, not a silent full scan |
| Filters | `tenant_external_id`, `feature_key`, `from`, `to`, `source`, `event_type` |
| Filter values | `event_type` is one of `consumed`, `refunded`, `granted`, `set`, `plan_changed`, `cycle_rolled_over`, `force_rolled_over`. `source` is one of `api`, `admin`, `system`. Any other value is `400 validation_failed`, never an empty result |
| Time filters | RFC 3339 with offset, matching the response format |
| Totals | Not returned. A count over a large ledger is a slow query pretending to be metadata |
| Empty | An empty page returns an empty array, never `null` |

## 10. Idempotency and concurrency on the control plane

Admin mutations carry an expected `version`, taken from the `GET` that preceded them.

```
PATCH /v1/admin/plans/{id}
{ "version": 7, "entitlements": [ … ] }
```

A mismatch is `409 resource_version_conflict` with the current version in `details`, so a client
can re-read and retry deliberately. Last-write-wins on a plan that governs a customer's money is
not acceptable, and an operator with two open browser tabs is exactly the case (FS-12).

### 10.1 `apply-now` and its confirmation handshake

`POST /v1/admin/plans/{id}/apply` re-anchors every assigned tenant, so it is guarded by blast radius
rather than by operator intent (DR-046). The threshold is
`QUOTACORE_PLAN_APPLY_CONFIRM_THRESHOLD`, default `50`.

**At or below the threshold.** One call, `200`, and the tenants are re-anchored.

**Above the threshold.** The service counts first, writes nothing, and returns:

```
409 confirmation_required
{
  "error": {
    "code": "confirmation_required",
    "request_id": "01J8Z6M2QK9V3X7B4C0N5TDEFA",
    "details": {
      "affected_tenants": 1840,
      "earliest_cycle_end": "2026-10-01T00:00:00+02:00",
      "confirmation_token": "cnf_01J...",
      "threshold": 50,
      "expires_in_s": 900
    }
  }
}
```

Every code-specific field lives inside `details`, and `details` is where a client looks for
whatever the particular `code` means. The envelope's own fields are fixed: `code`, `message`,
`request_id`, and `details`. That is a deliberate cost. A client that switches on `code` and
reads `details` is the only shape that lets a new code carry new information without a new
envelope version, and it means a code can never be added that quietly overflows the envelope
shape, because the envelope has no room to overflow.

The operator re-issues the identical request with `confirmation_token` in the body, and it applies.
Three properties make the handshake safe rather than merely awkward:

| Property | Consequence of getting it wrong |
| --- | --- |
| The token is single-use | A retried confirmation cannot re-apply an already-applied `apply-now` |
| The token is bound to the plan `resource_version` it was issued against | A plan edited in between is refused, because the count the operator approved is no longer the count |
| The token expires on `QUOTACORE_CONFIRMATION_TTL` | An abandoned token does not accumulate in a store that cannot evict (DR-048) |

`Idempotency-Key` is not used for this. The handshake is already exactly-once by construction, and
adding a second exactly-once mechanism to the same operation is a way to have two opinions about
whether it happened.
*Answers:* [Q-04](../product/assumptions-and-open-questions.md#2-open-questions), which asked
whether `apply-now` requires a confirmation token when more than N tenants are affected.

### 10.2 `GET /v1/admin/plans/{id}/impact`

The read-only companion, so the common case is a quiet read rather than an error:

```
GET /v1/admin/plans/{id}/impact?resource_version=7

200
{
  "plan_id": "growth",
  "resource_version": 7,
  "affected_tenants": 1840,
  "requires_confirmation": true,
  "threshold": 50,
  "earliest_cycle_end": "2026-10-01T00:00:00+02:00",
  "latest_cycle_end": "2026-11-30T00:00:00+01:00"
}
```

It changes nothing: no anchor moves, no audit row is written, no token is issued. It reports the
same count the `409` would, so an operator who wants to know does not have to provoke a refusal to
find out (UC-19). `resource_version` is optional and, when supplied, must match; the count is
meaningless against a version the operator has not read.

`earliest_cycle_end` is the answer to "when does this land first", which is the question that
actually decides whether an `apply-now` is safe to run on a plan whose tenants are on monthly
cycles with scattered anchors.
*Answers:* [Q-14](../product/assumptions-and-open-questions.md#2-open-questions), which asked
whether an operator can see a plan edit's blast radius before committing it. This route is the
answer, and the `409` above is the fallback for an operator who does not call it.

### 10.3 Deletion and in-flight requests

`DELETE /v1/admin/tenants/{id}` is soft and is ordered against concurrent requests by its commit
point (DR-047). The contract is deliberately narrow, because the alternative is worse:

| Timing of the request | Result |
| --- | --- |
| Entered the protected script before the delete committed | Completes. The deduction stands |
| Reads the tenant after the delete committed | `404 tenant_not_found`, or `409 tenant_deleted` where the caller may already see that the tenant exists |
| Completed, then the tenant is deleted | Unchanged. Deletion never unwinds a completed deduction |

A client that received a `2xx` from `consume` and then sees a `404` on its next call has not lost
money; it has a successful charge against a tenant that has since been closed. The remedy is
`POST /v1/refund`, which is a first-class operation with its own audit trail, and not the delete.
Documented because the intuitive reading of "deleted" is "undone", and that reading is wrong.
*Answers:* [Q-16](../product/assumptions-and-open-questions.md#2-open-questions), which asked what
happens to a request that is in flight when a tenant is deleted.

## 11. Headers

### Request

| Header | Required | Notes |
| --- | --- | --- |
| `Authorization` | yes, except probes and docs | `Bearer <key>` |
| `Idempotency-Key` | on `consume` and `refund` | See §6 |
| `X-Request-Id` | no | Adopted if a valid ULID or UUID, else minted |
| `Accept-Encoding` | no | `gzip` supported |
| `Content-Type` | yes for bodies | `application/json` |

### Response

| Header | Notes |
| --- | --- |
| `X-Request-Id` | Always. Quote it in a bug report |
| `Idempotent-Replay` | `true` on a replay |
| `Retry-After` | Seconds, on `429` only, alongside `retry_after_ms` in the body for convenience |
| `Cache-Control` | `no-store` on every response. A cached quota answer is a wrong quota answer |
| `Content-Type` | `application/json` |
| `Deprecation`, `Sunset` | Only on a deprecated endpoint |
| `Server` | Suppressed. The version is in `GET /v1/admin/status`, which is authenticated |

## 12. Versioning and compatibility

| Level | Change | Client impact |
| --- | --- | --- |
| Additive, optional field | New response field, new optional request field, new `details` key | None. Clients must ignore unknown fields — stated in the reference as a requirement, not a suggestion |
| Additive, new code | A new error code | **Action required.** A client that treats an unknown code as fatal will break. The reference therefore says: handle unknown codes as retryable-or-fatal per your own risk, and never assume the set is closed |
| Additive, new endpoint | — | None |
| Breaking | Removing or renaming a field, changing a type, changing the meaning of a `code` | Only in a new `/v2` path prefix, and only after evidence that the change is necessary |
| Never | Changing the meaning of an existing `code`, or the semantics of `balance`, `limit` or `cycle_end` | These are the contract. A silent semantic change is a licence to lose money quietly |

The "never" row is the whole reason the OpenAPI document is treated as a published contract rather
than generated documentation.

## 13. Contract tests

Enforced in CI, so this document and the specification cannot drift apart:

1. Every error code in [error-catalog.md](../product/error-catalog.md) exists in the OpenAPI
   document with the documented status, and vice versa.
2. Every endpoint in the OpenAPI document has a request and response example that is validated.
3. Every response field in the specification has a type and a description; a field with no
   description is a failing test.
4. The error envelope shape is validated for every documented non-2xx response.
5. `additionalProperties` is `false` on request bodies, so a typo in a field name is a `400`
   rather than a silently ignored field. This is the difference between a typo being found in
   testing and found in production by a customer. Asserted per schema, not on a sample, so a
   body added without the constraint fails the build.
6. The generated Go types are rebuilt from the specification, so the served validation and the
   reference cannot describe different shapes. TypeScript types are not generated in this phase;
   see [ADR-0010](../decisions/0010-go-chi-spec-first-openapi.md).
7. No `$ref` in the document is unresolved, and no component is defined and left unreferenced. A
   dead schema is either a draft or a mistake, and neither should survive review.
8. A constraint the schema cannot express is stated as such in the schema, with the mechanism
   that does enforce it. See [section 7.4](#74-nullable-but-required-control-plane-fields) for
   the one such constraint today; the rule is that a silent gap fails the contract test rather
   than passing as enforcement.

Item 3 is enforced slightly more strictly than it reads. The test requires a description on every
*schema* as well as on every field, and it accepts a description that a field inherits through a
`$ref` or an `allOf`, so a field that is a bare `$ref` to a documented component is not asked to
repeat the component's prose. The stricter form is deliberate: a schema with no description is a
shape nobody has explained, and field-level inheritance only works if the component is documented in
the first place.

## 14. The served contract

The contract is one file, `api/openapi.yaml`, and it is served. Three things follow from that, and
each is a rule rather than an implementation note.

| Route | Serves | Cache |
| --- | --- | --- |
| `GET /openapi.json` | The embedded contract, converted from YAML to JSON at start-up | `no-store` |
| `GET /docs` | A committed HTML page, `api/docs.html`, generated from the same file | `no-store` |
| `GET /healthz`, `GET /readyz` | The probe bodies | `no-store` |

- **`/openapi.json` is JSON, and it is generated from the YAML at build time** rather than parsed
  per request. A request path that parses a 5,000-line document is a request path that spends time
  on something that cannot change while the process runs, and the conversion is done once and
  cached. Object key order is preserved, so the served bytes are stable and diffable; a converter
  that sorted keys would make the output look reordered on every change.
- **Both are unauthenticated**, for the reason in [section 5.1](#51-how-the-contract-states-security).
  A contract that needs a credential cannot be fetched by the tool that documents the credential.
- **`/docs` is a committed artefact, not a runtime template.** Generating the page per request would
  mean the reference is only as correct as the code path that renders it; committing it means the
  page is reviewed in a diff like any other deliverable. It is self-contained — inline CSS, no
  JavaScript, no CDN, no external font, nothing fetched — because a documentation page that needs
  the network is a documentation page that is blank on an air-gapped deployment, and this product is
  designed to run on one.
- **The page and the generated Go types are both derived from the same file**, and both are checked
  for drift: `make generate-check` regenerates them and fails if the working tree changed. A
  contract that has drifted from its artefacts is worse than no contract, because it looks
  authoritative.
- **`/docs` and `/openapi.json` are `GET` only.** A `HEAD` or `POST` is `405`, not a silent success
  and not a redirect; the contract documents one method and the router serves that method.
