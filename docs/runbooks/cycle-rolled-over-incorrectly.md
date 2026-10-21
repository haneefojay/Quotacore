# Runbook: A cycle rolled over incorrectly

Part of [the runbooks](README.md). Every command assumes `QC_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QC_ADMIN_KEY` an admin key. The `quotacore` commands run the binary against the same environment
as the service.

## Trigger

A customer reports a reset at the wrong time, or a tenant stuck in a window that has ended.

## Impact

One tenant received an allowance in the wrong window, or none.

## Check

```sh
curl -sS -X GET -H "Authorization: Bearer $RUNTIME_KEY" "$QC_URL/v1/balance?tenant_external_id=$EXTERNAL_ID"
```

Note `cycle_index`, `cycle_start` and `cycle_end`, and compare them with the boundary table in [cycle-engine.md](../architecture/cycle-engine.md#6-test-matrix) for the interval and zone.

## Decide

- Stored index ahead of the computed one: the tenant is stuck; act.
- Stored index behind: the next request advances it; no action.
- The computed window itself is wrong: a cycle-engine defect; escalate.

## Act

```sh
curl -sS -X POST -H "Authorization: Bearer $QC_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"feature_key":"FEATURE","reason":"stuck cycle"}' "$QC_URL/v1/admin/tenants/$TENANT_ID/force-rollover"
```

## Verify

The balance route shows the current window, and the audit log holds the force-rollover with its reason.

## Escalate

A wrong computed window: escalate to the cycle engine's owner and add the case to the boundary table.

## Source

[Section 4.4](../architecture/consistency-and-recovery.md#44-a-cycle-rolled-over-incorrectly); DR-009.
