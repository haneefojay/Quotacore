# Data Model

Two stores with a strict division of responsibility: PostgreSQL holds what should be true, and
the Redis-compatible store holds what is true right now. This document specifies both, the
invariants that span them, and the migration policy.

Rule identifiers in parentheses refer to [domain-rules](../product/domain-rules.md).

---

## 1. Division of responsibility

| Concern | PostgreSQL | Redis-compatible | Why there |
| --- | --- | --- | --- |
| Features, plans, entitlements | authoritative | projected into the snapshot cache | Configuration changes rarely and is read on every request |
| Tenants | authoritative | projected into the snapshot cache | Same |
| API keys | authoritative | hashed form cached, bounded | Auth must not touch Postgres on the hot path |
| Balances | ledger of deltas only | **authoritative** | Must be readable and writable atomically at request rate |
| Cycle index and window | anchor only | authoritative | Must be advanced atomically with the deduction (ADR-0003) |
| Idempotency records | unique constraint, detection only | **authoritative, prevention** | Only a script can prevent a double charge (DR-030) |
| Audit log | authoritative | never | An audit record that could be lost is not an audit log (NFR-D4) |
| Usage events | authoritative, append-only | never | History and reconstruction |

The single most important line in that table is that the balance is authoritative in the fast
store and the ledger is authoritative in the durable one. They are not mirrors, and a
reconciliation between them is a recovery procedure, not a steady-state process
([consistency-and-recovery.md](consistency-and-recovery.md)).

---

## 2. PostgreSQL schema

Target version 16 or later (ADR-0011, A-13). Identifiers are `snake_case`; timestamps are
`timestamptz` and always stored in UTC; money is absent by design.

### 2.1 Enumerations

Rather than free-text columns with a `CHECK`, lifecycle states are real PostgreSQL enums, so an
illegal value cannot be written even by a bug in application code.

```sql
CREATE TYPE tenant_state       AS ENUM ('active', 'suspended', 'deleted');
CREATE TYPE archive_state      AS ENUM ('active', 'archived');
CREATE TYPE feature_kind       AS ENUM ('metered', 'boolean', 'rolling');   -- 'boolean' and 'rolling' from v0.2 (DR-050, DR-052)
CREATE TYPE reset_interval     AS ENUM
  ('hourly', 'daily', 'weekly', 'monthly', 'yearly', 'never');
CREATE TYPE key_scope          AS ENUM ('runtime', 'admin');
CREATE TYPE key_state          AS ENUM ('active', 'revoked', 'expired');
CREATE TYPE event_source       AS ENUM ('api', 'admin', 'system');
CREATE TYPE event_type         AS ENUM (
  'consumed', 'refunded', 'granted', 'set',
  'plan_changed', 'cycle_rolled_over', 'force_rolled_over'
);
```

`feature_kind` exists in v0.1 with a single valid value in practice. It is added now because
adding an enum value later is a schema change and adding a column later is also a schema change,
and the v0.2 design is known.

### 2.2 Features

```sql
CREATE TABLE features (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  key           text NOT NULL UNIQUE,
  name          text NOT NULL,
  unit          text NOT NULL,
  kind          feature_kind NOT NULL DEFAULT 'metered',
  description   text NOT NULL DEFAULT '',
  state         archive_state NOT NULL DEFAULT 'active',
  version       bigint NOT NULL DEFAULT 1,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  archived_at   timestamptz,
  CONSTRAINT features_key_format
    CHECK (key ~ '^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$'),
  CONSTRAINT features_unit_format
    CHECK (unit ~ '^[a-z][a-z0-9_]{0,31}$')
);
```

`key` is immutable (INV-F3) and `unit` is immutable once an assignment exists (INV-F2). Both are
enforced in the application layer with distinct error codes, because a database `CHECK` cannot
express "immutable unless untouched" and an opaque trigger error would be worse for the API
contract.

`gen_random_uuid()` produces a **version 4** identifier, not a version 7 one. That is a real
distinction and it is not cosmetic: v4 is uniform, so it does not fragment a b-tree as badly as
many other schemes, but it is not time-ordered, so primary-key inserts scatter across the index and
`ORDER BY id` is not chronological.

The specification does not depend on UUIDv7, so the DDL above is correct as written and needs no
generated column. It is documented here rather than left implicit because the comment in the schema
would otherwise invite the wrong conclusion:

- If time-ordered identifiers are wanted later, PostgreSQL 18's `uuidv7()` is the supported way, and
  the change is one `DEFAULT` per table plus a data migration.
- Nothing in the API, the keyspace or the event ordering depends on it. Ordering is always by
  `created_at` or `occurred_at`, never by `id` — which is also why the ledger's `seq` column exists.

### 2.3 Plans and entitlements

