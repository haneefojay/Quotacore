# Runbook: Reconciliation mismatch

Part of [the runbooks](README.md). Every command assumes `QC_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QC_ADMIN_KEY` an admin key. The `quotacore` commands run the binary against the same environment
as the service.

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
curl -sS -X POST -H "Authorization: Bearer $QC_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"feature_key":"FEATURE","value":0,"reason":"reconciliation"}' "$QC_URL/v1/admin/tenants/$TENANT_ID/set"
```

## Verify

`quotacore verify-tenant $TENANT_ID` reports no finding.

## Escalate

A mismatch nobody can explain: escalate to the service owner.

## Source

[Section 5](../architecture/consistency-and-recovery.md#5-reconciliation); DR-042.
