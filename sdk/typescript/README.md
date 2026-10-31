# @quotacore/sdk

The TypeScript client for [Quotacore](../../README.md). It checks and deducts quota, retries
safely, and handles idempotency for you, so a retried call is charged once.

> **Not yet published to npm.** The package is built and tested in this repository. It is
> published once v0.2's contract has landed, so the published API does not move under its users
> ([ADR-0015](../../docs/decisions/0015-release-slicing.md)). Until then, install it from a checkout
> with `npm install ./sdk/typescript` after `npm ci && npm run build` there.

Node 22 or later. No runtime dependencies.

## Quickstart

Run the [quickstart](../../README.md#quickstart) first. It prints a runtime key and creates the
tenant `local-dev` with the feature `llm.tokens.output`.

```ts
import { Quotacore, QuotaExceededError } from "@quotacore/sdk";

const qc = new Quotacore({
  baseUrl: process.env.QUOTACORE_URL ?? "http://localhost:8080",
  apiKey: process.env.QUOTACORE_API_KEY!,
});

try {
  const r = await qc.consume({
    tenant_external_id: "local-dev",
    feature_key: "llm.tokens.output",
    amount: 1,
  });
  console.log(`${r.balance} left until ${r.cycle_end}`);
} catch (e) {
  if (e instanceof QuotaExceededError) {
    console.log(`out of quota until ${e.details.cycle_end}`);
  } else {
    throw e;
  }
}
```

That's the whole integration. Every `consume` and `refund` carries an `Idempotency-Key` generated
for that call, and every retry of the call reuses it, so the service charges it once however many
attempts it takes ([DR-026](../../docs/product/domain-rules.md)).

## What the client does for you

| You don't have to | Because the client |
| --- | --- |
| Write a retry loop | Retries a transport failure, a `5xx` and `429 rate_limited`, up to 3 attempts with jittered backoff. It never retries a `quota_exceeded` or any other `4xx` |
| Generate idempotency keys | Generates one per call and reuses it on every retry. Pass `{ idempotencyKey }` to derive it from your own durable id instead |
| Guess what an error means | Raises a class per catalogue code, such as `QuotaExceededError` or `TenantSuspendedError`, each with `code`, `details`, `requestId` and the catalogue's `clientAction` |
| Wonder whether a timed-out call was charged | Raises `UnknownOutcomeError`, whose `resolve()` settles it safely |
| Page through lists | Follows `next_cursor` for you in `admin.paginate()` |
| Find the request id for a bug report | Puts `requestId` on every result and every error |

## Asking before you spend

`check` answers without deducting. A denial is `allowed: false`, not an error. It is advisory:
two calls can both pass, and one of them can still be refused at `consume`.

```ts
import { Quotacore } from "@quotacore/sdk";

const qc = new Quotacore({ baseUrl: process.env.QUOTACORE_URL!, apiKey: process.env.QUOTACORE_API_KEY! });

const answer = await qc.check({ tenant_external_id: "local-dev", feature_key: "llm.tokens.output", amount: 1 });
if (!answer.allowed) console.log("would be refused");

const { entries } = await qc.balance({ tenant_external_id: "local-dev" });
for (const e of entries) console.log(e.feature_key, e.balance, "of", e.limit);
```

Every answer carries a `kind`. A `boolean` feature is on or off: it has no counter, so `balance`,
`limit` and `bonus` are `null`, a consume takes an amount of 0 or 1, and anything else throws
`FeatureNotMeteredError`. A feature the plan does not include throws `FeatureNotInPlanError`, which
is not the same as `QuotaExceededError` on a metered feature with nothing left.

A `rolling` feature is counted over a sliding window, such as `rolling_1h`, and every call to it
must name that window in `window`; a missing or different one throws `InvalidWindowError`. Its
answers carry `window` and `window_precision_seconds`, `balance` is what remains in the window, and
`bonus` and the cycle fields are `null`.

## When the outcome is unknown

If every attempt of a `consume` or `refund` fails in a way that might have been applied, such as
a dropped connection or a timeout, the client raises `UnknownOutcomeError` rather than guess.
`resolve()` re-sends the same request with the same key. If the first attempt was applied, the
service replays its answer (`replayed: true`) and charges nothing more. If it wasn't, this applies
it once.

```ts
import { Quotacore, UnknownOutcomeError } from "@quotacore/sdk";

const qc = new Quotacore({ baseUrl: process.env.QUOTACORE_URL!, apiKey: process.env.QUOTACORE_API_KEY! });
const request = { tenant_external_id: "local-dev", feature_key: "llm.tokens.output", amount: 1 };

let result;
try {
  result = await qc.consume(request, { idempotencyKey: `example:${Date.now()}` });
} catch (e) {
  if (!(e instanceof UnknownOutcomeError)) throw e;
  result = await e.resolve(); // same key: charged at most once
}
console.log(result.requestId, result.replayed);
```

Don't settle an unknown outcome by calling `consume` again. A new call has a new key, and the
service will charge it a second time ([FS-03](../../docs/product/error-catalog.md)). `resolve()`
refuses after 23 hours, because the service's idempotency window is 24 hours
([DR-029](../../docs/product/domain-rules.md)).

## Errors

Branch on the class or on `code`, never on `message`. A code newer than this package arrives as a
plain `QuotacoreError` with its `code`, so new codes never crash an old client. A transport
failure is a `TransportError`, which shares no base class with `QuotacoreError`, so a handler for
network trouble can't swallow a quota denial by accident.

```ts
import { Quotacore, isQuotacoreError } from "@quotacore/sdk";

const qc = new Quotacore({ baseUrl: process.env.QUOTACORE_URL!, apiKey: process.env.QUOTACORE_API_KEY! });

try {
  await qc.consume({ tenant_external_id: "no-such-tenant", feature_key: "llm.tokens.output", amount: 1 });
} catch (e) {
  if (isQuotacoreError(e, "tenant_not_found")) {
    console.log(e.status, e.code, e.requestId, e.clientAction);
  } else {
    throw e;
  }
}
```

## The admin API

`client.admin` has every `/v1/admin/` operation in the contract, named by its `operationId` and
typed from it. It needs an `admin` key. Arguments are `{ path, query, body }`, in the contract's
own field names. Admin writes are retried only on `rate_limited`, which the service answers before
doing anything; a failed write is returned to you, because retrying it blindly could apply it
twice.

```ts
import { Quotacore } from "@quotacore/sdk";

const admin = new Quotacore({ baseUrl: process.env.QUOTACORE_URL!, apiKey: process.env.QUOTACORE_ADMIN_KEY! }).admin;

for await (const feature of admin.paginate("listFeatures", { query: { limit: 50 } })) {
  console.log(feature.feature_key, feature.state);
}

const { features, next_cursor } = await admin.listFeatures({ query: { limit: 10 } });
console.log(features.length, next_cursor);
```

## Options

| Option | Default | Meaning |
| --- | --- | --- |
| `baseUrl` | required | The service, for example `http://localhost:8080` |
| `apiKey` | required | A `runtime` key, or an `admin` key for `client.admin` |
| `maxAttempts` | `3` | Attempts in total, from 1 to 10 |
| `timeoutMs` | `5000` | The limit on each attempt |
| `baseDelayMs`, `maxDelayMs` | `100`, `2000` | The jittered backoff's first and largest ceilings |
| `maxRetryAfterMs` | `10000` | The longest `retry_after_ms` the client waits out; a longer one is returned as the error |
| `fetch` | global `fetch` | A fetch implementation |

## Developing

```sh
npm ci
npm run generate   # after a change to api/openapi.yaml or the error catalogue
npm test           # unit tests, the type checks, and every example in this README
```

[BOUNDARY.md](BOUNDARY.md) says what is generated and what is written by hand. The tests in
`test/live/` run against a real service when `QUOTACORE_SDK_LIVE_URL` and
`QUOTACORE_SDK_ADMIN_KEY` are set, and CI runs them against the reference stack.
