# Runbook: Ceiling violated

Part of [the runbooks](README.md). Every command assumes `QC_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QC_ADMIN_KEY` an admin key. The `quotacore` commands run the binary against the same environment
as the service.

## Trigger

The *Ceiling violated* alert (P0).

## Impact

A balance was read above `limit + bonus`: DR-019's invariant is broken and a tenant can spend more than its allowance.

## Check

```sh
curl -sS "$QC_METRICS/metrics" | grep -E '^(quotacore_ceiling_violation_total)'
```

Then the tenant's balances:

```sh
curl -sS -X GET -H "Authorization: Bearer $QC_ADMIN_KEY" "$QC_URL/v1/admin/tenants/$TENANT_ID"
quotacore verify-tenant $TENANT_ID
```

## Decide

Halt investigation of anything else: this is a defect in a script or a write that went around one.

## Act

Set each affected balance to the correct value with a reason ([security-model.md section 9](../architecture/security-model.md#9-incident-response), *A tenant's balance was inflated*), using `POST /v1/admin/tenants/{id}/set`.

## Verify

`quotacore_ceiling_violation_total` stops rising and `verify-tenant` reports no finding.

## Escalate

Always: escalate to the service owner with the tenant, the feature and the window.

## Source

DR-019; [section 4.4 of the recovery procedures](../architecture/consistency-and-recovery.md#44-a-cycle-rolled-over-incorrectly).
