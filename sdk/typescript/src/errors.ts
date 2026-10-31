// The SDK's error types (IP-26 D8).
//
// Three families that share no base class, on purpose:
//   - QuotacoreError: the service answered with the error envelope. `code` is the only field to
//     branch on, and each catalogue code has its own subclass in generated/errors.ts.
//   - TransportError and UnexpectedResponseError: the service's answer never arrived, or was not
//     the envelope. Nothing is known about the code.
//   - UnknownOutcomeError: a consume or refund may have been applied, and `resolve()` finds out.
// A QuotaExceededError is therefore never caught by a handler written for a network failure,
// which is how a customer ends up disabling enforcement (ROADMAP.md, v0.5).

import { catalogue, type ErrorCode } from "./generated/catalogue.js";

/** The `error` object of the contract's envelope. */
export interface ErrorEnvelope {
  code: string;
  message: string;
  request_id: string;
  retry_after_ms?: number;
  details?: Record<string, unknown>;
}

/** An error the service returned in its envelope. Branch on `code`, never on `message`. */
export class QuotacoreError extends Error {
  /** The catalogue code. A code newer than this package is still carried here verbatim. */
  readonly code: ErrorCode | (string & {});
  /** The HTTP status. A class hint only; the code is the meaning. */
  readonly status: number;
  /** Equal to the `X-Request-Id` response header. Quote it in a bug report. */
  readonly requestId: string;
  /** Code-specific, and additive across versions. */
  readonly details: Record<string, unknown>;
  /** Present only when the service says retrying is correct. */
  readonly retryAfterMs: number | undefined;

  constructor(status: number, envelope: ErrorEnvelope, requestId?: string) {
    super(envelope.message);
    this.name = new.target.name;
    this.code = envelope.code;
    this.status = status;
    this.requestId = envelope.request_id || requestId || "";
    this.details = envelope.details ?? {};
    this.retryAfterMs = envelope.retry_after_ms;
  }

  /** The catalogue's client action for this code, or undefined for a code newer than this package. */
  get clientAction(): string | undefined {
    return (catalogue as Record<string, { clientAction: string }>)[this.code]?.clientAction;
  }
}

/** True when `e` is a QuotacoreError, optionally with the given code. */
export function isQuotacoreError(e: unknown, code?: ErrorCode): e is QuotacoreError {
  return e instanceof QuotacoreError && (code === undefined || e.code === code);
}

/** The request never produced a response: a connection failure, a reset, or a timeout. */
export class TransportError extends Error {
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = "TransportError";
  }
}

/** The service answered, but not with the contract's envelope, for example a proxy's 502 page. */
export class UnexpectedResponseError extends Error {
  readonly status: number;
  /** The `X-Request-Id` header, when whatever answered sent one. */
  readonly requestId: string | undefined;
  readonly body: string;

  constructor(status: number, body: string, requestId: string | undefined) {
    super(`unexpected ${status} response without the error envelope`);
    this.name = "UnexpectedResponseError";
    this.status = status;
    this.requestId = requestId;
    this.body = body;
  }
}

/**
 * A `consume` or `refund` whose outcome is not known: an attempt may have reached the service and
 * been applied, and no later attempt returned a definitive answer.
 *
 * `resolve()` re-issues the identical request with the identical `Idempotency-Key`. If the
 * original was applied, the service replays it (`replayed: true`) and charges nothing more; if it
 * was not, this applies it once (DR-027). A new key would charge twice (FS-03), which is why the
 * key is fixed and is never offered for change.
 */
export class UnknownOutcomeError<T = unknown> extends Error {
  /** The key every attempt carried, and the one `resolve()` will carry. */
  readonly idempotencyKey: string;
  /** When the first attempt was sent, in milliseconds since the epoch. */
  readonly firstAttemptAt: number;
  readonly #resolve: () => Promise<T>;

  constructor(
    idempotencyKey: string,
    firstAttemptAt: number,
    resolve: () => Promise<T>,
    options: { cause: unknown },
  ) {
    super(
      `the outcome of the request with Idempotency-Key ${idempotencyKey} is unknown; call resolve() to settle it`,
      options,
    );
    this.name = "UnknownOutcomeError";
    this.idempotencyKey = idempotencyKey;
    this.firstAttemptAt = firstAttemptAt;
    this.#resolve = resolve;
  }

  /**
   * Settles the outcome with the same key. Throws IdempotencyWindowError once the service's 24-hour
   * window may have closed, because a same-key request after that is a new operation (DR-029).
   */
  resolve(): Promise<T> {
    return this.#resolve();
  }
}

/**
 * `resolve()` was called too late to be safe. After the 24-hour idempotency window the service
 * treats the key as new and would charge again, so the SDK refuses with a one-hour margin. Use the
 * admin API's usage events, filtered by `idempotency_key`, to see what was recorded (UC-16).
 */
export class IdempotencyWindowError extends Error {
  readonly idempotencyKey: string;

  constructor(idempotencyKey: string) {
    super(
      `refusing to re-issue Idempotency-Key ${idempotencyKey}: more than 23 hours have passed since the first attempt`,
    );
    this.name = "IdempotencyWindowError";
    this.idempotencyKey = idempotencyKey;
  }
}