```sql
CREATE TABLE plans (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  key           text NOT NULL UNIQUE,
  name          text NOT NULL,
  description   text NOT NULL DEFAULT '',
  state         archive_state NOT NULL DEFAULT 'active',
  version       bigint NOT NULL DEFAULT 1,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  archived_at   timestamptz,
  CONSTRAINT plans_key_format CHECK (key ~ '^[a-z0-9][a-z0-9_-]{0,62}$')
);

CREATE TABLE plan_entitlements (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  plan_id          uuid NOT NULL REFERENCES plans(id) ON DELETE RESTRICT,
  feature_id       uuid NOT NULL REFERENCES features(id) ON DELETE RESTRICT,
  limit_value      bigint,                             -- null for a boolean feature
  interval         reset_interval,                     -- null for a boolean or rolling feature
  rolling_window_seconds integer                       -- a rolling feature's window (DR-052)
    CHECK (rolling_window_seconds IN (300, 900, 3600, 21600, 86400, 604800, 2592000)),
  UNIQUE (plan_id, feature_id),                       -- INV-E2
  CONSTRAINT plan_entitlements_limit_nonneg CHECK (limit_value >= 0),
  -- Lua 5.1 numbers are doubles: above 2^53-1 integer arithmetic stops being
  -- exact, so a balance in that range cannot be represented faithfully in the
  -- script that enforces it. Bounded here so the database, the API and the
  -- script all reject the same values. See security-model.md section 6.
  CONSTRAINT plan_entitlements_limit_range
    CHECK (limit_value <= 9007199254740991),
  CONSTRAINT plan_entitlements_shape
    CHECK ((limit_value IS NULL AND interval IS NULL AND rolling_window_seconds IS NULL)
        OR (limit_value IS NOT NULL AND interval IS NOT NULL AND rolling_window_seconds IS NULL)
        OR (limit_value IS NOT NULL AND interval IS NULL AND rolling_window_seconds IS NOT NULL))
);

CREATE INDEX plan_entitlements_plan_idx    ON plan_entitlements (plan_id);
CREATE INDEX plan_entitlements_feature_idx ON plan_entitlements (feature_id);
```

`limit_value` is `bigint`, matching the `int64` in the API (DR-022). There is no `numeric` type
anywhere in this schema, because there are no fractional amounts (DR-022) and a `numeric` column
would invite one.

The `2^53 − 1` upper bound is not a `bigint` limit but a **scripting-language limit**: Lua 5.1
numbers are doubles, so a balance above `2^53 − 1` cannot be represented exactly by the code that
enforces the ceiling. Every amount, limit, bonus and balance is bounded to that range in the
database, in the API validation and in the script, so all three reject the same values. A metering
system that silently rounds a balance is worse than one that refuses the value.

`ON DELETE RESTRICT` makes a referenced plan or feature undeletable at the database level, which
is the second line of defence behind the `plan_in_use` and `feature_in_use` errors (DR-016).

> **Amended 2026-10-31 (`IP-17`) — a boolean entitlement has no limit and no interval.** A boolean
> feature is an inclusion with no counter (DR-050), so its row holds `null` for both, and
> `plan_entitlements_limit_pair` keeps the two null together. Which kind of feature a null pair
> belongs to is a rule across two tables, which a `CHECK` cannot express; `internal/db` reads the
> feature's `kind` in the transaction that writes the row and refuses a mismatch with
> `400 validation_failed` naming the field. A boolean entitlement is never pinned
> (section 2.10), never materialised as a balance hash (section 3.1) and never written to the
> ledger, because it has no cadence, no balance and no delta.

> **Amended 2026-10-31 (`IP-19`) — a rolling entitlement has a limit and a window and no interval.**
> A rolling feature is counted against a limit over a sliding window, with no calendar cycle
> (DR-052, ADR-0008), so its row holds `limit_value` and `rolling_window_seconds` and a null
> `interval`. `plan_entitlements_shape` replaces `plan_entitlements_limit_pair` and admits exactly
> three shapes: nothing (boolean), a limit and an interval (metered), a limit and a window
> (rolling). The window is stored in seconds and only the seven the contract names are
> representable. As for a boolean, which shape belongs to which kind is checked in `internal/db`
> in the same transaction, a rolling entitlement is never pinned, never materialised as a balance
> hash and never written to the ledger (Q-37).

### 2.4 Tenants

```sql
CREATE TABLE tenants (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  external_id   text NOT NULL UNIQUE,
  display_name  text NOT NULL,
  plan_id       uuid NOT NULL REFERENCES plans(id) ON DELETE RESTRICT,
  timezone      text NOT NULL DEFAULT 'UTC',
  anchor_at     timestamptz NOT NULL,
  state         tenant_state NOT NULL DEFAULT 'active',
  metadata      jsonb NOT NULL DEFAULT '{}'::jsonb,
  version       bigint NOT NULL DEFAULT 1,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  deleted_at    timestamptz,
  CONSTRAINT tenants_external_id_format
    CHECK (external_id ~ '^[A-Za-z0-9._:-]{1,128}$')
);

CREATE INDEX tenants_plan_idx ON tenants (plan_id);
```

The `external_id` character class deliberately excludes `@` (DR-034), which makes it structurally
impossible to store an email address as a tenant identifier. That is a schema-level guarantee
about the absence of PII, not a convention (ADR-0013, NFR-S2).

`anchor_at` is the only per-tenant cycle state (DR-002). There is no `current_cycle_start`
column, deliberately: a stored current window that could disagree with the anchor is precisely the
defect the pure-function design removes.

`metadata` is a small JSON object, explicitly not PII, not indexed by content, and excluded from
logs (DR-043, NFR-O6).

Indexes: the unique index on `external_id` serves lookups. `plan_id` is indexed for
`apply-now` and for impact previews (Q-14).

> **Amended 2026-10-19 (`IP-08`).** The sentence above already required `tenants_plan_idx`, and
> [request-lifecycle.md](request-lifecycle.md) calls the impact route "a single indexed count", but
> the DDL block did not contain the index. It does now, so the schema the migrations build and the
> schema this document states are the same text.

### 2.5 Tenant overrides

```sql
CREATE TYPE override_apply_at AS ENUM ('immediate', 'next_boundary');

CREATE TABLE tenant_entitlement_overrides (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  feature_id    uuid NOT NULL REFERENCES features(id) ON DELETE RESTRICT,
  limit_value   bigint,
  interval      reset_interval,
  apply_at      override_apply_at NOT NULL DEFAULT 'immediate',
  version       bigint NOT NULL DEFAULT 1,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, feature_id),
  CONSTRAINT tenant_overrides_limit_range
    CHECK (limit_value IS NULL OR (limit_value >= 0 AND limit_value <= 9007199254740991)),
  CONSTRAINT tenant_overrides_not_empty
    CHECK (limit_value IS NOT NULL OR interval IS NOT NULL)
);
```

