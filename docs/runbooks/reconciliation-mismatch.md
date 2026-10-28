# Runbook: Reconciliation mismatch

Part of [the runbooks](README.md). Every command assumes `QUOTACORE_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QUOTACORE_ADMIN_KEY` an admin key, as [the CLI](../architecture/cli.md#2-environment) reads them.
The local operations (`verify-tenant`, `verify-all`, `reconcile-report`, `rebuild`) run the binary
against the same environment as the service. On the reference Compose stack (amended 2026-10-27, `IP-15`), a
command runs in the service's container as `docker compose exec quotacore quotacore …`, which has
that environment, and the metrics listener is reachable at `QC_METRICS` only with the override:
`docker compose -f docker-compose.yml -f docker-compose.metrics.yml up -d --wait`.

## Trigger

The *Reconciliation mismatch* alert (P1).

## Impact

A balance disagrees with what its ledger explains. Nothing is corrected automatically.

## Check

```sh
quotacore reconcile-report --since 24h
quotacore verify-tenant $TENANT_ID
```

## Decide

- The difference is a known ledger gap (`event_sink_dropped_total`): the runtime balance is authoritative; see [section 4.5](../architecture/consistency-and-recovery.md#45-ledger-gap-detected).
- Otherwise a person decides the correct balance.

## Act

Set the decided balance with a reason; the change is audited and emits a `set` event:

```sh
quotacore set $TENANT_ID --feature FEATURE --value 0 --reason reconciliation
```

## Verify

`quotacore verify-tenant $TENANT_ID` reports no finding.

## Escalate

A mismatch nobody can explain: escalate to the service owner.

## Source

[Section 5](../architecture/consistency-and-recovery.md#5-reconciliation); DR-042.
