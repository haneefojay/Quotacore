// IP-26 DoD 2: a retry reuses the idempotency key, so a mutation the service applied before the
// connection dropped is charged once and stored once (DR-027, the T-02 property).

import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { Quotacore } from "../src/index.js";
import { feature, recordingSleep, startFake, tenant, type FakeQuotacore } from "./fake-server.js";

let fake: FakeQuotacore;
let url: string;
before(async () => ({ fake, url } = await startFake()));
after(() => fake.stop());

for (const op of ["consume", "refund"] as const) {
  test(`${op}: applied, connection dropped, retried with the same key, charged once`, async () => {
    fake.balances.set(`${tenant}|${feature}`, 500);
    const before = fake.seen.length;
    const { sleep } = recordingSleep();
    const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", sleep });
    fake.faults.push("drop-after-apply");

    const r = await qc[op]({ tenant_external_id: tenant, feature_key: feature, amount: 40 });

    const attempts = fake.seen.slice(before);
    assert.equal(attempts.length, 2, "one dropped attempt and one retry");
    assert.ok(attempts[0]!.idempotencyKey, "the first attempt carried a key");
    assert.equal(attempts[1]!.idempotencyKey, attempts[0]!.idempotencyKey, "the retry carried the same key");
    assert.equal(r.replayed, true, "the retry was a replay, not a second application");
    assert.equal(r.consumed, 40);
    const expected = op === "consume" ? 460 : 540;
    assert.equal(fake.balances.get(`${tenant}|${feature}`), expected, "one deduction");
    assert.equal(
      [...fake.records.keys()].filter((k) => k === `${tenant}|${attempts[0]!.idempotencyKey}`).length,
      1,
      "one stored response",
    );
  });
}

test("a caller-supplied key is the one every attempt carries", async () => {
  const before = fake.seen.length;
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", sleep: recordingSleep().sleep });
  fake.faults.push({ status: 503, code: "service_unavailable" }, "drop-after-apply");
  await qc.consume({ tenant_external_id: tenant, feature_key: feature, amount: 1 }, { idempotencyKey: "order:8841:charge" });
  const keys = fake.seen.slice(before).map((s) => s.idempotencyKey);
  assert.deepEqual(keys, ["order:8841:charge", "order:8841:charge", "order:8841:charge"]);
});

test("a key the contract would refuse is rejected before anything is sent", async () => {
  const before = fake.seen.length;
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x" });
  await assert.rejects(
    qc.consume({ tenant_external_id: tenant, feature_key: feature, amount: 1 }, { idempotencyKey: "short" }),
    TypeError,
  );
  assert.equal(fake.seen.length, before);
});

test("an amount that is not a safe integer is rejected before anything is sent", async () => {
  const before = fake.seen.length;
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x" });
  for (const amount of [4200.5, Number.MAX_SAFE_INTEGER + 1, Number.NaN]) {
    await assert.rejects(qc.consume({ tenant_external_id: tenant, feature_key: feature, amount }), TypeError);
    await assert.rejects(qc.check({ tenant_external_id: tenant, feature_key: feature, amount }), TypeError);
  }
  assert.equal(fake.seen.length, before);
});
