# The `quotacore` CLI

The administration tool, and the reference for what each command does. It is the same binary as
the service ([NFR-OPS6](non-functional-requirements.md)): with no argument, or `serve`, it runs the
service; with a command it administers one. Added 2026-10-27 (`IP-14`); the phase is
[IP-14 in the v0.1 roadmap](../roadmaps/v0-1-enforcement-path.md#ip-14--the-quotacore-cli), and the
scope it serves is [mvp-scope.md](../product/mvp-scope.md) §2's CLI row.

A remote command is an ordinary admin request
([request-lifecycle.md](request-lifecycle.md) steps 1 to 3): it is authenticated, rate limited and
audited exactly as the same call from any other client, and the audit row's `actor` is the key's
name. It obeys [api-conventions.md](api-conventions.md) because it is a client of the contract, not
a second way in.

---

## 1. Commands

Every command maps to exactly one documented endpoint or one local operation, with one exception:
`init`, which makes the quickstart's calls in order. Every `x-required-scope: admin` operation in
[`api/openapi.yaml`](../../api/openapi.yaml) has exactly one row. The data-plane routes and the
unauthenticated operational routes have none: they are not the administrative surface.
`TestTheMappingTableIsTheBinary` parses this table and fails on a row the binary does not have, a
command the table does not have, or a method, path or operation that is not the contract's.

| Command | Maps to | Changes state |
| --- | --- | --- |
| `feature list` | `GET /v1/admin/features` (`listFeatures`) | no |
| `feature get <feature_key>` | `GET /v1/admin/features/{feature_key}` (`getFeature`) | no |
| `feature create` | `POST /v1/admin/features` (`createFeature`) | yes |
| `feature update <feature_key>` | `PATCH /v1/admin/features/{feature_key}` (`updateFeature`) | yes |
| `feature delete <feature_key>` | `DELETE /v1/admin/features/{feature_key}` (`deleteFeature`) | yes |
| `feature archive <feature_key>` | `POST /v1/admin/features/{feature_key}/archive` (`archiveFeature`) | yes |
| `plan list` | `GET /v1/admin/plans` (`listPlans`) | no |
| `plan get <plan>` | `GET /v1/admin/plans/{id}` (`getPlan`) | no |
| `plan create` | `POST /v1/admin/plans` (`createPlan`) | yes |
| `plan update <plan>` | `PATCH /v1/admin/plans/{id}` (`updatePlan`) | yes |
| `plan delete <plan>` | `DELETE /v1/admin/plans/{id}` (`deletePlan`) | yes |
| `plan archive <plan>` | `POST /v1/admin/plans/{id}/archive` (`archivePlan`) | yes |
| `plan apply <plan>` | `POST /v1/admin/plans/{id}/apply` (`applyPlan`) | yes |
| `plan impact <plan>` | `GET /v1/admin/plans/{id}/impact` (`planImpact`) | no |
| `tenant list` | `GET /v1/admin/tenants` (`listTenants`) | no |
| `tenant get <tenant>` | `GET /v1/admin/tenants/{id}` (`getTenant`) | no |
| `tenant create` | `POST /v1/admin/tenants` (`createTenant`) | yes |
| `tenant update <tenant>` | `PATCH /v1/admin/tenants/{id}` (`updateTenant`) | yes |
| `tenant delete <tenant>` | `DELETE /v1/admin/tenants/{id}` (`deleteTenant`) | yes |
| `tenant suspend <tenant>` | `POST /v1/admin/tenants/{id}/suspend` (`suspendTenant`) | yes |
| `tenant resume <tenant>` | `POST /v1/admin/tenants/{id}/resume` (`resumeTenant`) | yes |
| `tenant override set <tenant>` | `POST /v1/admin/tenants/{id}/overrides` (`upsertTenantOverride`) | yes |
| `tenant override clear <tenant> <feature_key>` | `DELETE /v1/admin/tenants/{id}/overrides/{feature_key}` (`clearTenantOverride`) | yes |
| `grant <tenant>` | `POST /v1/admin/tenants/{id}/grant` (`grantTenant`) | yes |
| `set <tenant>` | `POST /v1/admin/tenants/{id}/set` (`setTenantBalance`) | yes |
| `rollover <tenant>` | `POST /v1/admin/tenants/{id}/force-rollover` (`forceRollover`) | yes |
| `events <tenant>` | `GET /v1/admin/tenants/{id}/events` (`listUsageEvents`) | no |
| `key list` | `GET /v1/admin/keys` (`listKeys`) | no |
| `key create` | `POST /v1/admin/keys` (`createKey`) | yes |
| `key revoke <key_id>` | `DELETE /v1/admin/keys/{id}` (`revokeKey`) | yes |
| `key mint` | local: prints a new admin token in the format of [ADR-0018](../decisions/0018-api-key-format-and-verifier.md), for `QUOTACORE_BOOTSTRAP_ADMIN_KEY`; reaches no server and no database | no |
| `audit` | `GET /v1/admin/audit` (`listAudit`) | no |
| `status` | `GET /v1/admin/status` (`status`) | no |
| `init` | composite: `createFeature`, `createPlan`, `createTenant`, `createKey` (`runtime`), then `balance` with the new runtime key | yes |
| `serve` | local: the service; no argument does the same | no |
| `healthcheck` | local: `GET /readyz` on `127.0.0.1` at `QUOTACORE_LISTEN_ADDR`'s port, never `QUOTACORE_URL`; exits `0` when ready and `9` otherwise, and prints nothing on success. The reference image's `HEALTHCHECK`, which has no shell to ask with; added 2026-10-27 (`IP-15`) | no |
| `verify-tenant <tenant>` | local: verifies one tenant's ledger against itself and its balance hashes (`IP-11`) | no |
| `verify-all` | local: the same for every tenant (`IP-11`) | no |
| `reconcile-report` | local: prints `reconcile_findings`, newest first (`IP-11`) | no |
| `rebuild` | local: rebuilds lost balance hashes from the ledger (`IP-11`) | with `--apply` |
| `config bump` | local: records a new configuration version against every tenant and publishes the invalidation, so every instance re-reads the configuration now; step 4 after a Postgres restore (`IP-16`, Q-28) | yes |
| `audit note` | local: `--reason` is required; appends an audit row with `subject_type` `system` and action `system.note`, such as a restore's truncation of history; step 6 after a Postgres restore (`IP-16`, Q-28) | yes |

A tenant is addressed by its `external_id` or its id, and a plan by its key or its id, as the
contract's path parameters allow; a key is addressed by its id. `key create --tenant <external_id>` binds a
runtime key to one tenant, which it alone can address, and `key list --tenant` lists that tenant's
keys (DR-053); `feature create --allocation-cap` marks a metered feature that counts entities
(DR-054). Both were added 2026-10-31 by `IP-20`. `quotacore <command> --help` prints
its flags and its route, from the same table the binary dispatches on.

## 2. Environment

| Variable | Default | Notes |
| --- | --- | --- |
| `QUOTACORE_URL` | `http://127.0.0.1:` and `QUOTACORE_LISTEN_ADDR`'s port | The service's base URL. `--url` overrides it, since a URL is not a secret. A URL carrying credentials is refused. Plain `http` to an address that is not loopback warns, because the admin key would cross the network unencrypted |
| `QUOTACORE_ADMIN_KEY` | `QUOTACORE_BOOTSTRAP_ADMIN_KEY` | An admin key. There is no flag for it, so it never appears in a process list or a shell history (NFR-S1). Falling back to the bootstrap key is what lets `docker compose exec quotacore quotacore init` run with what the quickstart already set |

The local operations read the service's own configuration
([deployment.md](deployment.md#4-configuration) §4), because they reach Postgres and the data store
directly. `key mint` reads nothing.

## 3. Exit codes

None is an HTTP status, so a script cannot mistake one for the other. An error answer is mapped by
its catalogue code, not by its status, so a `403` that is not about the credential is not reported
as one. `TestEveryCatalogueCodeHasOneExitCode` holds this table to
[error-catalog.md](../product/error-catalog.md) §2 in both directions.

| Exit | Meaning | Codes |
| --- | --- | --- |
| 0 | Success | — |
| 1 | A local operation found something a person must look at, or the service refused to start | — |
| 2 | Usage: an unknown command, a bad or missing flag, a missing variable | — |
| 3 | Client error: the request is wrong, or the contract refuses it before it is sent | `validation_failed`, `invalid_amount`, `invalid_window`, `invalid_timezone`, `missing_idempotency_key`, `payload_too_large`, `feature_not_in_plan`, `feature_archived`, `tenant_suspended`, `not_implemented` |
| 4 | Credential | `unauthorized`, `api_key_invalid`, `insufficient_scope` |
| 5 | Not found | `tenant_not_found`, `feature_not_found`, `plan_not_found`, `api_key_not_found` |
| 6 | Conflict, including a confirmation the operator must give | `idempotency_key_reuse`, `plan_in_use`, `feature_in_use`, `feature_key_exists`, `tenant_exists`, `tenant_deleted`, `plan_key_exists`, `refund_exceeds_grant`, `grant_exceeds_ceiling`, `reset_not_supported`, `feature_not_metered`, `feature_not_anchored`, `feature_unit_immutable`, `resource_version_conflict`, `confirmation_required` |
| 7 | Quota exhausted. No v0.1 command can be answered with it, since no admin route returns it; it is reserved for the data-plane commands of v0.5 | `quota_exceeded` |
| 8 | Rate limited | `rate_limited` |
| 9 | Unavailable, including a service or dependency that did not answer at all | `service_unavailable`, `control_plane_unavailable` |
| 10 | Internal, including any answer the contract does not define | `internal_error` |

`GET /v1/admin/status` refuses a caller with a bare `401`, `403` or `429` and no envelope, by
contract, so `quotacore status` maps those three by status: `4`, `4` and `8`. Every other answer
without an envelope is `10`.

A local operation's error that is not a finding is `2`, `5` (no such tenant), `6` (the rebuild
fence is held), `9` (Postgres or the data store did not answer) or `10`.

## 4. Output

- A success prints the response body as indented JSON on stdout, and nothing else on stdout. A
  `204` prints nothing. An operator who wants a table pipes to `jq`.
- An error prints one line to stderr, `error: <code>: <message> (request_id <id>)`, then the error
  envelope. The request id is always printed, even when the service never answered, because the
  CLI minted it and it is the thing to quote (NFR-S10).
- `init` prints the summary [deployment.md](deployment.md#2-the-quickstart) §2 shows, or with
  `--json` the five answers as one object.
- List commands print one page, with `--limit` and `--cursor`; `--all` follows `next_cursor` and
  prints every page's items as one page.

## 5. What a command never does

- **Print a secret it was not asked to create.** `key create` and `init` are the only commands that
  can print a plaintext key, each prints it once on stdout, and each writes to stderr that it
  cannot be shown again ([INV-K3](../product/state-machines.md#6-api-key)). No command echoes the admin key,
  the `Authorization` header or a request body, on success or on failure.
- **Retry a write.** A grant has no `Idempotency-Key`, so a retried grant is a second grant. A read
  under `--all` waits out one `rate_limited` answer per page; nothing else is retried.
- **Infer a value the contract requires.** `--version` is typed, never read from a preceding `GET`,
  because reading it implicitly is last-write-wins with extra steps (FS-12). `plan apply` never
  re-issues itself with the token a `409 confirmation_required` returned: the operator reads the
  count and re-runs with `--confirmation-token` (DR-046).
- **Prompt, except `init` at a terminal.** Every command runs non-interactively; `init` asks for
  its values only when stdin is a terminal and `--yes` is absent.
- **Reach a datastore over a remote command.** A remote command talks to the service over HTTP and
  nothing else; only the local operations open Postgres or the data store.
