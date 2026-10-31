// IP-26 DoD 6: a transport failure is an explicitly unknown outcome, and the SDK offers the
// request that settles it. That request is the same one with the same key, which the service
// replays if the first was applied (DR-027); see the amendment to DoD 6 in v0-5-sdks.md.

import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { IdempotencyWindowError, Quotacore, QuotaExceededError, TransportError, UnknownOutcomeError } from "../src/index.js";
import { feature, recordingSleep, startFake, tenant, type FakeQuotacore } from "./fake-server.js";

let fake: FakeQuotacore;
let url: string;
before(async () => ({ fake, url } = await startFake()));
after(() => fake.stop());

const request = { tenant_external_id: tenant, feature_key: feature, amount: 10 };
const key = () => `${tenant}|${feature}`;

test("applied and then the connection dropped on every attempt: unknown, and resolve() replays it", async () => {
  fake.balances.set(key(), 100);
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", maxAttempts: 2, sleep: recordingSleep().sleep });
  fake.faults.push("drop-after-apply", "drop-after-apply");
  const e = await qc.consume(request).catch((err: unknown) => err);
  assert.ok(e instanceof UnknownOutcomeError, String(e));
  assert.ok(e.cause instanceof TransportError);
  assert.equal(fake.balances.get(key()), 90, "the service applied it once");
  const settled = (await e.resolve()) as { replayed: boolean; balance: number; requestId: string };
  assert.equal(settled.replayed, true);
  assert.equal(settled.balance, 90);
  assert.ok(settled.requestId);
  assert.equal(fake.balances.get(key()), 90, "still once");
  assert.equal(fake.seen.at(-1)!.idempotencyKey, e.idempotencyKey);
});

test("dropped before it was applied: unknown, and resolve() applies it once", async () => {
  fake.balances.set(key(), 100);
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", maxAttempts: 1 });
  fake.faults.push("drop-before-apply");
  const e = await qc.consume(request).catch((err: unknown) => err);
  assert.ok(e instanceof UnknownOutcomeError);
  assert.equal(fake.balances.get(key()), 100);
  const settled = (await e.resolve()) as { replayed: boolean };
  assert.equal(settled.replayed, false);
  assert.equal(fake.balances.get(key()), 90);
});

test("a 503 on the last attempt after a dropped one is still unknown", async () => {
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", maxAttempts: 2, sleep: recordingSleep().sleep });
  fake.faults.push("drop-after-apply", { status: 503, code: "service_unavailable" });
  assert.ok((await qc.consume(request).catch((err: unknown) => err)) instanceof UnknownOutcomeError);
});

test("a 500 after a dropped attempt is still unknown: the 500 says only that attempt changed nothing", async () => {
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", maxAttempts: 2, sleep: recordingSleep().sleep });
  fake.faults.push("drop-after-apply", { status: 500, code: "internal_error" });
  assert.ok((await qc.consume(request).catch((err: unknown) => err)) instanceof UnknownOutcomeError);
});

test("a definitive 4xx after a dropped attempt settles it, and is returned as itself", async () => {
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", maxAttempts: 2, sleep: recordingSleep().sleep });
  fake.faults.push("drop-before-apply", { status: 429, code: "quota_exceeded" });
  assert.ok((await qc.consume(request).catch((err: unknown) => err)) instanceof QuotaExceededError);
});

test("a 500 alone is not unknown: a mutation that answers 500 was not applied", async () => {
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", maxAttempts: 1 });
  fake.faults.push({ status: 500, code: "internal_error" });
  const e = await qc.consume(request).catch((err: unknown) => err);
  assert.equal((e as { code: string }).code, "internal_error");
});

test("a read never raises UnknownOutcomeError, because it has nothing to settle", async () => {
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", maxAttempts: 1 });
  fake.faults.push("drop-before-apply");
  assert.ok((await qc.check(request).catch((err: unknown) => err)) instanceof TransportError);
});

test("resolve() refuses once the 24-hour window may have closed", async () => {
  let now = 1_000_000;
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", maxAttempts: 1, now: () => now });
  fake.faults.push("drop-after-apply");
  const e = await qc.consume(request).catch((err: unknown) => err);
  assert.ok(e instanceof UnknownOutcomeError);
  const sent = fake.seen.length;
  now += 23 * 60 * 60 * 1000 + 1;
  await assert.rejects(e.resolve(), IdempotencyWindowError);
  assert.equal(fake.seen.length, sent, "nothing was re-issued");
});

test("resolve() that fails again keeps the first attempt's time", async () => {
  let now = 5_000;
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", maxAttempts: 1, now: () => now });
  fake.faults.push("drop-after-apply", "drop-after-apply");
  const e = (await qc.consume(request).catch((err: unknown) => err)) as UnknownOutcomeError;
  now = 9_000;
  const again = await e.resolve().catch((err: unknown) => err);
  assert.ok(again instanceof UnknownOutcomeError);
  assert.equal(again.firstAttemptAt, 5_000);
  assert.equal(again.idempotencyKey, e.idempotencyKey);
});

test("a timeout is a transport failure, so the outcome is unknown", async () => {
  const hang: typeof fetch = (input, init) =>
    new Promise((_, reject) => init?.signal?.addEventListener("abort", () => reject(init.signal!.reason)));
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", maxAttempts: 1, timeoutMs: 20, fetch: hang });
  const e = await qc.consume(request).catch((err: unknown) => err);
  assert.ok(e instanceof UnknownOutcomeError);
  assert.match((e.cause as Error).message, /timed out after 20 ms/);
});
