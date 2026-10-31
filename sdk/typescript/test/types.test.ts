// Compile-time checks of the admin client's types. The assertions are in the types: this file
// fails `tsc`, not the run, when the generated surface stops matching the contract.

import { test } from "node:test";
import type { AdminClient, ListItem, OperationArgs, components } from "../src/index.js";

type Equal<A, B> = (<T>() => T extends A ? 1 : 2) extends <T>() => T extends B ? 1 : 2 ? true : false;
const yes = <T extends true>() => undefined as unknown as T;

// A list with no required parameter takes no argument.
type ListArgs = Parameters<AdminClient["listFeatures"]>;
yes<Equal<ListArgs extends [args?: unknown] ? true : false, true>>();
// A route with a path parameter requires it.
yes<Equal<OperationArgs<"getTenant">["path"], { id: string }>>();
// A create requires its body, typed by the contract.
yes<Equal<OperationArgs<"createFeature">["body"], components["schemas"]["FeatureCreate"]>>();
// paginate() yields the list's element type.
yes<Equal<ListItem<"listFeatures">, components["schemas"]["Feature"]>>();
// @ts-expect-error a required path parameter cannot be left out
void ((a: AdminClient) => a.getTenant({}));

test("the admin client's types compile", () => {});
