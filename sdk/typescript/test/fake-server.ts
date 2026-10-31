// An in-process stand-in for the service, faithful to the parts of the contract the SDK's
// guarantees rest on: the replay rules of DR-027 and DR-028, the error envelope, X-Request-Id, and
// cursor pagination. It also injects the faults those guarantees exist for.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";

/** What the next request suffers. Faults are taken in order, one per request. */
export type Fault =
  | "drop-before-apply"
  | "drop-after-apply"
  | "html-502"
  | { status: number; code: string; retry_after_ms?: number };

export interface Seen {
  method: string;
  path: string;
  idempotencyKey: string | undefined;
  body: unknown;
}

interface Stored {
  fingerprint: string;
  response: Record<string, unknown>;
}

export class FakeQuotacore {
  readonly seen: Seen[] = [];
  readonly faults: Fault[] = [];
  /** Remaining balance by `tenant|feature`. */
  readonly balances = new Map<string, number>();
  /** Stored idempotency records by `tenant|key`. */
  readonly records = new Map<string, Stored>();
  /** The admin feature catalogue, newest first, for pagination. */
  features: { feature_key: string }[] = [];
  limit = 1000;
  /** Tenants the data plane answers `404 tenant_not_found` for. */
  readonly unknownTenants = new Set(["no-such-tenant"]);
  #server: Server;
  #requests = 0;

  constructor() {
    this.#server = createServer((req, res) => void this.#handle(req, res));
  }

  async start(): Promise<string> {
    await new Promise<void>((r) => this.#server.listen(0, "127.0.0.1", r));
    return `http://127.0.0.1:${(this.#server.address() as AddressInfo).port}`;
  }

  async stop(): Promise<void> {
    this.#server.closeAllConnections();
    await new Promise<void>((r) => this.#server.close(() => r()));
  }

  /** How many attempts reached the server for a path. */
  attempts(path: string): number {
    return this.seen.filter((s) => s.path === path).length;
  }

  async #handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const chunks: Buffer[] = [];
    for await (const c of req) chunks.push(c as Buffer);
    const text = Buffer.concat(chunks).toString("utf8");
    const url = new URL(req.url ?? "/", "http://fake");
    const body = text ? JSON.parse(text) : undefined;
    const idempotencyKey = req.headers["idempotency-key"] as string | undefined;
    this.seen.push({ method: req.method ?? "", path: url.pathname, idempotencyKey, body });
    const requestId = `01JFAKE${String(++this.#requests).padStart(19, "0")}`;
    const fault = this.faults.shift();

    if (fault === "drop-before-apply") return void req.socket.destroy();
    if (fault === "html-502") {
      res.writeHead(502, { "content-type": "text/html" });
      return void res.end("<html>bad gateway</html>");
    }
    if (fault && typeof fault === "object") {
      return this.#error(res, requestId, fault.status, fault.code, fault.retry_after_ms);
    }

    const answer = this.#route(req.method ?? "", url, body, idempotencyKey);
    if (fault === "drop-after-apply") return void req.socket.destroy();
    if (answer.status === 204) {
      res.writeHead(204, { "x-request-id": requestId });
      return void res.end();
    }
    if ("error" in answer) return this.#error(res, requestId, answer.status, answer.error);
    const headers: Record<string, string> = { "content-type": "application/json", "x-request-id": requestId };
    if (answer.body.replayed === true) headers["idempotent-replay"] = "true";
    res.writeHead(answer.status, headers);
    res.end(JSON.stringify(answer.body));
  }

  #error(res: ServerResponse, requestId: string, status: number, code: string, retryAfterMs?: number): void {
    const error: Record<string, unknown> = { code, message: `fake ${code}`, request_id: requestId };
    if (retryAfterMs !== undefined) error.retry_after_ms = retryAfterMs;
    res.writeHead(status, { "content-type": "application/json", "x-request-id": requestId });
    res.end(JSON.stringify({ error }));
  }

