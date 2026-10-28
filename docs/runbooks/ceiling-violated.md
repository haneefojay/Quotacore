# Runbook: Ceiling violated

Part of [the runbooks](README.md). Every command assumes `QUOTACORE_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QUOTACORE_ADMIN_KEY` an admin key, as [the CLI](../architecture/cli.md#2-environment) reads them.
The local operations (`verify-tenant`, `verify-all`, `reconcile-report`, `rebuild`) run the binary
against the same environment as the service. On the reference Compose stack (amended 2026-10-27, `IP-15`), a
command runs in the service's container as `docker compose exec quotacore quotacore …`, which has
that environment, and the metrics listener is reachable at `QC_METRICS` only with the override:
`docker compose -f docker-compose.yml -f docker-compose.metrics.yml up -d --wait`.

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
quotacore tenant get $TENANT_ID
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
