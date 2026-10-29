# Runbook: Double charge

Part of [the runbooks](README.md). Every command assumes `QUOTACORE_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QUOTACORE_ADMIN_KEY` an admin key, as [the CLI](../architecture/cli.md#2-environment) reads them.
The local operations (`verify-tenant`, `verify-all`, `reconcile-report`, `rebuild`) run the binary
against the same environment as the service. On the reference Compose stack (amended 2026-10-27, `IP-15`), a
command runs in the service's container as `docker compose exec quotacore quotacore …`, which has
that environment, and the metrics listener is reachable at `QC_METRICS` only with the override:
`docker compose -f docker-compose.yml -f docker-compose.metrics.yml up -d --wait`.

## Trigger

*Double charge detected* (P0), *Ledger dead letter, duplicate* (P0) or *Duplicate reversal issued* (P1).

## Impact

A second deduction under one `Idempotency-Key` was applied. After a store loss it is expected, and reconciliation reverses it exactly once (DR-049); without one, the idempotency guarantee (DR-030) is broken.

## Check

1. Whether the data store was lost: `quotacore_balance_key_missing_total` and the store's own restart history.

```sh
curl -sS "$QC_METRICS/metrics" | grep -E '^(quotacore_event_duplicate_detected_total|quotacore_duplicate_reversal_total|quotacore_event_dead_letters)'
```

2. What the pass found and reversed:

```sh
quotacore reconcile-report --since 24h
```

## Decide

- A store loss preceded it: the reversal is the remedy; confirm it happened once.
- No store loss: the script is at fault; rotate runtime keys and find the window of exposure.

## Act

With a store loss, let the next reconciliation pass reverse it, or run a tenant's now:

```sh
quotacore verify-tenant $TENANT_ID
```

Without one, halt and follow [A leaked key](leaked-key.md) for the runtime keys.

## Verify

Each duplicate has exactly one reversal with a matching `event_id`, and a second pass issues nothing further.

> **Amended 2026-10-29 (`IP-16`, the v0.1 runbook review on the reference stack).** `reconcile-report` lists
> reconciliation findings, not duplicates. A detected duplicate and its reversal show in the
> metrics above and in `quotacore events $TENANT_ID`, as a `refunded` row carrying the duplicate's
> `Idempotency-Key`. Verify with those. The background pass issues the reversal, and
> `verify-tenant` only checks it.

## Escalate

A duplicate with no store loss: escalate at once to the service owner.

## Source

DR-030; DR-049; [section 5](../architecture/consistency-and-recovery.md#5-reconciliation); [section 4.1](../architecture/consistency-and-recovery.md#41-data-store-lost-or-corrupted--total-loss).