Either column may be null, meaning "inherit from the plan for that aspect". Both null is a
validation error, since a row that inherits everything is indistinguishable from its absence.

`apply_at` is an enum, not a timestamp, and it is the only place in the schema where a
"when"-shaped column holds a *mode* rather than a time. Naming it `apply_at` is a deliberate
naming wart kept for the API's benefit, and it is the reason the enum type exists: a
`timestamptz DEFAULT 'immediate'` would not be a slightly awkward value, it would not parse. The
actual time an override takes effect is always the tenant's next boundary, computed by the cycle
engine, never stored here.

> **Amended 2026-10-20 (`IP-10`).** "Always the tenant's next boundary" is true of `next_boundary`
> only. With `immediate`, an override's limit re-limits the open cycle when it is written: the
> balance moves by the same difference as the limit, floored at zero, so what was spent stays
> spent. An override's **interval** reaches the tenant at its next re-anchor in either mode,
> because it is pinned (§2.10).

### 2.6 API keys

```sql
CREATE TABLE api_keys (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name          text NOT NULL,
  prefix        text NOT NULL,                    -- public, for display and audit
  key_hash      bytea NOT NULL,                    -- sha256(secret), never reversible
  scope         key_scope NOT NULL,
  state         key_state NOT NULL DEFAULT 'active',
  expires_at    timestamptz,
  last_used_at  timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  revoked_at    timestamptz,
  UNIQUE (prefix),
  CONSTRAINT api_keys_name_unique UNIQUE (name)
);
```

`prefix` is a short public string, unique, shown in audit records and in the admin list, and
recorded in logs in place of the key. It exists so a leaked key in a support ticket can be traced
to the key that leaked, which is the entire operational purpose of a credential identifier
(DR-043, K-1).

There is no `key_hash_index` beyond the primary key: authentication looks up by `prefix`, which is
unique and indexed, then verifies the SHA-256 digest ([ADR-0018](../decisions/0018-api-key-format-and-verifier.md);
this said Argon2id until 2026-10-19). The hash is deliberately not indexed, because
indexing a verifier invites a timing-oracle design and a scan.

### 2.7 Usage events (the ledger)

```sql
CREATE TABLE usage_events (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  seq            bigint GENERATED ALWAYS AS IDENTITY,
  tenant_id      uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  feature_id     uuid NOT NULL REFERENCES features(id) ON DELETE RESTRICT,
  cycle_index    bigint NOT NULL,
  delta          bigint NOT NULL,                  -- signed, never zero
  balance_after  bigint NOT NULL,
  source         event_source NOT NULL,
  event_type     event_type NOT NULL,
  request_id     text NOT NULL,
  idempotency_key text,
  metadata       jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at     timestamptz NOT NULL DEFAULT now(),
  cycle_start    timestamptz,                      -- the window the hash was in
  reconciles_key text,                             -- the key a reversal pair reconciles
  CONSTRAINT usage_events_delta_nonzero CHECK (delta <> 0),
  CONSTRAINT usage_events_seq_unique UNIQUE (seq)
);
```

`seq` is the ledger's order. `created_at DEFAULT now()` is the start of the *transaction*, so every
event a writer inserts in one batch carries the same timestamp, and an order by `created_at` alone
would read `balance_after` in an arbitrary order inside the batch. `seq` is assigned per row, in
insert order, and is the tiebreaker that newest-first pagination needs (api-conventions.md §9).

> **Amended 2026-10-19 (`IP-08`).** Section 2.2 already said ordering is "never by `id` — which is
> also why the ledger's `seq` column exists", but this block had no `seq`. The column is added here
> rather than the sentence deleted, because the within-batch order is the one the reconciliation
> identity of DR-042 is read in.

> **Amended 2026-10-20 (`IP-12`) — the writer sets `id` and `created_at`.** The ledger writer mints
> `id`, the `event_id` a consumer deduplicates on, as a UUIDv7 once, when the event is enqueued, and
> inserts with `ON CONFLICT DO NOTHING`, so a batch retried after a lost commit acknowledgement
> writes nothing twice. It writes `created_at` from the instant the request accepted, not the
> writer's transaction start, so the events route's window and retention both read when the
> mutation happened; the defaults stay for any other insert. `seq` is still the tiebreaker inside
> one instant.

> **Amended 2026-10-20 (`IP-12`) — `cycle_start`, and how a cycle is identified.** ADR-0016 says a
> row records the cycle window, and the block had only `cycle_index`. A re-anchor restarts the index
> at 0 and a force-rollover can lower it, so an index alone does not name a cycle. `cycle_start` is
> the start of the window the balance hash was in, taken from the same script reply as
> `cycle_index`, so the two always agree even when the caller's snapshot was stale. It is nullable
> only because rows written before migration `00010` have none; the writer always fills it.
>
> A cycle is `(tenant_id, feature_id, cycle_start, cycle_index)`. It is **opened** by a
> `cycle_rolled_over`, `plan_changed` or `force_rolled_over` row, whose `balance_after` is the
> cycle's opening allowance; a cycle with no opening row — a tenant's first, or one whose opening
> delta was zero and so wrote nothing (Q-22) — opens at the effective limit. A force-rollover
> refills the window it is in, so its row reopens the same cycle rather than a new one. The
> reconciliation identity is then: the last `balance_after` of a cycle equals its last opening plus
> the deltas of every row written after that opening. Every row carries its own `balance_after`, so
> the order of the rows after the opening does not matter to the sum.
`delta <> 0` is enforced, which makes U-8 in the state machines a database guarantee rather than
an application intention: an `amount: 0` consume cannot produce a row.

