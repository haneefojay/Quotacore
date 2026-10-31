// IP-26 DoD 3: the misuse the SDK exists to prevent. A retry that mints a new key is a second
// operation and is charged twice (FS-03); the same failure resolved through the SDK is charged
// once. The test shows the mechanism defeated, so the protection is not assumed.

import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { Quotacore, UnknownOutcomeError } from "../src/index.js";
import { feature, recordingSleep, startFake, tenant, type FakeQuotacore } from "./fake-server.js";

let fake: FakeQuotacore;
let url: string;
before(async () => ({ fake, url } = await startFake()));
after(() => fake.stop());

const request = { tenant_external_id: tenant, feature_key: feature, amount: 100 };

test("a hand-rolled retry with a new key is charged twice", async () => {
  fake.balances.set(`${tenant}|${feature}`, 1000);
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", maxAttempts: 1 });
  fake.faults.push("drop-after-apply");
  const first = await qc.consume(request).catch((e: unknown) => e);
  assert.ok(first instanceof UnknownOutcomeError);
  // What a caller's own retry loop around the SDK does: call again, which is a new logical call.
  const again = await qc.consume(request);
  assert.equal(again.replayed, false, "the service saw a new operation");
  assert.equal(fake.balances.get(`${tenant}|${feature}`), 800, "charged twice: the mechanism is defeated");
});

test("the same failure settled with resolve() is charged once", async () => {
  fake.balances.set(`${tenant}|${feature}`, 1000);
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", maxAttempts: 1 });
  fake.faults.push("drop-after-apply");
  const first = await qc.consume(request).catch((e: unknown) => e);
  assert.ok(first instanceof UnknownOutcomeError);
  const settled = (await first.resolve()) as { replayed: boolean };
  assert.equal(settled.replayed, true);
  assert.equal(fake.balances.get(`${tenant}|${feature}`), 900, "charged once");
});

test("the SDK's own retries never mint a second key", async () => {
  const before = fake.seen.length;
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", maxAttempts: 5, sleep: recordingSleep().sleep });
  fake.faults.push("drop-before-apply", "drop-after-apply", { status: 503, code: "service_unavailable" }, "html-502");
  await qc.consume(request);
  const keys = new Set(fake.seen.slice(before).map((s) => s.idempotencyKey));
  assert.equal(fake.seen.length - before, 5);
  assert.equal(keys.size, 1, "five attempts, one key");
});

