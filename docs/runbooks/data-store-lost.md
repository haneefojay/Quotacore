# Runbook: Data store lost

Part of [the runbooks](README.md). Every command assumes `QUOTACORE_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QUOTACORE_ADMIN_KEY` an admin key, as [the CLI](../architecture/cli.md#2-environment) reads them.
The local operations (`verify-tenant`, `verify-all`, `reconcile-report`, `rebuild`) run the binary
against the same environment as the service. On the reference Compose stack (amended 2026-10-27, `IP-15`), a
command runs in the service's container as `docker compose exec quotacore quotacore …`, which has
that environment, and the metrics listener is reachable at `QC_METRICS` only with the override:
`docker compose -f docker-compose.yml -f docker-compose.metrics.yml up -d --wait`.

## Trigger

The *Balance hash missing* alert (P0): `quotacore_balance_key_missing_total` above zero, usually after a data-store restore or restart that lost data.

## Impact

Every tenant-feature whose balance hash is absent is refused with `503`; nothing is granted (DR-045). Idempotency records are gone, so a client retrying a pre-loss request can be charged twice until reconciliation reverses it (DR-049).

## Check

1. How many and which: 

```sh
curl -sS "$QC_METRICS/metrics" | grep -E '^(quotacore_balance_key_missing_total|quotacore_datastore_up)'
```

2. A dry run of the rebuild, which reads the ledger and writes nothing:

```sh
quotacore rebuild --report /tmp/rebuild-report.json
```

## Decide

- A handful of tenants after a known incident, and the report reconciles them: rebuild.
- The report lists tenants that do not reconcile (a `set`, a `grant` or a mid-cycle plan change in the cycle): decide each one with `--accept <tenant>:<feature>` or a `set` with a reason after the rebuild.
- Do not restart in normal mode to clear the alert.

## Act

Follow [section 4.1 of the recovery procedures](../architecture/consistency-and-recovery.md#41-data-store-lost-or-corrupted--total-loss): restart every instance with `QUOTACORE_STARTUP_MODE=readonly`, then

```sh
quotacore rebuild --apply --reason "data store lost" --report /tmp/rebuild-report.json
```

and restart in normal mode. The `--reason` is required whenever the rebuild writes; amended
2026-10-27 (`IP-14`), because the command as first written was one the binary refuses.

## Verify

`quotacore_balance_key_missing_total` stops rising, `/readyz` is `200` on every instance, and `quotacore verify-all` reports no finding for the rebuilt tenants. Watch *Double charge detected* for the next reconciliation pass: a reversal is the expected remedy (DR-049).

## Escalate

Any tenant the rebuild cannot reconcile and nobody can decide, or a second loss: escalate to the service owner before switching to normal mode.

## Source

[Section 4.1](../architecture/consistency-and-recovery.md#41-data-store-lost-or-corrupted--total-loss); DR-045; DR-049; ADR-0016.
