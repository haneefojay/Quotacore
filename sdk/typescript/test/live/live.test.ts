// The guarantees that matter, against a real Quotacore (IP-26 D14): DoD 2, 3, 6, 7 and 8. The
// mutation is applied by the service, and its response is lost by DroppingProxy, which is the
// failure the guarantees exist for.
//
// Skipped unless QUOTACORE_SDK_LIVE_URL and QUOTACORE_SDK_ADMIN_KEY name a running service and a
// bootstrap admin key. CI runs it against the reference stack, with QUOTACORE_SDK_REQUIRE_LIVE set.

import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { after, before, describe, test } from "node:test";
import {
  FeatureNotInPlanError,
  Quotacore,
  QuotaExceededError,
  UnknownOutcomeError,
  type MeteredRequest,
} from "../../src/index.js";
import { DroppingProxy } from "./dropping-proxy.js";

const liveUrl = process.env.QUOTACORE_SDK_LIVE_URL;
const adminKey = process.env.QUOTACORE_SDK_ADMIN_KEY;
const skip = !liveUrl || !adminKey ? "QUOTACORE_SDK_LIVE_URL and QUOTACORE_SDK_ADMIN_KEY are not set" : false;
// CI sets QUOTACORE_SDK_REQUIRE_LIVE, so a misconfigured job fails rather than skipping quietly.
if (skip && process.env.QUOTACORE_SDK_REQUIRE_LIVE) throw new Error(`live suite required: ${skip}`);

describe("against a real Quotacore", { skip }, () => {
  const run = randomBytes(4).toString("hex");
  const featureKey = `sdk.live.${run}`;
  const tenantId = `sdk-live-${run}`;
  const limit = 1000;
  let admin: Quotacore;
  let direct: Quotacore;
  let proxy: DroppingProxy;
  let proxied: string;
  let runtimeKey: string;
  let tenantUuid: string;

  const request = (amount: number): MeteredRequest => ({ tenant_external_id: tenantId, feature_key: featureKey, amount });
  const balance = async () => (await direct.balance({ tenant_external_id: tenantId, feature_key: featureKey })).entries[0]!.balance!; // metered, so never null (DR-050)
  const viaProxy = (maxAttempts: number) =>
    new Quotacore({ baseUrl: proxied, apiKey: runtimeKey, maxAttempts, baseDelayMs: 10 });

  before(async () => {
    admin = new Quotacore({ baseUrl: liveUrl!, apiKey: adminKey! });
    await admin.admin.createFeature({
      body: { feature_key: featureKey, name: "SDK live test", unit: "tokens", description: "IP-26 live suite", kind: "metered", allocation_cap: false },
    });
    await admin.admin.createPlan({
      body: {
        plan_key: `sdk-live-${run}`,
        name: "SDK live test",
        description: "IP-26 live suite",
        entitlements: [{ feature_key: featureKey, limit, reset_interval: "monthly" }],
      },
    });
    const tenant = await admin.admin.createTenant({
      body: { external_id: tenantId, display_name: "SDK live test", plan_key: `sdk-live-${run}`, timezone: "UTC" },
    });
    tenantUuid = tenant.id;
    runtimeKey = (await admin.admin.createKey({ body: { name: `sdk-live-${run}`, scope: "runtime" } })).key;
    direct = new Quotacore({ baseUrl: liveUrl!, apiKey: runtimeKey });
    // A new tenant reaches the data plane within the snapshot refresh (DR-031).
    for (let i = 0; ; i++) {
      try {
        await balance();
        break;
      } catch (e) {
        if (i > 60) throw e;
        await new Promise((r) => setTimeout(r, 500));
      }
    }
    proxy = new DroppingProxy(new URL(liveUrl!));
    proxied = await proxy.start();
  });

  after(() => proxy?.stop());

  test("DoD 2: applied, response lost, retried with the same key: one deduction, one stored response", async () => {
    const start = await balance();
    proxy.drop = 1;
    const r = await viaProxy(3).consume(request(40));
    assert.equal(proxy.dropped >= 1, true);
    assert.equal(r.replayed, true, "the retry replayed the stored response");
    assert.equal(r.consumed, 40);
    assert.equal(await balance(), start - 40, "charged once");
  });

  test("DoD 3: a retry with a regenerated key is charged twice; the SDK's resolve() is charged once", async () => {
    const start = await balance();
    proxy.drop = 1;
    const lost = await viaProxy(1).consume(request(10)).catch((e: unknown) => e);
    assert.ok(lost instanceof UnknownOutcomeError);
    await direct.consume(request(10)); // a caller's own retry: a new logical call, a new key
    assert.equal(await balance(), start - 20, "the mechanism is defeated by a new key");

    const mid = await balance();
    proxy.drop = 1;
    const lost2 = await viaProxy(1).consume(request(10)).catch((e: unknown) => e);
    assert.ok(lost2 instanceof UnknownOutcomeError);
    const settled = (await lost2.resolve()) as { replayed: boolean };
    assert.equal(settled.replayed, true);
    assert.equal(await balance(), mid - 10, "charged once");
  });

  test("DoD 6: every response lost is an unknown outcome, settled by the same key", async () => {
    const start = await balance();
    proxy.drop = 2;
    const e = await viaProxy(2).consume(request(7)).catch((err: unknown) => err);
    assert.ok(e instanceof UnknownOutcomeError, String(e));
    assert.equal(await balance(), start - 7, "the service applied it");
    const settled = (await e.resolve()) as { replayed: boolean; balance: number };
    assert.equal(settled.replayed, true);
    assert.equal(settled.balance, start - 7);
    assert.equal(await balance(), start - 7, "and only once");
    const events = await admin.admin.listUsageEvents({ path: { id: tenantUuid }, query: { idempotency_key: e.idempotencyKey } });
    assert.ok(events.events.length <= 1, "the ledger never holds the operation twice");
  });

  test("DoD 7: paginate() walks a real list to exhaustion", async () => {
    const keys = Array.from({ length: 5 }, (_, i) => `sdk.page.${run}.${i}`);
    for (const k of keys) {
      await admin.admin.createFeature({ body: { feature_key: k, name: k, unit: "calls", description: "page", kind: "metered", allocation_cap: false } });
    }
    const found: string[] = [];
    let pages = 0;
    for await (const f of admin.admin.paginate("listFeatures", { query: { limit: 2 } })) {
      if (f.feature_key.startsWith(`sdk.page.${run}.`)) found.push(f.feature_key);
      pages++;
    }
    assert.deepEqual(found.sort(), keys);
    assert.ok(pages >= 6, "more than one page was read");
  });

  test("DoD 8: a request id on every result and every error", async () => {
    const ulid = /^([0-9A-HJKMNP-TV-Z]{26}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/;
    assert.match((await direct.check(request(1))).requestId, ulid);
    const denied = await direct.consume(request(limit * 10)).catch((e: unknown) => e);
    assert.ok(denied instanceof QuotaExceededError);
    assert.match(denied.requestId, ulid);
    assert.ok(typeof denied.details.cycle_end === "string", "the reset time a caller waits for");
    const missing = await direct.consume({ ...request(1), feature_key: `sdk.absent.${run}` }).catch((e: unknown) => e);
    assert.ok(missing instanceof FeatureNotInPlanError);
  });
});
