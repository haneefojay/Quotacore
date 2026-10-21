# Runbook: Enforcement is failing closed

Part of [the runbooks](README.md). Every command assumes `QC_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QC_ADMIN_KEY` an admin key. The `quotacore` commands run the binary against the same environment
as the service.

## Trigger

The *Enforcement failing closed* alert (P0) or *Data store unreachable* (P1), or a customer reporting `503` on the data plane.

## Impact

Consumes and refunds are refused with `503 service_unavailable` rather than answered. No balance is changed and nothing is granted for free (NFR-A3, DR-045), so the customer is denied service but not overcharged.

## Check

1. Which dependency is down, by this instance's own probes:

```sh
curl -sS "$QC_METRICS/metrics" | grep -E '^(quotacore_datastore_up|quotacore_controlplane_up|quotacore_datastore_errors_total|quotacore_balance_key_missing_total)'
```

2. What the status route says, with each dependency's latency:

```sh
curl -sS -X GET -H "Authorization: Bearer $QC_ADMIN_KEY" "$QC_URL/v1/admin/status"
```

3. Whether the instance is serving: `curl -sS -o /dev/null -w "%{http_code}\n" "$QC_URL/readyz"` answers `503` while the data store is down.

## Decide

- `datastore_up` is `0`: the data store is down or unreachable. Go to **Act**.
- `datastore_up` is `1` and `balance_key_missing_total` is rising: balances are missing; follow [Data store lost](data-store-lost.md) instead.
- Both are healthy and the customer still sees `503`: read the `code` in the customer's envelope and the matching `quotacore_errors_total{code=...}` series; `control_plane_unavailable` is a tenant the snapshot had not seen during a Postgres outage, which [the dependency matrix](../architecture/consistency-and-recovery.md#3-dependency-failure-matrix) expects.

## Act

1. Restore the data store's availability: its host, its network path, its memory (`quotacore_datastore_memory_used_ratio`; the store runs `noeviction`, DR-048).
2. Do not restart the service to make the alert go away: it fails closed by design, and the probe returns it to ready within two seconds of the store answering.
3. Do not flush or restart the data store unless it is lost; a restart that loses data is [Data store lost](data-store-lost.md).

## Verify

`quotacore_datastore_up` is `1`, `/readyz` answers `200`, and the share of `rejected` outcomes in `quotacore_consume_total` has fallen back. Then confirm no partial deduction: a retry with the customer's same `Idempotency-Key` replays or applies once (DR-027).

## Escalate

If the data store does not come back within the outage the customer tolerates, or balances are missing when it does, escalate to the owner of the data store and follow [Data store lost](data-store-lost.md).

## Source

[observability.md section 5](../architecture/observability.md#5-alerts); [the dependency failure matrix](../architecture/consistency-and-recovery.md#3-dependency-failure-matrix); NFR-A3; DR-045; DR-048.
