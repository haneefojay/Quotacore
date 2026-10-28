# Runbook: Long outage

Part of [the runbooks](README.md). Every command assumes `QUOTACORE_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QUOTACORE_ADMIN_KEY` an admin key, as [the CLI](../architecture/cli.md#2-environment) reads them.
The local operations (`verify-tenant`, `verify-all`, `reconcile-report`, `rebuild`) run the binary
against the same environment as the service. On the reference Compose stack (amended 2026-10-27, `IP-15`), a
command runs in the service's container as `docker compose exec quotacore quotacore …`, which has
that environment, and the metrics listener is reachable at `QC_METRICS` only with the override:
`docker compose -f docker-compose.yml -f docker-compose.metrics.yml up -d --wait`.

## Trigger

The *Re-anchors pending* alert (P2), or any outage longer than one cycle interval.

## Impact

None on balances: the first request after the outage advances each cycle once (DR-009). A re-anchor committed during the outage waits in `pending_reanchors` until the data store takes it.

## Check

1. Pending work and the store:

```sh
curl -sS "$QC_METRICS/metrics" | grep -E '^(quotacore_reanchors_pending|quotacore_datastore_up)'
```

2. A tenant's state after the outage:

```sh
quotacore tenant get $TENANT_ID
```

## Decide

- `reanchors_pending` above zero with `datastore_up` at `1` for more than one refresh interval: the drain is failing; read its `job=drain` log lines.
- Otherwise wait: the drain runs on every refresh tick.

## Act

Restore the data store; the drain finishes every pending re-anchor on its next tick. Nothing is replayed by hand, because a hand replay could re-anchor twice.

## Verify

`quotacore_reanchors_pending` is `0`, and the tenant's events show exactly one `cycle_rolled_over` for each boundary crossed ([section 4.3](../architecture/consistency-and-recovery.md#43-the-service-was-down-for-a-long-period)).

## Escalate

A re-anchor that stays pending with the store healthy: escalate to the service owner with the `job=drain` lines.

## Source

[Section 4.3](../architecture/consistency-and-recovery.md#43-the-service-was-down-for-a-long-period); DR-009.
