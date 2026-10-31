// The Quotacore client: the four data-plane calls, the admin client, and the one loop that retries
// all of them (IP-26 D4 to D7).

import { createAdmin, type AdminClient, type OperationResult } from "./admin.js";
import { IdempotencyWindowError, QuotacoreError, UnknownOutcomeError } from "./errors.js";
import type { components } from "./generated/schema.js";
import { contractVersion, routes, type OperationId } from "./generated/routes.js";
import { checkIdempotencyKey, newIdempotencyKey } from "./idempotency.js";
import { defaultRetryPolicy, isAmbiguous, nextDelay, validatePolicy, type RetryPolicy } from "./retry.js";
import { exchange } from "./transport.js";

type Schemas = components["schemas"];
export type MeteredRequest = Schemas["MeteredRequest"];
export type MeteredResponse = Schemas["MeteredResponse"];
export type CheckResponse = Schemas["CheckResponse"];
export type BalanceResponse = Schemas["BalanceResponse"];

/** The service's idempotency window is 24 hours (DR-029); `resolve()` stops an hour short of it. */
export const resolveWindowMs = 23 * 60 * 60 * 1000;

export interface QuotacoreOptions {
  /** The service's origin, for example `http://localhost:8080`. */
  baseUrl: string;
  /** A `runtime` key for the data plane, or an `admin` key, which also works there. */
  apiKey: string;
  /** Attempts in total, including the first. Default 3, at most 10. */
  maxAttempts?: number;
  /** The limit on each attempt. Default 5000 ms. */
  timeoutMs?: number;
  /** The first backoff ceiling. Default 100 ms. */
  baseDelayMs?: number;
  /** The largest backoff ceiling. Default 2000 ms. */
  maxDelayMs?: number;
  /** The longest `retry_after_ms` the client will wait out. Default 10000 ms. */
  maxRetryAfterMs?: number;
  /** A fetch implementation. Default the global `fetch`. */
  fetch?: typeof fetch;
  /** For tests: the source of jitter, the wait, and the clock. */
  random?: () => number;
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
  now?: () => number;
}

export interface MutationOptions {
  /**
   * The key for this logical operation. Supply one derived from your own durable identifier when
   * you have one, such as `order:8841:charge`; otherwise a UUID is generated. Either way it is
   * fixed for every attempt of this call.
   */
  idempotencyKey?: string;
  signal?: AbortSignal;
}

interface Call {
  operation: OperationId;
  args: Record<string, unknown>;
  idempotencyKey?: string;
}

export class Quotacore {
  /** Every `/v1/admin/` operation in the contract. Needs an `admin` key. */
  readonly admin: AdminClient;
  /** The contract version this package was generated from. */
  static readonly contractVersion = contractVersion;

  readonly #base: string;
  readonly #apiKey: string;
  readonly #policy: RetryPolicy;
  readonly #timeoutMs: number;
  readonly #fetch: typeof fetch;
  readonly #random: () => number;
  readonly #sleep: (ms: number, signal?: AbortSignal) => Promise<void>;
  readonly #now: () => number;

