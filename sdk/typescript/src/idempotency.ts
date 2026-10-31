// Idempotency keys (IP-26 D5, DR-026).
//
// One key per logical operation, fixed before the first attempt and carried by every retry and by
// UnknownOutcomeError.resolve(). The caller may supply it, which is better when they have a durable
// identifier for the operation (J-7): a key derived from an order id survives a process restart,
// and a generated one does not.

import { randomUUID } from "node:crypto";

/** The contract's `IdempotencyKey` pattern. */
export const idempotencyKeyPattern = /^[A-Za-z0-9._:-]{8,255}$/;

/** A fresh key for one logical operation. A UUID matches the contract's pattern. */
export function newIdempotencyKey(): string {
  return randomUUID();
}

/** Throws a TypeError for a key the service would refuse, before anything is sent. */
export function checkIdempotencyKey(key: string): string {
  if (!idempotencyKeyPattern.test(key)) {
    throw new TypeError(`idempotencyKey must match ${idempotencyKeyPattern.source}`);
  }
  return key;
}
