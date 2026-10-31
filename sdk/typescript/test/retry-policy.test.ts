// IP-26 DoD 4: retries only on 5xx and `429 rate_limited`, never on another 4xx, bounded, with
// jitter (DR-040).

import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { Quotacore, QuotaExceededError, RateLimitedError, ServiceUnavailableError, ValidationFailedError, defaultRetryPolicy } from "../src/index.js";
import { nextDelay } from "../src/retry.js";
import { feature, recordingSleep, startFake, tenant, type FakeQuotacore } from "./fake-server.js";

let fake: FakeQuotacore;
let url: string;
before(async () => ({ fake, url } = await startFake()));
after(() => fake.stop());

const request = { tenant_external_id: tenant, feature_key: feature, amount: 1 };
const client = (extra: Partial<ConstructorParameters<typeof Quotacore>[0]> = {}) => {
  const rec = recordingSleep();
  return { qc: new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", sleep: rec.sleep, random: () => 0.5, ...extra }), waits: rec.waits };
};
const attemptsOf = async (fn: () => Promise<unknown>) => {
  const start = fake.seen.length;
  const outcome = await fn().then((v) => v, (e: unknown) => e);
  return { outcome, attempts: fake.seen.length - start };
};

test("a 400 is returned on the first attempt", async () => {
  const { qc, waits } = client();
  fake.faults.push({ status: 400, code: "validation_failed" });
  const { outcome, attempts } = await attemptsOf(() => qc.consume(request));
  assert.ok(outcome instanceof ValidationFailedError);
  assert.equal(attempts, 1);
  assert.equal(waits.length, 0);
});

test("quota_exceeded is never retried", async () => {
  const { qc } = client();
  fake.faults.push({ status: 429, code: "quota_exceeded" });
  const { outcome, attempts } = await attemptsOf(() => qc.consume(request));
  assert.ok(outcome instanceof QuotaExceededError);
  assert.equal(attempts, 1);
});

test("no 4xx in the catalogue other than rate_limited is retried", async () => {
  const { qc } = client();
  for (const [status, code] of [[401, "api_key_invalid"], [403, "feature_not_in_plan"], [404, "tenant_not_found"], [409, "idempotency_key_reuse"], [413, "payload_too_large"]] as const) {
    fake.faults.push({ status, code });
    const { attempts } = await attemptsOf(() => qc.consume(request));
    assert.equal(attempts, 1, code);
  }
});

test("501 not_implemented is not retried", async () => {
  const { qc } = client();
  fake.faults.push({ status: 501, code: "not_implemented" });
  assert.equal((await attemptsOf(() => qc.check(request))).attempts, 1);
});

test("a 5xx is retried, and the attempts are bounded", async () => {
  const { qc, waits } = client({ maxAttempts: 3 });
  for (let i = 0; i < 5; i++) fake.faults.push({ status: 503, code: "control_plane_unavailable" });
  const { outcome, attempts } = await attemptsOf(() => qc.check(request));
  assert.equal(attempts, 3);
  assert.equal(waits.length, 2);
  assert.equal((outcome as { code: string }).code, "control_plane_unavailable");
  fake.faults.length = 0;
});

test("a 5xx that clears is answered", async () => {
  const { qc } = client();
  fake.faults.push({ status: 500, code: "internal_error" }, { status: 503, code: "service_unavailable" });
  const { outcome, attempts } = await attemptsOf(() => qc.consume(request));
  assert.equal(attempts, 3);
  assert.equal((outcome as { replayed: boolean }).replayed, false);
});

test("rate_limited is retried, waiting at least retry_after_ms", async () => {
  const { qc, waits } = client();
  fake.faults.push({ status: 429, code: "rate_limited", retry_after_ms: 750 });
  const { attempts } = await attemptsOf(() => qc.balance({ tenant_external_id: tenant }));
  assert.equal(attempts, 2);
  assert.ok(waits[0]! >= 750, `waited ${waits[0]}`);
});

test("a retry_after_ms longer than the client accepts is returned, not waited out", async () => {
  const { qc, waits } = client({ maxRetryAfterMs: 1000 });
  fake.faults.push({ status: 429, code: "rate_limited", retry_after_ms: 60_000 });
  const { outcome, attempts } = await attemptsOf(() => qc.balance({ tenant_external_id: tenant }));
  assert.ok(outcome instanceof RateLimitedError);
  assert.equal(outcome.retryAfterMs, 60_000);
  assert.equal(attempts, 1);
  assert.equal(waits.length, 0);
});

test("an admin write is not retried on a 5xx, which may have applied it", async () => {
  const { qc } = client();
  fake.faults.push({ status: 503, code: "service_unavailable" });
  const { outcome, attempts } = await attemptsOf(() =>
    qc.admin.createTenant({ body: { external_id: "t-1", display_name: "T", plan_key: "free", timezone: "UTC" } }),
  );
  assert.ok(outcome instanceof ServiceUnavailableError);
  assert.equal(attempts, 1);
});

test("an admin write is retried on rate_limited, which is answered before anything is done", async () => {
  const { qc } = client();
  fake.faults.push({ status: 429, code: "rate_limited", retry_after_ms: 10 });
  const { attempts } = await attemptsOf(() =>
    qc.admin.createTenant({ body: { external_id: "t-2", display_name: "T", plan_key: "free", timezone: "UTC" } }),
  );
  assert.equal(attempts, 2);
});

test("the backoff is full jitter under a doubling ceiling", () => {
  const p = { ...defaultRetryPolicy, maxAttempts: 10 };
  const err = new ServiceUnavailableError(503, { code: "service_unavailable", message: "", request_id: "r" });
  for (let attempt = 1; attempt <= 9; attempt++) {
    const ceiling = Math.min(p.maxDelayMs, p.baseDelayMs * 2 ** (attempt - 1));
    assert.equal(nextDelay(err, attempt, p, () => 0), 0);
    assert.equal(nextDelay(err, attempt, p, () => 0.999999), Math.floor(0.999999 * ceiling));
  }
  assert.equal(nextDelay(err, 10, p, () => 0.5), undefined, "no attempt past maxAttempts");
});

test("maxAttempts is bounded", () => {
  assert.throws(() => new Quotacore({ baseUrl: url, apiKey: "k", maxAttempts: 0 }), RangeError);
  assert.throws(() => new Quotacore({ baseUrl: url, apiKey: "k", maxAttempts: 11 }), RangeError);
});
