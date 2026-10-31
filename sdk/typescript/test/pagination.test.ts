// IP-26 DoD 7: pagination follows the contract's `next_cursor`, and a walk to exhaustion
// reassembles the whole set.

import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { Quotacore } from "../src/index.js";
import { startFake, type FakeQuotacore } from "./fake-server.js";

let fake: FakeQuotacore;
let url: string;
before(async () => ({ fake, url } = await startFake()));
after(() => fake.stop());

test("paginate() pages to exhaustion and reassembles the set in order", async () => {
  fake.features = Array.from({ length: 120 }, (_, i) => ({ feature_key: `f.${String(i).padStart(3, "0")}` }));
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_admin_test.x" });
  const start = fake.seen.length;
  const keys: string[] = [];
  for await (const f of qc.admin.paginate("listFeatures", { query: { limit: 50 } })) keys.push(f.feature_key);
  assert.deepEqual(keys, fake.features.map((f) => f.feature_key));
  const pages = fake.seen.slice(start);
  assert.equal(pages.length, 3, "50 + 50 + 20");
});

test("one page at a time, as the iteration reaches it", async () => {
  fake.features = Array.from({ length: 120 }, (_, i) => ({ feature_key: `g.${i}` }));
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_admin_test.x" });
  const start = fake.seen.length;
  for await (const f of qc.admin.paginate("listFeatures", { query: { limit: 50 } })) {
    assert.equal(f.feature_key, "g.0");
    break;
  }
  assert.equal(fake.seen.length - start, 1);
});

test("an empty list is an empty iteration", async () => {
  fake.features = [];
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_admin_test.x" });
  const seen: unknown[] = [];
  for await (const f of qc.admin.paginate("listFeatures")) seen.push(f);
  assert.deepEqual(seen, []);
});

test("a single page call returns next_cursor for the caller to follow", async () => {
  fake.features = Array.from({ length: 3 }, (_, i) => ({ feature_key: `h.${i}` }));
  const qc = new Quotacore({ baseUrl: url, apiKey: "qc_admin_test.x" });
  const page = await qc.admin.listFeatures({ query: { limit: 2 } });
  assert.equal(page.features.length, 2);
  assert.equal(page.next_cursor, "2");
  const last = await qc.admin.listFeatures({ query: { limit: 2, cursor: page.next_cursor! } });
  assert.equal(last.next_cursor, null);
});
