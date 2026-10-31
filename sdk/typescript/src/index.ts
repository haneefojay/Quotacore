// The package's public surface.

export { Quotacore, resolveWindowMs } from "./client.js";
export type {
  QuotacoreOptions,
  MutationOptions,
  MeteredRequest,
  MeteredResponse,
  CheckResponse,
  BalanceResponse,
} from "./client.js";
export type {
  AdminClient,
  OperationArgs,
  OperationResult,
  ListOperationId,
  ListItem,
  ResponseMeta,
} from "./admin.js";
export {
  QuotacoreError,
  TransportError,
  UnexpectedResponseError,
  UnknownOutcomeError,
  IdempotencyWindowError,
  isQuotacoreError,
} from "./errors.js";
export type { ErrorEnvelope } from "./errors.js";
export * from "./generated/errors.js";
export { catalogue, errorCodes } from "./generated/catalogue.js";
export type { ErrorCode, CatalogueEntry } from "./generated/catalogue.js";
export { contractVersion, routes } from "./generated/routes.js";
export type { OperationId, AdminOperationId } from "./generated/routes.js";
export { defaultRetryPolicy, isRetryable } from "./retry.js";
export type { RetryPolicy } from "./retry.js";
export { newIdempotencyKey, idempotencyKeyPattern } from "./idempotency.js";
export type { components, operations, paths } from "./generated/schema.js";
