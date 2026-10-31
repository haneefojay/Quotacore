// One HTTP exchange: no retries here, those are client.ts's.

import { QuotacoreError, TransportError, UnexpectedResponseError, type ErrorEnvelope } from "./errors.js";
import { errorClasses } from "./generated/errors.js";

export interface Exchange {
  method: string;
  url: string;
  headers: Record<string, string>;
  body?: string;
  timeoutMs: number;
  signal?: AbortSignal;
}

export interface Answer {
  status: number;
  headers: Headers;
  /** The parsed JSON body, or undefined for a 204. */
  body: unknown;
  requestId: string;
}

/**
 * Sends one request. A 2xx resolves; an error envelope rejects with the code's QuotacoreError
 * subclass; anything else rejects with TransportError or UnexpectedResponseError.
 */
export async function exchange(fetchImpl: typeof fetch, x: Exchange): Promise<Answer> {
  const timeout = AbortSignal.timeout(x.timeoutMs);
  const signal = x.signal ? AbortSignal.any([x.signal, timeout]) : timeout;
  let res: Response;
  let text: string;
  try {
    res = await fetchImpl(x.url, { method: x.method, headers: x.headers, body: x.body, signal });
    text = await res.text();
  } catch (cause) {
    if (x.signal?.aborted) throw x.signal.reason;
    const why = timeout.aborted ? `timed out after ${x.timeoutMs} ms` : "failed";
    throw new TransportError(`${x.method} ${new URL(x.url).pathname} ${why}`, { cause });
  }
  const requestId = res.headers.get("x-request-id") ?? "";
  if (res.status >= 200 && res.status < 300) {
    if (res.status === 204 || text === "") return { status: res.status, headers: res.headers, body: undefined, requestId };
    try {
      return { status: res.status, headers: res.headers, body: JSON.parse(text), requestId };
    } catch {
      throw new UnexpectedResponseError(res.status, text, requestId || undefined);
    }
  }
  const envelope = parseEnvelope(text);
  if (!envelope) throw new UnexpectedResponseError(res.status, text, requestId || undefined);
  const Class = (errorClasses as Record<string, typeof QuotacoreError>)[envelope.code] ?? QuotacoreError;
  throw new Class(res.status, envelope, requestId);
}

function parseEnvelope(text: string): ErrorEnvelope | undefined {
  try {
    const parsed = JSON.parse(text) as { error?: ErrorEnvelope };
    const e = parsed?.error;
    if (e && typeof e.code === "string" && typeof e.message === "string") return e;
  } catch {
    // Not JSON, so not the envelope.
  }
  return undefined;
}

