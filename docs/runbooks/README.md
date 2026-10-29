# Runbooks

What an operator does when an alert fires or one of [observability.md section
6](../architecture/observability.md#6-operator-runbooks)'s events happens. Each runbook links to the procedure in
[consistency-and-recovery.md](../architecture/consistency-and-recovery.md) or [security-model.md](../architecture/security-model.md) rather than copying it, and holds
only the checks, the decision and the action, under the same eight headings. The alert rules are
[`ops/alerts/quotacore.rules.yml`](../../ops/alerts/quotacore.rules.yml); a P0 or P1 rule's
`runbook` annotation points here. Added 2026-10-21 (`IP-13`).

| Runbook | Trigger |
| --- | --- |
| [Enforcement is failing closed](enforcement-failing-closed.md) | *Enforcement failing closed* (P0), *Data store unreachable* (P1), a customer reporting `503` |
| [Data store lost](data-store-lost.md) | *Balance hash missing* (P0) |
| [Postgres restore](postgres-restore.md) | Any restore from backup |
| [Long outage](long-outage.md) | *Re-anchors pending* (P2), any outage over one cycle interval |
| [A cycle rolled over incorrectly](cycle-rolled-over-incorrectly.md) | A customer reports the wrong reset time |
| [A leaked key](leaked-key.md) | Any suspected credential exposure |
| [Stale rollover or clock skew](stale-rollover-or-clock-skew.md) | *Stale rollover refusal* (P1), *Clock skew* (P1) |
| [Double charge](double-charge.md) | *Double charge detected* (P0), *Ledger dead letter, duplicate* (P0), *Duplicate reversal issued* (P1) |
| [Reconciliation mismatch](reconciliation-mismatch.md) | *Reconciliation mismatch* (P1) |
| [Ceiling violated](ceiling-violated.md) | *Ceiling violated* (P0) |
| [Audit write failure](audit-write-failure.md) | *Audit write failure* (P1) |

The commands are the `quotacore` CLI's ([cli.md](../architecture/cli.md)). Amended 2026-10-27
(`IP-14`): every `curl` to an admin route is now its command, and three kinds of `curl` stay because
no command wraps them: the metrics scrape, `/readyz`, and `/v1/balance` with a runtime key.
`TestEveryRunbookIsFollowable` parses every `quotacore` line against the binary's command table and
fails on a `curl` to `/v1/admin/`.

## Review

IP-13 DoD 7 is ticked when a person has followed each runbook against a running instance. The
review is recorded here, one row per runbook.

| Runbook | Followed by | Against | Date | Result |
| --- | --- | --- | --- | --- |
| Every runbook | Claude, on Haneef's instruction | The reference Compose stack (`docker compose up`, the README quickstart) at `dev` 5b81d38, each fault injected for real | 2026-10-29 | Approved by Haneef on 2026-10-29, from [the review report](https://claude.ai/artifact/8vCkvqTMy8TVsESdTXVkq5). All 11 runbooks were followed and their commands work. Eight needed a note, added as dated amendments. One defect was found and fixed: a tenant first seen during a store outage took 3 s and was blamed on Postgres |
