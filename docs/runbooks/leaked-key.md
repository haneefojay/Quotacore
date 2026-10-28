# Runbook: A leaked key

Part of [the runbooks](README.md). Every command assumes `QUOTACORE_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QUOTACORE_ADMIN_KEY` an admin key, as [the CLI](../architecture/cli.md#2-environment) reads them.
The local operations (`verify-tenant`, `verify-all`, `reconcile-report`, `rebuild`) run the binary
against the same environment as the service. On the reference Compose stack (amended 2026-10-27, `IP-15`), a
command runs in the service's container as `docker compose exec quotacore quotacore …`, which has
that environment, and the metrics listener is reachable at `QC_METRICS` only with the override:
`docker compose -f docker-compose.yml -f docker-compose.metrics.yml up -d --wait`.

## Trigger

Any suspected exposure of an API key.

## Impact

A runtime key can spend any tenant's allowance; an admin key can change any configuration.

## Check

1. Which key, by its prefix (the part before the dot), and its last use:

```sh
quotacore key list --all
```

2. For an admin key, every action it took:

```sh
quotacore audit --limit 100
```

## Decide

Any suspicion is enough: revoke first, investigate second.

## Act

Revoke it; the revocation is immediate:

```sh
quotacore key revoke $KEY_ID
```

Issue a replacement with `POST /v1/admin/keys`, and redeploy the client that used it.

## Verify

A request with the revoked key answers `401 api_key_invalid`, and `quotacore_rate_limited_total` shows no further traffic under its prefix.

## Escalate

Signs of use you cannot explain in the audit log or the events: escalate as a security incident per [security-model.md section 9](../architecture/security-model.md#9-incident-response).

## Source

[Security-model.md section 9](../architecture/security-model.md#9-incident-response); DR-043.
