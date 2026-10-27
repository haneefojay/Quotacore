# Runbook: Postgres restore

Part of [the runbooks](README.md). Every command assumes `QUOTACORE_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QUOTACORE_ADMIN_KEY` an admin key, as [the CLI](../architecture/cli.md#2-environment) reads them.
The local operations (`verify-tenant`, `verify-all`, `reconcile-report`, `rebuild`) run the binary
against the same environment as the service.

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

Follow [section 4.2](../architecture/consistency-and-recovery.md#42-postgres-lost--restore-from-backup): stop the service, restore to a new database, start the service, then reconcile:

```sh
quotacore verify-all
quotacore reconcile-report --since 24h
```

## Verify

`quotacore_controlplane_up` is `1` on every instance, `verify-all` reports only differences after the truncation point, and the truncation is recorded in the audit log.

## Escalate

Differences before the truncation point: escalate to the service owner.

## Source

[Section 4.2](../architecture/consistency-and-recovery.md#42-postgres-lost--restore-from-backup); NFR-A4.
