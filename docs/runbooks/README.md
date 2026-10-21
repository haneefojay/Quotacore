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

The commands are the admin routes and the four local operations on `dev`; `IP-14`'s CLI replaces
each `curl` with its command.

## Review

IP-13 DoD 7 is ticked when a person has followed each runbook against a running instance. The
review is recorded here, one row per runbook.

| Runbook | Followed by | Against | Date | Result |
| --- | --- | --- | --- | --- |
| Every runbook | Haneef | — | — | Awaiting review |
