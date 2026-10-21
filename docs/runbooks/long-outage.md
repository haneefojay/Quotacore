# Runbook: Long outage

Part of [the runbooks](README.md). Every command assumes `QC_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QC_ADMIN_KEY` an admin key. The `quotacore` commands run the binary against the same environment
as the service.

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
curl -sS -X GET -H "Authorization: Bearer $QC_ADMIN_KEY" "$QC_URL/v1/admin/tenants/$TENANT_ID"
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