  #route(
    method: string,
    url: URL,
    body: any,
    key: string | undefined,
  ): { status: number; body: Record<string, unknown> } | { status: number; error: string } {
    const path = url.pathname;
    if (body && this.unknownTenants.has(body.tenant_external_id)) return { status: 404, error: "tenant_not_found" };
    if (method === "POST" && (path === "/v1/consume" || path === "/v1/refund")) {
      if (!key) return { status: 400, error: "missing_idempotency_key" };
      const op = path.slice(4);
      const fingerprint = [op, body.tenant_external_id, body.feature_key, body.amount].join("|");
      const recordKey = `${body.tenant_external_id}|${key}`;
      const stored = this.records.get(recordKey);
      if (stored) {
        if (stored.fingerprint !== fingerprint) return { status: 409, error: "idempotency_key_reuse" };
        return { status: 200, body: { ...stored.response, replayed: true } };
      }
      const bal = this.#balance(body.tenant_external_id, body.feature_key);
      const next = op === "consume" ? bal - body.amount : bal + body.amount;
      if (op === "consume" && next < 0) return { status: 429, error: "quota_exceeded" };
      this.balances.set(`${body.tenant_external_id}|${body.feature_key}`, next);
      const response = this.#entry(body.tenant_external_id, body.feature_key, body.amount);
      this.records.set(recordKey, { fingerprint, response });
      return { status: 200, body: { ...response, replayed: false } };
    }
    if (method === "POST" && path === "/v1/check") {
      const bal = this.#balance(body.tenant_external_id, body.feature_key);
      return {
        status: 200,
        body: { allowed: bal >= body.amount, kind: "metered", balance: bal, limit: this.limit, bonus: 0, requested: body.amount, cycle_end: null },
      };
    }
    if (method === "GET" && path === "/v1/balance") {
      const tenant = url.searchParams.get("tenant_external_id") ?? "";
      const feature = url.searchParams.get("feature_key") ?? "llm.tokens.output";
      const { tenant_external_id, ...entry } = this.#entry(tenant, feature, 0);
      return { status: 200, body: { tenant_external_id, entries: [entry] } };
    }
    if (method === "GET" && path === "/v1/admin/features") {
      const limit = Number(url.searchParams.get("limit") ?? 50);
      const start = Number(url.searchParams.get("cursor") ?? 0);
      const page = this.features.slice(start, start + limit);
      const end = start + page.length;
      return { status: 200, body: { features: page, next_cursor: end < this.features.length ? String(end) : null } };
    }
    if (method === "POST" && path === "/v1/admin/tenants") {
      return { status: 201, body: { external_id: body.external_id } };
    }
    if (method === "DELETE" && path.startsWith("/v1/admin/keys/")) {
      return { status: 204, body: {} };
    }
    return { status: 400, error: "validation_failed" };
  }

  #balance(tenant: string, feature: string): number {
    return this.balances.get(`${tenant}|${feature}`) ?? this.limit;
  }

  #entry(tenant: string, feature: string, amount: number): Record<string, unknown> {
    return {
      tenant_external_id: tenant,
      feature_key: feature,
      kind: "metered",
      balance: this.#balance(tenant, feature),
      limit: this.limit,
      bonus: 0,
      consumed: amount,
      cycle_start: "2026-10-01T00:00:00Z",
      cycle_end: "2026-11-01T00:00:00Z",
    };
  }
}

/** Starts a fake and returns it with its URL; call `stop()` when done. */
export async function startFake(): Promise<{ fake: FakeQuotacore; url: string }> {
  const fake = new FakeQuotacore();
  return { fake, url: await fake.start() };
}

/** A sleep that records each wait and does not wait. */
export function recordingSleep(): { sleep: (ms: number) => Promise<void>; waits: number[] } {
  const waits: number[] = [];
  return { waits, sleep: async (ms: number) => void waits.push(ms) };
}

export const tenant = "acme-prod-01";
export const feature = "llm.tokens.output";