`balance_after` is stored so history is readable without replaying, and the reconciliation
identity of DR-042 is directly checkable: for a cycle, the opening allowance plus the sum of
deltas equals the final `balance_after`.

Indexes:

```sql
CREATE INDEX usage_events_tenant_time_idx ON usage_events (tenant_id, created_at DESC);
CREATE INDEX usage_events_tenant_cycle_idx ON usage_events (tenant_id, feature_id, cycle_index);
CREATE UNIQUE INDEX usage_events_idem_idx
  ON usage_events (tenant_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL AND event_type IN ('consumed', 'refunded');
CREATE INDEX usage_events_created_brin ON usage_events USING brin (created_at);
CREATE INDEX usage_events_reconciles_idx
  ON usage_events (tenant_id, reconciles_key) WHERE reconciles_key IS NOT NULL;
CREATE INDEX usage_events_cycle_seq_idx
  ON usage_events (tenant_id, feature_id, cycle_start, cycle_index, seq);
```

> **Amended 2026-10-20 (`IP-11`) — `reconciles_key`, and the order the verifier reads.** A
> duplicate reversal (DR-049) appends two rows: the second deduction itself, which the partial
> unique index refused under the customer's key, and the `system` refund that reverses it. Neither
> carries `idempotency_key`, so the index does not fire again; both carry the customer's key in
> `reconciles_key`, and the events route's key filter reads either column, so a disputed key returns
> the first deduction, the second and the reversal together. `usage_events_cycle_seq_idx` is the
> order the verifier reads one tenant-feature's cycles in, which `usage_events_tenant_cycle_idx`
> cannot give because it does not carry the window. The verifier checks every row against the one
> before it in that tenant-feature's history: a row's `balance_after` less its `delta` is the
> previous row's `balance_after`, across cycle boundaries as well as inside one, because every
> balance move is a row and a move of zero writes none (Q-22).

The BRIN index is the time index the retention job deletes by (section 5), as ADR-0016 names it.

The partial unique index is the asynchronous duplicate detector (DR-030). Its failure is a
detected anomaly, logged and counted, not a customer-visible error — by the time it fires, the
runtime store has already prevented the duplicate.

This table is append-only (INV-U2). There is no `UPDATE` and no `DELETE` path in the application;
retention is a bounded background deletion (DR-044, NFR-C6).

An event the writer could not append lands in `usage_event_dead_letters` (section 2.12), never
silently nowhere: a second deduction under one idempotency key, which the partial unique index
refuses, is kept there for reconciliation's reversal (DR-049).

### 2.8 Audit log

```sql
CREATE TABLE audit_log (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  actor_key_id uuid REFERENCES api_keys(id) ON DELETE SET NULL,
  actor        text NOT NULL,                    -- 'bootstrap', 'cli:<host>', or key name
  action       text NOT NULL,                    -- 'tenant.create', 'plan.apply_now', …
  subject_type text NOT NULL,                    -- 'tenant', 'plan', 'feature', 'api_key', 'system'
  subject_id   uuid,
  before       jsonb,
  after        jsonb,
  request_id   text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  reason       text,                             -- archive, delete, suspend, resume
  CONSTRAINT audit_log_subject_type
    CHECK (subject_type IN ('tenant', 'plan', 'feature', 'api_key', 'system'))
);

CREATE INDEX audit_log_subject_idx ON audit_log (subject_type, subject_id, created_at DESC);
CREATE INDEX audit_log_time_idx    ON audit_log (created_at DESC);
```

`subject_type` is a `CHECK` rather than an enum because it names a kind of resource rather than a
lifecycle state, and its values are exactly the `AuditEntry.subject_type` values the API
returns, so a row the API could not describe cannot be written.

`actor_key_id` is `ON DELETE SET NULL`, which would be the database itself updating an append-only
table. It never fires: an API key is revoked, which is a state change, and no path deletes an
`api_keys` row. The source check that holds `audit_log` append-only also refuses `DELETE FROM
api_keys` for that reason.

> **Amended 2026-10-19 (`IP-08`).** The `subject_type` constraint is new. The column was free text,
> while the API documents it as a four-value enum.

> **Amended 2026-10-20 (`IP-09`).** `reason` is new. Archive, delete, suspend and resume each
> require a reason on the wire, and the table had no column to keep it in, so the reason was
> required and then discarded. It is last because a migration adds it, and a fresh database and an
> upgraded one must have the same column order.

> **Amended 2026-10-28 (`IP-16`, Q-28).** `subject_type` gains `system`, for a row about the
> installation rather than one resource, and the contract's `AuditEntry.subject_type` gains it with
> it, so the API can still describe every row. Its one writer is `quotacore audit note`, which
> records the truncation a Postgres restore leaves in the history (consistency-and-recovery.md
> section 4.2, step 6); its `subject_id` is null and its action `system.note`.

The audit write happens **in the same transaction as the mutation it records** (NFR-D4). An audit
log that is written best-effort is a log, not an audit trail, and the distinction is the entire
reason this table is separate from `usage_events`.

Retention differs from events by design: events are operational history, audit is the contractual
record (Q-09, proposed 90 days and 400 days).

### 2.9 Migrations

Forward-only, applied at startup by an embedded `goose` binary (ADR-0011). No separate migration
container, no migration step in the quickstart, and no requirement for the operator to install
anything.

