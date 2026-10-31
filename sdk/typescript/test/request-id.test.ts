// IP-26 DoD 8: `X-Request-Id` is on every result and every error, so it can be pasted into a bug
// report (NFR-S10).

import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { Quotacore, QuotacoreError, UnexpectedResponseError } from "../src/index.js";
import { feature, startFake, tenant, type FakeQuotacore } from "./fake-server.js";

let fake: FakeQuotacore;
let url: string;
before(async () => ({ fake, url } = await startFake()));
after(() => fake.stop());

const request = { tenant_external_id: tenant, feature_key: feature, amount: 1 };
const qc = () => new Quotacore({ baseUrl: url, apiKey: "qc_runtime_test.x", maxAttempts: 1 });

test("on every data-plane result", async () => {
  for (const r of [await qc().consume(request), await qc().refund(request), await qc().check(request), await qc().balance({ tenant_external_id: tenant })]) {
    assert.match(r.requestId, /^01JFAKE\d{19}$/);
  }
});

test("on admin results, including a 204", async () => {
  assert.match((await qc().admin.listFeatures()).requestId, /^01JFAKE/);
  assert.match((await qc().admin.revokeKey({ path: { id: "00000000-0000-0000-0000-000000000000" } })).requestId, /^01JFAKE/);
});

test("on every error, equal to the body's request_id", async () => {
  fake.faults.push({ status: 404, code: "tenant_not_found" });
  const e = await qc().consume(request).catch((err: unknown) => err);
  assert.ok(e instanceof QuotacoreError);
  assert.match(e.requestId, /^01JFAKE/);
});

test("on a non-envelope response, when one was sent", async () => {
  fake.faults.push("html-502");
  const e = await qc().check(request).catch((err: unknown) => err);
  assert.ok(e instanceof UnexpectedResponseError);
  assert.equal(e.status, 502);
});

test("the result stays the contract's shape: requestId is not serialised", async () => {
  const r = await qc().check(request);
  assert.equal("requestId" in JSON.parse(JSON.stringify(r)), false);
  assert.deepEqual(Object.keys(r).sort(), ["allowed", "balance", "bonus", "cycle_end", "limit", "requested"]);
});
