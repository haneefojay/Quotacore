// The admin client (IP-26 D4, D10): every `/v1/admin/` operation in the contract, typed from the
// generated schema and built from the generated route table, so a contract change reaches this
// surface by regeneration alone.

import type { operations } from "./generated/schema.js";
import { routes, type AdminOperationId, type OperationId } from "./generated/routes.js";

type Op<K extends OperationId> = operations[K];
type Params<K extends OperationId> = Op<K>["parameters"];

type PathArg<K extends OperationId> = Params<K>["path"] extends infer P
  ? [NonNullable<P>] extends [never]
    ? {}
    : { path: NonNullable<P> }
  : {};

type QueryArg<K extends OperationId> = Params<K>["query"] extends infer Q
  ? [NonNullable<Q>] extends [never]
    ? {}
    : undefined extends Q
      ? { query?: NonNullable<Q> }
      : { query: NonNullable<Q> }
  : {};

type BodyArg<K extends OperationId> = Op<K> extends { requestBody: { content: { "application/json": infer B } } }
  ? { body: B }
  : Op<K> extends { requestBody?: { content: { "application/json": infer B } } }
    ? [B] extends [never]
      ? {}
      : { body?: B }
    : {};

/** The arguments of an operation: its path parameters, query and body, in the contract's names. */
export type OperationArgs<K extends OperationId> = PathArg<K> & QueryArg<K> & BodyArg<K> & {
  /** Aborts the call, including any wait between retries. */
  signal?: AbortSignal;
};

type Responses<K extends OperationId> = Op<K>["responses"];
type Success<K extends OperationId> = {
  [S in keyof Responses<K>]: S extends 200 | 201 ? Responses<K>[S] : never;
}[keyof Responses<K>];

/** The `X-Request-Id` of the response, attached to every result (NFR-S10). */
export interface ResponseMeta {
  readonly requestId: string;
}

/** The success body of an operation, or an empty object for a `204`. */
export type OperationResult<K extends OperationId> = ([Success<K>] extends [never]
  ? {}
  : Success<K> extends { content: { "application/json": infer R } }
    ? R
    : {}) &
  ResponseMeta;

type ArgsParam<K extends OperationId> = {} extends Omit<OperationArgs<K>, "signal">
  ? [args?: OperationArgs<K>]
  : [args: OperationArgs<K>];

/** The admin operations whose result is a page: they carry `next_cursor`. */
export type ListOperationId = {
  [K in AdminOperationId]: "next_cursor" extends keyof OperationResult<K> ? K : never;
}[AdminOperationId];

/** The element type of a list operation's page. */
export type ListItem<K extends ListOperationId> = {
  [P in keyof OperationResult<K>]-?: OperationResult<K>[P] extends readonly (infer I)[] ? I : never;
}[keyof OperationResult<K>];

export type AdminClient = {
  readonly [K in AdminOperationId]: (...args: ArgsParam<K>) => Promise<OperationResult<K>>;
} & {
  /**
   * Every item of a list, following `next_cursor` until it is null (api-conventions.md section 9).
   * Pages are fetched one at a time, as the iteration reaches them.
   */
  paginate<K extends ListOperationId>(operation: K, ...args: ArgsParam<K>): AsyncIterableIterator<ListItem<K>>;
};

/** What client.ts provides: one operation, retried as its method allows. */
export type Invoke = <K extends OperationId>(operation: K, args: Record<string, unknown>) => Promise<OperationResult<K>>;

export function createAdmin(invoke: Invoke): AdminClient {
  const admin: Record<string, unknown> = {};
  for (const id of Object.keys(routes) as OperationId[]) {
    if (!routes[id].admin) continue;
    admin[id] = (args?: Record<string, unknown>) => invoke(id, args ?? {});
  }
  admin.paginate = async function* (operation: ListOperationId, args?: Record<string, unknown>) {
    const query = { ...((args?.query as Record<string, unknown>) ?? {}) };
    for (;;) {
      const page = (await invoke(operation, { ...args, query })) as unknown as Record<string, unknown>;
      const items = Object.entries(page).find(([k, v]) => k !== "next_cursor" && Array.isArray(v));
      for (const item of (items?.[1] as unknown[]) ?? []) yield item;
      if (typeof page.next_cursor !== "string" || page.next_cursor === "") return;
      query.cursor = page.next_cursor;
    }
  };
  return admin as AdminClient;
}
