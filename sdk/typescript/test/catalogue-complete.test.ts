// IP-26 DoD 5: every catalogue code maps to a typed error carrying the code and the catalogue's
// client action, and the mapping is complete in both directions.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import yaml from "js-yaml";
import * as sdk from "../src/index.js";
import { errorClasses } from "../src/generated/errors.js";
import { exchange } from "../src/transport.js";
import { repoFile } from "./paths.js";

const contractCodes = (yaml.load(readFileSync(repoFile("api/openapi.yaml"), "utf8")) as any).components.schemas
  .ErrorCode.enum as string[];

test("the SDK's codes are the contract's ErrorCode enum, in both directions", () => {
  assert.deepEqual([...sdk.errorCodes].sort(), [...contractCodes].sort());
  assert.deepEqual(Object.keys(errorClasses).sort(), [...contractCodes].sort());
  assert.deepEqual(Object.keys(sdk.catalogue).sort(), [...contractCodes].sort());
});

test("every code has its own exported class, and every exported error class has its own code", () => {
  const classes = Object.values(errorClasses);
  assert.equal(new Set(classes).size, classes.length, "no two codes share a class");
  const exported = Object.entries(sdk).filter(
    ([, v]) => typeof v === "function" && v.prototype instanceof sdk.QuotacoreError,
  );
  assert.equal(exported.length, contractCodes.length, "one exported subclass per code");
  for (const [name, cls] of exported) {
    const codes = Object.entries(errorClasses).filter(([, c]) => c === cls).map(([code]) => code);
    assert.equal(codes.length, 1, `${name} maps to exactly one code`);
  }
});

test("each code's envelope is raised as its class, carrying the code and the client action", async () => {
  for (const code of contractCodes) {
    const entry = sdk.catalogue[code as sdk.ErrorCode];
    const fakeFetch: typeof fetch = async () =>
      new Response(JSON.stringify({ error: { code, message: "m", request_id: "01JREQ" } }), {
        status: entry.status,
        headers: { "x-request-id": "01JREQ" },
      });
    const err = await exchange(fakeFetch, { method: "GET", url: "http://x/v1/balance", headers: {}, timeoutMs: 1000 }).catch(
      (e: unknown) => e,
    );
    assert.ok(err instanceof errorClasses[code as sdk.ErrorCode], code);
    assert.ok(err instanceof sdk.QuotacoreError);
    assert.equal(err.code, code);
    assert.equal(err.status, entry.status);
    assert.ok(err.clientAction && err.clientAction.length > 0, `${code} has a client action`);
    assert.equal(err.clientAction, entry.clientAction);
  }
});

test("a code newer than this package is a plain QuotacoreError, not a crash", async () => {
  const fakeFetch: typeof fetch = async () =>
    new Response(JSON.stringify({ error: { code: "brand_new_code", message: "m", request_id: "r" } }), { status: 409 });
  const err = await exchange(fakeFetch, { method: "GET", url: "http://x/v1/balance", headers: {}, timeoutMs: 1000 }).catch(
    (e: unknown) => e,
  );
  assert.ok(err instanceof sdk.QuotacoreError);
  assert.equal(err.constructor, sdk.QuotacoreError);
  assert.equal(err.code, "brand_new_code");
  assert.equal(err.clientAction, undefined);
});

test("a quota denial and a transport failure share no base class", () => {
  const q = new sdk.QuotaExceededError(429, { code: "quota_exceeded", message: "", request_id: "r" });
  const t = new sdk.TransportError("down");
  assert.ok(!(q instanceof sdk.TransportError));
  assert.ok(!(t instanceof sdk.QuotacoreError));
  assert.ok(!(q instanceof sdk.UnknownOutcomeError));
  assert.equal(q.name, "QuotaExceededError");
});
