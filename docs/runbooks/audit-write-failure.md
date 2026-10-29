# Runbook: Audit write failure

Part of [the runbooks](README.md). Every command assumes `QUOTACORE_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QUOTACORE_ADMIN_KEY` an admin key, as [the CLI](../architecture/cli.md#2-environment) reads them.
The local operations (`verify-tenant`, `verify-all`, `reconcile-report`, `rebuild`) run the binary
against the same environment as the service. On the reference Compose stack (amended 2026-10-27, `IP-15`), a
command runs in the service's container as `docker compose exec quotacore quotacore …`, which has
that environment, and the metrics listener is reachable at `QC_METRICS` only with the override:
`docker compose -f docker-compose.yml -f docker-compose.metrics.yml up -d --wait`.

## Trigger

The *Audit write failure* alert (P1): an admin write answered `500 internal_error`.

## Impact

An audit row commits in its mutation's transaction, so the mutation did not commit either, except for a balance change, which logs `a balance change was applied and its audit row did not commit` (NFR-D4).

## Check

```sh
curl -sS "$QC_METRICS/metrics" | grep -E '^(quotacore_errors_total\{code="internal_error")'
```

Find the request's `error` log line by its `request_id`, and read the audit log:

```sh
quotacore audit --limit 20
```

## Decide

- The log names a balance change without its audit row: record it by hand with a `set` carrying the reason.
- Otherwise nothing changed; fix the cause (Postgres space, a constraint) and retry the write.

## Act

Fix the cause in Postgres, then retry the administrative write with its original intent.

## Verify

The retried write answers `2xx` and its audit row is listed by `GET /v1/admin/audit`.

> **Amended 2026-10-29 (`IP-16`, the v0.1 runbook review on the reference stack).** A grant, `set` or
> `force-rollover` whose audit insert fails has **already changed the balance**, and the request
> answers `500`. The `error` line says so: "a balance change was applied and its audit row did not
> commit". In that case **do not retry**, because a retried grant is a second grant. Record the change
> with a `set` to the balance the store now holds, with a reason that names the failed request's id.
> Retry only in the second Decide case, where nothing changed.

## Escalate

A balance change without an audit row: escalate to the service owner.

## Source

NFR-D4; [observability.md section 5](../architecture/observability.md#5-alerts).
