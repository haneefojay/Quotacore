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
CREATE TYPE feature_kind       AS ENUM ('metered', 'boolean');   -- 'boolean' unused in v0.1
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
  limit_value      bigint NOT NULL,
  interval         reset_interval NOT NULL,
  UNIQUE (plan_id, feature_id),                       -- INV-E2
  CONSTRAINT plan_entitlements_limit_nonneg CHECK (limit_value >= 0),
  -- Lua 5.1 numbers are doubles: above 2^53-1 integer arithmetic stops being
  -- exact, so a balance in that range cannot be represented faithfully in the
  -- script that enforces it. Bounded here so the database, the API and the
  -- script all reject the same values. See security-model.md section 6.
  CONSTRAINT plan_entitlements_limit_range
    CHECK (limit_value <= 9007199254740991)
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

### 2.6 API keys

```sql
CREATE TABLE api_keys (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name          text NOT NULL,
  prefix        text NOT NULL,                    -- public, for display and audit
  key_hash      bytea NOT NULL,                    -- Argon2id, never reversible
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
unique and indexed, then verifies the Argon2id hash. The hash is deliberately not indexed, because
indexing a verifier invites a timing-oracle design and a scan.

### 2.7 Usage events (the ledger)

```sql
CREATE TABLE usage_events (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
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
  CONSTRAINT usage_events_delta_nonzero CHECK (delta <> 0)
);
```

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
```

The partial unique index is the asynchronous duplicate detector (DR-030). Its failure is a
detected anomaly, logged and counted, not a customer-visible error — by the time it fires, the
runtime store has already prevented the duplicate.

This table is append-only (INV-U2). There is no `UPDATE` and no `DELETE` path in the application;
retention is a bounded background deletion (DR-044, NFR-C6).

### 2.8 Audit log

```sql
CREATE TABLE audit_log (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  actor_key_id uuid REFERENCES api_keys(id) ON DELETE SET NULL,
  actor        text NOT NULL,                    -- 'bootstrap', 'cli:<host>', or key name
  action       text NOT NULL,                    -- 'tenant.create', 'plan.apply_now', …
  subject_type text NOT NULL,                    -- 'tenant', 'plan', 'feature', 'api_key'
  subject_id   uuid,
  before       jsonb,
  after        jsonb,
  request_id   text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_subject_idx ON audit_log (subject_type, subject_id, created_at DESC);
CREATE INDEX audit_log_time_idx    ON audit_log (created_at DESC);
```

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
-- schema_migrations is maintained by goose
-- down migrations are shipped for emergency rollback during development only;
-- in production the documented downgrade is a restore (NFR-OPS3).
```

Each migration is a single transaction where possible. A migration that cannot be transactional,
such as one adding an index concurrently, is split into steps and its lock behaviour documented
in the migration comment.

---

## 3. Redis-compatible data model

### 3.1 Key layout

Every key carries a per-tenant hash tag, so all of one tenant's keys occupy one slot and a
multi-key script is single-slot (ADR-0002, A-11).

| Key | Type | Contents | TTL |
| --- | --- | --- | --- |
| `qc:{t:<tenant_id>}:bal:<feature_key>` | hash | `balance`, `limit`, `bonus`, `cycle_index`, `window_start`, `window_end` | `window_end + 24h`, none if `never` |
| `qc:{t:<tenant_id>}:idem:<key>` | string | the recorded decision state: the fingerprint, the transition verdict and the six balance fields (§3.3) | 24 h |
| `qc:{t:<tenant_id>}:f:<feature_key>` | hash | rolling-window bucket counters (v0.2) | `window_end + 24h` |
| `qc:{t:<tenant_id>}:w:<feature_key>` | string | rolling-window bucket boundary marker (v0.2) | `window_end + 24h` |
| `qc:cfg:tenants` | hash | tenant → snapshot version, for invalidation diffing | none |
| `qc:cfg:version` | string | monotonically increasing configuration version | none |
| `qc:admin:cnf:<token>` | hash | `plan_id`, `resource_version`, `affected_tenants`, `reason_hash` for a pending `apply-now` confirmation | `QUOTACORE_CONFIRMATION_TTL` |

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

---

## 4. Invariants spanning both stores

| ID | Invariant | Enforced by |
| --- | --- | --- |
| INV-X1 | The runtime balance never requires a Postgres read | Architecture rule DR-039, review-checked |
| INV-X2 | Every applied mutation has exactly one event | Ledger writer, `delta <> 0` constraint, and the reconciliation test |
| INV-X3 | Every administrative mutation has an audit row in the same transaction | Transaction scope in the admin store |
| INV-X4 | `cycle_index` never decreases | The monotonic guard in the Lua transition, and the anchor being the only stored cycle state |
| INV-X5 | `balance <= limit + bonus` at every instant, for every key | The ceiling check inside the atomic script (DR-019) |
| INV-X6 | No runtime key expires before its cycle could legitimately need it | TTL is `window_end + 24h`, strictly greater than any rollover requirement |
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
| `qc:*:bal:*` | One key per tenant-feature | `window_end + 24h` | Self-limiting; no dangling keys accumulate beyond one cycle plus a day |

The retention job deletes in bounded batches with a `DELETE` using the time index, never a single
unbounded statement, and it runs with a statement timeout. A retention job that locks a table for
an hour is a self-inflicted outage.

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
