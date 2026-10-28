# Runbook: Stale rollover or clock skew

Part of [the runbooks](README.md). Every command assumes `QUOTACORE_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QUOTACORE_ADMIN_KEY` an admin key, as [the CLI](../architecture/cli.md#2-environment) reads them.
The local operations (`verify-tenant`, `verify-all`, `reconcile-report`, `rebuild`) run the binary
against the same environment as the service. On the reference Compose stack (amended 2026-10-27, `IP-15`), a
command runs in the service's container as `docker compose exec quotacore quotacore …`, which has
that environment, and the metrics listener is reachable at `QC_METRICS` only with the override:
`docker compose -f docker-compose.yml -f docker-compose.metrics.yml up -d --wait`.

## Trigger

The *Stale rollover refusal* alert (P1) or the *Clock skew* alert (P1). Section 6 calls the first, with an identified cause, *An incorrect cycle was shipped*.

## Impact

Boundaries computed from a wrong clock are wrong. The monotonic guard refuses to roll a cycle backwards, so a tenant is answered against its live window, never re-granted twice.

## Check

1. The skew of every instance against the data store:

```sh
curl -sS "$QC_METRICS/metrics" | grep -E '^(quotacore_clock_skew_seconds|quotacore_rollover_stale_total)'
```

2. The host's time synchronisation (`timedatectl` or `chronyc tracking`).

## Decide

- One instance's `clock_skew_seconds` above one second: its clock is wrong.
- Every instance agrees and `rollover_stale_total` still rises: a stored index was written ahead, a data defect.
- The counter lags the clock by a tenant's idle time, because a transition happens on its first request after a boundary.

## Act

Fix NTP on the instance whose clock is off, then follow [A cycle rolled over incorrectly](cycle-rolled-over-incorrectly.md) for each tenant left stuck ahead.

## Verify

`quotacore_clock_skew_seconds` is within a second on every instance and `rollover_stale_total` stops rising.

## Escalate

A stored index ahead with every clock correct: escalate to the service owner as a data defect.

## Source

[Observability.md section 5](../architecture/observability.md#5-alerts); Q-26; A-17; [section 4.4](../architecture/consistency-and-recovery.md#44-a-cycle-rolled-over-incorrectly).
