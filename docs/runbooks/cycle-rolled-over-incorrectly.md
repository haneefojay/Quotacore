# Runbook: A cycle rolled over incorrectly

Part of [the runbooks](README.md). Every command assumes `QUOTACORE_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QUOTACORE_ADMIN_KEY` an admin key, as [the CLI](../architecture/cli.md#2-environment) reads them.
The local operations (`verify-tenant`, `verify-all`, `reconcile-report`, `rebuild`) run the binary
against the same environment as the service. On the reference Compose stack (amended 2026-10-27, `IP-15`), a
command runs in the service's container as `docker compose exec quotacore quotacore …`, which has
that environment, and the metrics listener is reachable at `QC_METRICS` only with the override:
`docker compose -f docker-compose.yml -f docker-compose.metrics.yml up -d --wait`.

## Trigger

A customer reports a reset at the wrong time, or a tenant stuck in a window that has ended.

## Impact

One tenant received an allowance in the wrong window, or none.

## Check

```sh
curl -sS -X GET -H "Authorization: Bearer $RUNTIME_KEY" "$QUOTACORE_URL/v1/balance?tenant_external_id=$EXTERNAL_ID"
```

Note `cycle_index`, `cycle_start` and `cycle_end`, and compare them with the boundary table in [cycle-engine.md](../architecture/cycle-engine.md#6-test-matrix) for the interval and zone.

## Decide

- Stored index ahead of the computed one: the tenant is stuck; act.
- Stored index behind: the next request advances it; no action.
- The computed window itself is wrong: a cycle-engine defect; escalate.

## Act

```sh
quotacore rollover $TENANT_ID --feature FEATURE --reason "stuck cycle"
```

## Verify

The balance route shows the current window, and the audit log holds the force-rollover with its reason.

## Escalate

A wrong computed window: escalate to the cycle engine's owner and add the case to the boundary table.

## Source

[Section 4.4](../architecture/consistency-and-recovery.md#44-a-cycle-rolled-over-incorrectly); DR-009.
