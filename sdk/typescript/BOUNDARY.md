# The generated and hand-written boundary

`IP-26` DoD 1. A contract change regenerates this package without touching hand-written code.

| Path | Written by | Edit it? |
| --- | --- | --- |
| `src/generated/` | `npm run generate`, from `api/openapi.yaml` and `docs/product/error-catalog.md` | Never. Change the contract or the catalogue and regenerate |
| `src/*.ts` | Hand | Yes. Only the retry policy, typed errors, idempotency, transport and the admin client's typing live here |
| `test/` | Hand | Yes |

`src/generated/` holds four files:

| File | Contents |
| --- | --- |
| `schema.ts` | Every type in the contract, from `openapi-typescript` |
| `routes.ts` | Each `/v1/` operation's method and path, and the contract version |
| `catalogue.ts` | Each error code's status, retry verdict and client action |
| `errors.ts` | One `QuotacoreError` subclass per code |

Every generated file begins with a `DO NOT EDIT` banner, and no hand-written file may carry it
(`test/boundary.test.ts`). The generator fails without writing anything when the catalogue and
the contract's `ErrorCode` enum disagree.

## Regenerating

```sh
cd sdk/typescript
npm ci
npm run generate
git diff --exit-code src/generated   # CI runs this; a diff means the committed files are stale
```

The admin client is built from `routes.ts` and typed from `schema.ts`, so a new admin operation in
the contract appears on `client.admin` by regeneration alone. A new error code appears as a new
exported class the same way.
