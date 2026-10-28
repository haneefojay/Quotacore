# Runbook: Postgres restore

Part of [the runbooks](README.md). Every command assumes `QUOTACORE_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QUOTACORE_ADMIN_KEY` an admin key, as [the CLI](../architecture/cli.md#2-environment) reads them.
The local operations (`verify-tenant`, `verify-all`, `reconcile-report`, `rebuild`) run the binary
against the same environment as the service. On the reference Compose stack (amended 2026-10-27, `IP-15`), a
command runs in the service's container as `docker compose exec quotacore quotacore …`, which has
that environment, and the metrics listener is reachable at `QC_METRICS` only with the override:
`docker compose -f docker-compose.yml -f docker-compose.metrics.yml up -d --wait`.

## Trigger

Any restore of Postgres from backup. No alert fires for it; *Control plane down* (P2) may have preceded it.

## Impact

Audit and ledger history are truncated to the backup point. Balances are unaffected, because they live in the data store.

## Check

1. The backup's timestamp, from the backup tool, and that it precedes the loss.
2. That the restored database answers and its migrations are current: the service refuses to start otherwise.

```sh
quotacore status
```

## Decide

- Restoring over a live database is never the choice: restore to a new one.
- Differences found after the truncation point are expected; differences before it are an incident.

## Act

Follow [section 4.2](../architecture/consistency-and-recovery.md#42-postgres-lost--restore-from-backup): stop the service, restore to a new database, start the service, make every instance re-read the restored configuration, reconcile, and record the truncation:

```sh
quotacore config bump
quotacore verify-all
quotacore reconcile-report --since 24h
quotacore audit note --reason "restored from the backup of <time>; history after it is lost"
```

## Verify

`quotacore_controlplane_up` is `1` on every instance, `verify-all` reports only differences after the truncation point, and the truncation is recorded in the audit log.

## Escalate

Differences before the truncation point: escalate to the service owner.

## Source

[Section 4.2](../architecture/consistency-and-recovery.md#42-postgres-lost--restore-from-backup); NFR-A4.
