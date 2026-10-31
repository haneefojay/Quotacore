// The retry policy (IP-26 D7, DR-040).
//
// A failure is retried only when the catalogue says a retry can succeed: a transport failure, a
// 5xx other than 501, or `429 rate_limited`. Never `quota_exceeded`, which waits for a calendar
// date, and never any other 4xx, which waits for a change to the request.

import { QuotacoreError, TransportError, UnexpectedResponseError } from "./errors.js";

export interface RetryPolicy {
  /** Attempts in total, including the first. 1 to 10. */
  readonly maxAttempts: number;
  /** The first backoff ceiling, doubled per attempt. */
  readonly baseDelayMs: number;
  /** The largest backoff ceiling. */
  readonly maxDelayMs: number;
  /**
   * The longest wait the SDK accepts from a `Retry-After` or `retry_after_ms`. A longer one is
   * returned to the caller as the error, rather than holding the call open.
   */
  readonly maxRetryAfterMs: number;
}

export const defaultRetryPolicy: RetryPolicy = {
  maxAttempts: 3,
  baseDelayMs: 100,
  maxDelayMs: 2_000,
  maxRetryAfterMs: 10_000,
};

/** Whether a failure is one a later attempt can cure. */
export function isRetryable(err: unknown): boolean {
  if (err instanceof TransportError) return true;
  if (err instanceof QuotacoreError) {
    return err.code === "rate_limited" || (err.status >= 500 && err.status !== 501);
  }
  if (err instanceof UnexpectedResponseError) return err.status >= 500 && err.status !== 501;
  return false;
}

/**
 * Whether a failure leaves a mutation's outcome unknown. A transport failure may have been lost
 * after the service applied it, and `503 service_unavailable` says the same of a store timeout. A
 * `500` is not ambiguous: a mutation that answers `500` was not applied (error-catalog.md 2.2).
 */
export function isAmbiguous(err: unknown): boolean {
  if (err instanceof TransportError) return true;
  if (err instanceof UnexpectedResponseError) return err.status >= 500;
  if (err instanceof QuotacoreError) return err.status === 503 || err.status === 502 || err.status === 504;
  return false;
}

/**
 * The wait before the next attempt, or undefined when there is no next attempt. `attempt` is the
 * number of the attempt that just failed, from 1. The backoff is full jitter; the envelope's
 * `retry_after_ms`, which mirrors `Retry-After`, is a floor under it.
 */
export function nextDelay(
  err: unknown,
  attempt: number,
  policy: RetryPolicy,
  random: () => number,
): number | undefined {
  if (attempt >= policy.maxAttempts || !isRetryable(err)) return undefined;
  const ceiling = Math.min(policy.maxDelayMs, policy.baseDelayMs * 2 ** (attempt - 1));
  const jittered = Math.floor(random() * ceiling);
  const asked = err instanceof QuotacoreError ? err.retryAfterMs : undefined;
  if (asked === undefined) return jittered;
  if (asked > policy.maxRetryAfterMs) return undefined;
  return Math.max(asked, jittered);
}

export function validatePolicy(policy: RetryPolicy): RetryPolicy {
  if (!Number.isInteger(policy.maxAttempts) || policy.maxAttempts < 1 || policy.maxAttempts > 10) {
    throw new RangeError("maxAttempts must be an integer from 1 to 10");
  }
  for (const k of ["baseDelayMs", "maxDelayMs", "maxRetryAfterMs"] as const) {
    if (!(policy[k] >= 0)) throw new RangeError(`${k} must be zero or more`);
  }
  return policy;
}