  constructor(options: QuotacoreOptions) {
    if (!options?.baseUrl) throw new TypeError("baseUrl is required");
    if (!options.apiKey) throw new TypeError("apiKey is required");
    this.#base = options.baseUrl.replace(/\/+$/, "");
    this.#apiKey = options.apiKey;
    this.#policy = validatePolicy({
      maxAttempts: options.maxAttempts ?? defaultRetryPolicy.maxAttempts,
      baseDelayMs: options.baseDelayMs ?? defaultRetryPolicy.baseDelayMs,
      maxDelayMs: options.maxDelayMs ?? defaultRetryPolicy.maxDelayMs,
      maxRetryAfterMs: options.maxRetryAfterMs ?? defaultRetryPolicy.maxRetryAfterMs,
    });
    this.#timeoutMs = options.timeoutMs ?? 5_000;
    this.#fetch = options.fetch ?? globalThis.fetch;
    this.#random = options.random ?? Math.random;
    this.#sleep = options.sleep ?? sleep;
    this.#now = options.now ?? Date.now;
    this.admin = createAdmin((operation, args) => this.#run({ operation, args }));
  }

  /**
   * Deducts `amount` from the tenant's current cycle. Retries reuse one `Idempotency-Key`, so the
   * call is charged once however many attempts it takes. Throws `UnknownOutcomeError` when the
   * attempts ran out without a definitive answer; its `resolve()` settles the outcome safely.
   */
  consume(request: MeteredRequest, options?: MutationOptions): Promise<MeteredResponse & { readonly requestId: string }> {
    return this.#mutate("consume", request, options);
  }

  /** Returns `amount` to the tenant's current cycle, with the same retry guarantee as `consume`. */
  refund(request: MeteredRequest, options?: MutationOptions): Promise<MeteredResponse & { readonly requestId: string }> {
    return this.#mutate("refund", request, options);
  }

  /**
   * Asks whether `amount` would be allowed. A denial is `allowed: false`, not an error (DR-024).
   * Advisory: two calls can both pass and one then be refused at `consume`.
   */
  async check(request: MeteredRequest, options?: { signal?: AbortSignal }): Promise<OperationResult<"check">> {
    checkAmount(request);
    return this.#run<"check">({ operation: "check", args: { body: request, signal: options?.signal } });
  }

  /** The tenant's balances: one entry with `feature_key`, otherwise the full set. */
  balance(
    query: { tenant_external_id: string; feature_key?: string },
    options?: { signal?: AbortSignal },
  ): Promise<OperationResult<"balance">> {
    return this.#run<"balance">({ operation: "balance", args: { query, signal: options?.signal } });
  }

  async #mutate(
    operation: "consume" | "refund",
    request: MeteredRequest,
    options?: MutationOptions,
  ): Promise<MeteredResponse & { readonly requestId: string }> {
    checkAmount(request);
    const idempotencyKey =
      options?.idempotencyKey === undefined ? newIdempotencyKey() : checkIdempotencyKey(options.idempotencyKey);
    return this.#run<"consume">({ operation, args: { body: request, signal: options?.signal }, idempotencyKey });
  }

  async #run<K extends OperationId>(call: Call, firstAttemptAt = this.#now()): Promise<OperationResult<K>> {
    const route = routes[call.operation];
    const signal = call.args.signal as AbortSignal | undefined;
    // A request is repeatable when repeating it cannot apply it twice: a read, `check`, or a
    // mutation carrying its idempotency key. An admin write is retried only on `rate_limited`,
    // which the service answers before doing anything.
    const repeatable = route.method === "GET" || call.operation === "check" || route.idempotent;
    const headers: Record<string, string> = {
      authorization: `Bearer ${this.#apiKey}`,
      accept: "application/json",
      "user-agent": `quotacore-sdk-typescript/${contractVersion}`,
    };
    if (call.idempotencyKey) headers["idempotency-key"] = call.idempotencyKey;
    let body: string | undefined;
    if (call.args.body !== undefined) {
      headers["content-type"] = "application/json";
      body = JSON.stringify(call.args.body);
    }
    const url = this.#base + buildPath(route.path, call.args.path) + buildQuery(call.args.query);

    let ambiguous = false;
    for (let attempt = 1; ; attempt++) {
      try {
        const answer = await exchange(this.#fetch, {
          method: route.method,
          url,
          headers,
          body,
          timeoutMs: this.#timeoutMs,
          signal,
        });
        const result = (answer.body ?? {}) as object;
        Object.defineProperty(result, "requestId", { value: answer.requestId, enumerable: false });
        return result as OperationResult<K>;
      } catch (err) {
        if (signal?.aborted) throw err;
        ambiguous ||= isAmbiguous(err);
        const retryable = repeatable || (err as { code?: string }).code === "rate_limited";
        const delay = retryable ? nextDelay(err, attempt, this.#policy, this.#random) : undefined;
        if (delay === undefined) {
          if (call.idempotencyKey && ambiguous && !isDefinitive(err)) {
            throw this.#unknown<K>(call, firstAttemptAt, err);
          }
          throw err;
        }
        await this.#sleep(delay, signal);
      }
    }
  }

  #unknown<K extends OperationId>(call: Call, firstAttemptAt: number, cause: unknown): UnknownOutcomeError {
    const key = call.idempotencyKey!;
    return new UnknownOutcomeError<OperationResult<K>>(
      key,
      firstAttemptAt,
      () => {
        if (this.#now() - firstAttemptAt > resolveWindowMs) {
          return Promise.reject(new IdempotencyWindowError(key));
        }
        return this.#run<K>(call, firstAttemptAt);
      },
      { cause },
    );
  }
}

// A 4xx after an ambiguous attempt still settles the outcome: with the same key, an applied
// attempt would have been replayed rather than refused. `rate_limited` settles nothing.
function isDefinitive(err: unknown): boolean {
  return err instanceof QuotacoreError && err.status >= 400 && err.status < 500 && err.code !== "rate_limited";
}

function checkAmount(request: MeteredRequest): void {
  if (!Number.isSafeInteger(request?.amount)) {
    throw new TypeError("amount must be an integer no larger than 2^53 - 1 (DR-022)");
  }
}

function buildPath(template: string, params: unknown): string {
  return template.replace(/\{([a-z_]+)\}/g, (_, name: string) => {
    const v = (params as Record<string, unknown> | undefined)?.[name];
    if (v === undefined || v === null || v === "") throw new TypeError(`path parameter ${name} is required`);
    return encodeURIComponent(String(v));
  });
}

function buildQuery(query: unknown): string {
  if (!query) return "";
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(query as Record<string, unknown>)) {
    if (v !== undefined && v !== null) q.append(k, String(v));
  }
  const s = q.toString();
  return s ? `?${s}` : "";
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const t = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(t);
      reject(signal!.reason);
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}