```sql
-- goose_db_version is maintained by goose, quotacore_migration_checksums by the
-- runner; no migration ships a down section, and the documented downgrade is a
-- restore (NFR-OPS3).
```

Each migration is a single transaction where possible. A migration that cannot be transactional,
such as one adding an index concurrently, is split into steps and its lock behaviour documented
in the migration comment.

> **Amended 2026-10-19 (`IP-08`).** No migration file ships a down section. The runner has no down
> path (`IP-01`, `TestUpOnlyIsStructural`), so a down block is SQL that can never run and never be
> tested, and a `DROP TABLE usage_events` sitting in the file set is exactly the hazard
> [ADR-0011](../decisions/0011-postgres-only-control-plane.md) describes; the ADR permits down
> migrations for development and does not require them. The rollback is a restore
> ([deployment.md](deployment.md#11-upgrade-and-rollback)). Each applied file's SHA-256 is recorded
> in `quotacore_migration_checksums`, which the runner creates itself, and a file whose bytes
> differ from the recorded hash refuses to start, which is the checksum rule ADR-0011 states and goose
> does not implement.

### 2.10 Tenant entitlement pins

```sql
CREATE TABLE tenant_entitlement_pins (
  tenant_id     uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  feature_id    uuid NOT NULL REFERENCES features(id) ON DELETE RESTRICT,
  interval      reset_interval NOT NULL,
  retired_limit bigint,
  retired_until timestamptz,
  PRIMARY KEY (tenant_id, feature_id),
  CONSTRAINT tenant_pins_retired_limit_range
    CHECK (retired_limit IS NULL OR (retired_limit >= 0 AND retired_limit <= 9007199254740991)),
  CONSTRAINT tenant_pins_retired_until_needs_limit
    CHECK (retired_until IS NULL OR retired_limit IS NOT NULL)
);

CREATE INDEX tenant_entitlement_pins_feature_idx ON tenant_entitlement_pins (feature_id);
```

A pin is what a tenant was anchored on: for each feature it holds, the cadence its cycle index is
counted in. A cycle index means something only under one anchor and one interval, so a plan edit
changes the offer and a tenant keeps its cadence and its feature set until its next re-anchor
(ADR-0006, E-3). The pins are written whenever a tenant is anchored: at creation (T-1), on a plan
change (T-5) and on an apply-now (P-3), from the plan's entitlements with an override's interval
in front. The limit is not pinned: it is the override's, else the plan's as it is now, which is
how a limit edit reaches the tenant at its own next boundary (DR-014).

A feature removed from the plan is **retired**, not dropped: `retired_limit` is the limit it had,
which a roll inside its last window still needs, and `retired_until` is the end of the window the
removal landed in, after which the tenant no longer has it (E-4, INV-E1). A retired `never` feature
has no window end, so `retired_until` is null and it stays until the re-anchor. Adding the feature
back before then un-retires the pin; a lapsed pin is deleted. A pin references its feature, so a
retired pin keeps a feature `feature_in_use` until the tenant's next re-anchor.

Migration `00009` pins every existing tenant to its plan's entitlements with its overrides'
intervals, which is exact because no route could edit a plan or write an override before it.

### 2.11 Pending re-anchors

```sql
CREATE TABLE pending_reanchors (
  tenant_id     uuid PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  anchor_at     timestamptz NOT NULL,
  kind          text NOT NULL,
  request_id    text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT pending_reanchors_kind CHECK (kind IN ('plan_change', 'apply_now'))
);

CREATE INDEX pending_reanchors_created_idx ON pending_reanchors (created_at);
```

A re-anchor committed in Postgres whose balance hashes have not been rewritten yet. The row is
written in the transaction that moves the anchor and the pins, and deleted in the transaction that
finishes the rewrite, so a crash between the two stores leaves work to do rather than a tenant
served under the wrong numbering. The request drains its own tenants inline while it has time, and
every instance drains the rest at start and on each refresh tick, a hundred tenants at a time
([consistency-and-recovery.md](consistency-and-recovery.md#3-dependency-failure-matrix)). Every
write that changes one tenant's balances drains that tenant first, under the same row lock.

### 2.12 Usage event dead letters

```sql
CREATE TABLE usage_event_dead_letters (
  event_id    uuid PRIMARY KEY,
  reason      text NOT NULL,
  payload     jsonb NOT NULL,
  error       text NOT NULL,
  attempts    integer NOT NULL DEFAULT 1,
  created_at  timestamptz NOT NULL DEFAULT now(),
  resolved_at timestamptz,
  resolution  text,
  CONSTRAINT usage_event_dead_letters_reason
    CHECK (reason IN ('duplicate_idempotency_key', 'rejected')),
  CONSTRAINT usage_event_dead_letters_resolution
    CHECK ((resolved_at IS NULL) = (resolution IS NULL)
           AND (resolution IS NULL OR resolution IN ('reversed', 'tenant_deleted', 'referred')))
);

CREATE INDEX usage_event_dead_letters_created_idx ON usage_event_dead_letters (created_at);
```

An event the ledger refused, kept whole in `payload` under its own `event_id` so it is visible and
replayable rather than lost (INV-U3). Added 2026-10-20 by `IP-12`. `duplicate_idempotency_key` is a
second deduction the partial unique index refused: the shape a total store loss produces (FS-22),
and the input reconciliation reverses (DR-049); it is never replayed into the ledger.
`rejected` is any other permanent refusal, such as a feature key that resolves to no feature; the
retention job re-attempts each one on every tick, deletes it in the transaction that appends it,
and raises `attempts` when it fails again. This table is not the ledger and is not append-only.

> **Amended 2026-10-20 (`IP-11`) — a duplicate is resolved, and is appended once.** "Never replayed
> into the ledger" holds for the duplicate **under its key**. Reconciliation's pass reverses it
> (DR-049): in one transaction it appends the duplicate with its key moved to `reconciles_key`,
> appends the reversal, writes a `tenant.reverse_duplicate` audit row and sets `resolved_at` and
> `resolution` to `reversed`. A deleted tenant's duplicate is resolved `tenant_deleted` with no
> refund; a refund the ceiling refuses (DR-021), and a duplicate `refunded`, which DR-049 does not
> reverse, are resolved `referred` with a finding for a person. Until it is reversed, a duplicate is
> part of the ledger the verifier reads, because the store applied it.

### 2.13 Reconciliation findings and checkpoint

```sql
CREATE TABLE reconcile_findings (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  fingerprint  text NOT NULL,
  kind         text NOT NULL,
  tenant_id    uuid,
  feature_key  text,
  cycle_index  bigint,
  cycle_start  timestamptz,
  expected     bigint,
  actual       bigint,
  detail       text NOT NULL,
  found_by     text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT reconcile_findings_fingerprint UNIQUE (fingerprint),
  CONSTRAINT reconcile_findings_kind CHECK (kind IN ('identity', 'chain', 'hash_balance', 'hash_cycle',
    'hash_missing', 'no_cycle_start', 'reversal_refused', 'duplicate_refund', 'unreconcilable')),
  CONSTRAINT reconcile_findings_found_by CHECK (found_by IN ('pass', 'verify', 'rebuild'))
);

CREATE INDEX reconcile_findings_created_idx ON reconcile_findings (created_at DESC);

CREATE TABLE reconcile_checkpoint (
  id          boolean PRIMARY KEY DEFAULT true,
  events_seq  bigint NOT NULL DEFAULT 0,
  gaps_id     bigint NOT NULL DEFAULT 0,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT reconcile_checkpoint_one_row CHECK (id)
);
```

Added 2026-10-20 by `IP-11`. A finding is what reconciliation found, kept so it outlives the log
line and the pass that found it; `fingerprint` names one disagreement, so a pass that sees it again
inserts nothing and `quotacore_event_reconcile_mismatch_total` rises once per finding. Nothing
corrects a balance because a finding exists (DR-042). The checkpoint is how far the in-service pass
has read, so each pass verifies only the tenant-features whose rows or gaps arrived since.

### 2.14 Usage event gaps

```sql
CREATE TABLE usage_event_gaps (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  session_id   uuid,
  tenant_id    uuid,
  feature_key  text,
  reason       text NOT NULL,
  events       bigint NOT NULL,
  first_at     timestamptz NOT NULL,
  last_at      timestamptz NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT usage_event_gaps_reason CHECK (reason IN ('queue_full', 'error', 'shutdown')),
  CONSTRAINT usage_event_gaps_events CHECK (events > 0)
);
```

Added 2026-10-20 by `IP-11`. What a ledger writer dropped, tallied by tenant and feature over the
instants the dropped mutations happened, so a rebuild knows which cycles it cannot call exact. It
is a record that something is missing, kept outside the ledger, not a synthetic event, which
consistency-and-recovery.md section 4.5 forbids. A `NULL` `tenant_id` means every tenant between
`first_at` and `last_at`: the tally is bounded in memory, and one that overflows writes that.

### 2.15 Ledger sessions

```sql
CREATE TABLE ledger_sessions (
  id               uuid PRIMARY KEY,
  host             text NOT NULL,
  started_at       timestamptz NOT NULL DEFAULT now(),
  settled_through  timestamptz NOT NULL DEFAULT now(),
  last_seen_at     timestamptz NOT NULL DEFAULT now(),
  closed_at        timestamptz
);
```

Added 2026-10-20 by `IP-11`. One row per ledger writer for its life. `settled_through` is an
instant before which every event the writer accepted has been written, dead-lettered or tallied as
a gap; `last_seen_at` is refreshed on a heartbeat while the writer lives. A clean drain sets
`closed_at`. A session left open whose `last_seen_at` has gone stale is a crash, and a rebuild
reads it as a gap for every tenant from `settled_through` to `last_seen_at` plus a heartbeat.

---

## 3. Redis-compatible data model

### 3.1 Key layout

Every key carries a per-tenant hash tag, so all of one tenant's keys occupy one slot and a
multi-key script is single-slot (ADR-0002, A-11).

| Key | Type | Contents | TTL |
| --- | --- | --- | --- |
| `qc:{t:<tenant_id>}:bal:<feature_key>` | hash | `balance`, `limit`, `bonus`, `cycle_index`, `window_start`, `window_end` | none. Amended 2026-10-20: this was `window_end + 24h`, which locked out a tenant idle for a day past its window (INV-C3) |
| `qc:{t:<tenant_id>}:idem:<key>` | string | the recorded decision state: the fingerprint, the transition verdict and the six balance fields (§3.3) | 24 h |
| `qc:{t:<tenant_id>}:f:<feature_key>` | hash | rolling-window bucket counters (v0.2): field = a bucket's start in Unix milliseconds, value = the units consumed in it; at most one window's 60 buckets (DR-052) | the newest bucket's start plus the window, refreshed by every write. Amended 2026-10-31 by `IP-19`: this was `window_end + 24h`, a cycle's end, and a rolling window has no cycle |
| `qc:{t:<tenant_id>}:w:<feature_key>` | string | reserved; not written. Amended 2026-10-31 by `IP-19`: a boundary marker was planned beside the buckets, and the buckets' own field names are their boundaries, so nothing needs one | — |
| `qc:cfg:tenants` | hash | tenant → snapshot version, for invalidation diffing | none |
| `qc:cfg:version` | string | monotonically increasing configuration version | none |
| `qc:cfg:cursor_key` | string | the 32-byte key that signs admin list cursors, created once with `SET NX` so every instance signs alike (`IP-09`) | none |
| `qc:admin:cnf:<token>` | hash | `plan_id`, `resource_version`, `affected_tenants`, `reason_hash` for a pending `apply-now` confirmation. `reason_hash` is the SHA-256 of the request's `reason`, so the reason itself never reaches the fast store; the token is read and deleted in one transaction, which is what makes it single-use (`IP-10`) | `QUOTACORE_CONFIRMATION_TTL` |

A boolean entitlement has no key of its own (DR-050, amended 2026-10-31 by `IP-17`): it has no
balance hash, and a `consume` against it writes only its idempotency record, with the verdict
`current` and the six balance fields empty. ADR-0002's sketch of a `:f:` string for a boolean
entitlement was not adopted; inclusion is configuration, read from the snapshot, and `:f:` stays the
rolling-window key above.

There is deliberately **no** `qc:idem:fingerprint:<key>` key. The fingerprint is a field of the
per-tenant idempotency record, which is what scopes it: a key that was not scoped to a tenant
would either need a hash tag to stay in the same slot as the balance key or would collide between
tenants that happen to choose the same key, and both are avoidable by not creating it.

The `{t:...}` tag contains a colon, which is legal inside a hash tag, and the tenant identifier is
a UUID. The tag is what makes `{t:01JC…}` and `{t:01JCB…}` distinguishable, and getting this
wrong silently splits a tenant's keys across slots, which breaks every multi-key script. The key
builder is therefore a single tested function with a test asserting the tag boundaries, not an
inline format string at each call site.

### 3.2 The balance hash

A hash rather than a single serialised string, because the atomic script must read one field and
write another without rewriting the whole object, and because partial reads are then possible
under `HGET` for the read-only `balance` path.

```
HGET qc:{t:T}:bal:llm.tokens.output balance        →  "41250"
HGET qc:{t:T}:bal:llm.tokens.output limit          →  "1000000"
HGET qc:{t:T}:bal:llm.tokens.output bonus          →  "0"
HGET qc:{t:T}:bal:llm.tokens.output cycle_index    →  "3"
HGET qc:{t:T}:bal:llm.tokens.output window_start   →  "2026-09-01T00:00:00Z"  (epoch ms)
HGET qc:{t:T}:bal:llm.tokens.output window_end     →  "2026-10-01T00:00:00Z"  (epoch ms)
```

Windows are stored as epoch milliseconds and rendered as RFC 3339 with offset in responses
(DR-001, and the API conventions). Storing a formatted string with a zone would make the script
parse a timestamp on every call, which is both slower and a place for a subtle bug.

All six fields are always present, and an entitlement that never resets stores `window_end` as the
**empty string**, not as a missing field. `HMGET` answers an absent field and an empty one with the
same value, `false`, so a missing `window_end` is indistinguishable from a `never` one that was
never written: a hash missing any of the six fields is a fault and is answered `state_missing`, and
`IP-05` proved that against a real store. The control-plane writer in `IP-09` therefore writes six
fields or none, and a `never` entitlement is the empty string rather than a sentinel timestamp a
caller could collide with.

A missing hash is a fault, never an initialisation opportunity. The hash is materialised when the
tenant is provisioned or its plan assignment changes, and is then maintained by the scripts; nothing
else creates it (DR-045). This is what makes a lost hash survivable without being exploitable:
whether it is removed by an operator's mistake or lost to total store loss, the consequence is a
`503` plus a metric plus a rebuild, never a restored full allowance.

> **Amended 2026-10-20 (`IP-10`).** "Its plan assignment changes" covers a plan change and an
> apply-now, which re-anchor, and force-rollover refills a hash in place. In each case the hash is
> rewritten by a control-plane script after the change commits, with six fields or none, and never
> by a request (DR-045). The re-anchor's rewrite is a compare-and-set on the stored window, so two
> drains of one tenant change it once (§2.11).

### 3.3 The idempotency record

A single string holding the operation fingerprint followed by the seven values that answer the
operation: the transition verdict and the six balance-hash fields, as they stood after the
mutation. The fields are separated by tabs, which cannot occur in a fingerprint (hex), a verdict
(`current`, `rolled`, `stale`) or a decimal integer, so the boundary between them is unambiguous.

```
SET qc:{t:T}:idem:01JCV3… 'a3f1…<TAB>current<TAB>70<TAB>100<TAB>0<TAB>5<TAB>1759200000000<TAB>1761792000000' EX 86400
```

The record is written by the same script execution that applies the mutation, and only when the
mutation is applied: a denial, a refused ceiling and a stale state are answers rather than changes,
and a record of one would let a later retry replay a charge that never happened (ADR-0004, DR-025).
The write needs no `NX`, because the check and the write are the same execution, which is the whole
of the atomicity argument (ADR-0002). A repeat whose fingerprint matches returns the recorded seven
values without reading the balance; a repeat whose fingerprint differs is `409 idempotency_key_reuse`
and touches nothing (DR-027, DR-028). The fingerprint is a hash of `operation | tenant | feature |
amount`; the tenant is also the key's scope, so a key chosen by one tenant can never collide with
another's.

### 3.4 Invalidation channel

```
PUBLISH qc:cfg:invalidate {"version":"4471"}
```

Subscribers bump their local snapshot version and refresh on the next request or immediately,
whichever the configuration selects. The message carries no tenant data — only a version — so a
compromised subscriber learns nothing beyond "configuration changed".

If pub/sub is unavailable, the periodic refresh interval is the fallback, and the observed
propagation delay widens to the interval. That is a documented degradation (NFR-D3), and it is why
the interval is a tuned value with a metric rather than a constant.

> **Amended 2026-10-29 (`IP-16`, the v0.1 code review).** The message also carries the moment it
> was published, `{"version":"4471","published_at_ms":"1761700000000"}`, which is still no tenant
> data. A subscriber measures `quotacore_invalidation_lag_seconds` from it, so the metric
> is the time from publish to applied that observability.md defines. Before the amendment the lag
> was timed from the previous message's arrival and measured the quiet between changes. A message
> without the field, from an older publisher, is timed from its arrival.

---

## 4. Invariants spanning both stores

| ID | Invariant | Enforced by |
| --- | --- | --- |
| INV-X1 | The runtime balance never requires a Postgres read | Architecture rule DR-039, review-checked |
| INV-X2 | Every applied mutation has exactly one event | Ledger writer, `delta <> 0` constraint, and the reconciliation test |
| INV-X3 | Every administrative mutation has an audit row in the same transaction | Transaction scope in the admin store |
| INV-X4 | `cycle_index` never decreases under one anchor | The monotonic guard in the Lua transition, and the anchor being the only stored cycle state. Amended 2026-10-20 (`IP-10`): a re-anchor restarts the index at 0 (C-7) and a force-rollover can lower it (C-5); both are audited control-plane writes, and no data-plane path writes a smaller index |
| INV-X5 | `balance <= limit + bonus` at every instant, for every key | The ceiling check inside the atomic script (DR-019) |
| INV-X6 | No runtime key expires before its cycle could legitimately need it | A balance hash has no TTL. Amended 2026-10-20: the TTL was `window_end + 24h`, and an idle tenant could need its key after that (INV-C3) |
| INV-X7 | A deleted tenant has no runtime keys within one refresh interval | Soft delete publishes an invalidation and the script refuses unknown or deleted tenants |
| INV-X8 | Postgres and the runtime store never disagree about a *schedule*; they may transiently disagree about a *value* | Anchor is Postgres-only and the script recomputes from it, so a stale snapshot can only delay a reset, never skip or duplicate one |

INV-X8 is the subtle one and it is why the split is safe. A stale configuration cannot produce a
wrong *boundary*, because boundaries are computed from the anchor at request time inside the
script. It can only produce a stale *limit*, and that is bounded by the refresh interval and
corrected at the next cycle.

---

## 5. Retention and growth

| Table | Growth driver | Default retention | Notes |
| --- | --- | --- | --- |
| `usage_events` | One row per applied mutation | 90 days (Q-09) | At NFR-T8 for one tenant, the table is dominated by the highest-volume tenant. Partitioning by month is the planned mechanism if retention queries degrade |
| `audit_log` | One row per admin mutation | 400 days (Q-09) | Low volume, high value. Never dropped by API (DR-044) |
| All configuration tables | Human changes | indefinite | Bounded by features × plans × tenants; small |
| `qc:*:idem:*` | One key per logical operation | 24 h (DR-029) | The largest consumer in the fast store by a wide margin, and the one the sizing floor in [deployment.md](deployment.md#5-sizing) exists for. Bounded by TTL in time and by the throughput envelope in volume, and **never evicted** (DR-048) |
| `qc:*:bal:*` | One key per tenant-feature | none | Bounded by tenants times the features each was ever entitled to. Amended 2026-10-20: this was `window_end + 24h` (INV-C3); a deleted tenant's and a retired feature's hashes now stay until removed, because deleting a hash on soft deletion would answer an in-flight request `503`, which T-15 does not permit |

The retention job deletes in bounded batches with a `DELETE` using the time index, never a single
unbounded statement, and it runs with a statement timeout. A retention job that locks a table for
an hour is a self-inflicted outage.

> **Amended 2026-10-20 (`IP-12`).** The job runs in every instance on
> `QUOTACORE_RETENTION_INTERVAL` and takes a transaction-scoped advisory lock, so one instance prunes
> at a time; it deletes by `created_at` alone, 5 000 rows a batch under a 5 s statement timeout, and
> its `DELETE` statements are the only ones the source check permits on either append-only table.
> Deletion is by age only, so a cycle longer than the event retention — a yearly or `never`
> entitlement under the 90-day default — loses its earliest rows, and a rebuild of that cycle from
> the ledger is no longer exact ([deployment.md](deployment.md#44-ledger-and-retention)).

---

## 6. Migration policy

1. **Forward-only in production.** A `down` migration exists for development convenience and is
   never run against a live database by the application.
2. **Additive first.** A release that removes or renames a column is two releases: stop writing it,
   then drop it. The deprecation window is at least one minor release.
3. **No destructive change without a documented restore path.** The rollback procedure for any
   release is: restore Postgres from backup, redeploy the previous image, rebuild the fast store.
   That is always possible, which is why the fast store needs no backup (NFR-OPS4).
4. **Enums grow, never shrink.** Adding a value to `event_type` or `feature_kind` is additive and
   safe. Removing one is a breaking change requiring a major version, because an old reader would
   fail on an unknown value.
5. **Every migration is idempotent where it can be.** `CREATE TABLE IF NOT EXISTS`, guarded index
   creation. A partially-applied migration that can be re-run safely is worth the small extra
   complexity.
6. **The schema is the contract for the ledger.** `usage_events` is a documented interface for the
   customer's own reporting and restore drills. Changing a column name or type is a breaking change
   for them, not just for us, and is announced as such.
