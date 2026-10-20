-- +goose Up
-- data-model.md sections 2.7, 2.12, 2.13, 2.14 and 2.15, added by IP-11.
--
-- reconciles_key is the customer's idempotency key carried by the two rows a
-- duplicate reversal appends: the second deduction itself, whose own key the
-- partial unique index refused, and the system refund that reverses it
-- (DR-049, IP-11 D4). idempotency_key stays NULL on both, so the index that
-- caught the duplicate does not fire again, and the events route's key filter
-- reads either column (D9). Added last, so a fresh database and an upgraded
-- one have the same column order.
ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS reconciles_key text;

CREATE INDEX IF NOT EXISTS usage_events_reconciles_idx
  ON usage_events (tenant_id, reconciles_key) WHERE reconciles_key IS NOT NULL;

-- The order the verifier reads one tenant-feature's cycles in (S8):
-- usage_events_tenant_cycle_idx does not carry the window.
CREATE INDEX IF NOT EXISTS usage_events_cycle_seq_idx
  ON usage_events (tenant_id, feature_id, cycle_start, cycle_index, seq);

-- A duplicate dead letter is resolved, never deleted: reversed (appended to the
-- ledger with its reversal), tenant_deleted (no refund for a tenant that no
-- longer exists), or referred to a person (a refund the ceiling refused, or a
-- duplicate refund, which DR-049 does not reverse).
ALTER TABLE usage_event_dead_letters ADD COLUMN IF NOT EXISTS resolved_at timestamptz;
ALTER TABLE usage_event_dead_letters ADD COLUMN IF NOT EXISTS resolution text;
ALTER TABLE usage_event_dead_letters DROP CONSTRAINT IF EXISTS usage_event_dead_letters_resolution;
ALTER TABLE usage_event_dead_letters ADD CONSTRAINT usage_event_dead_letters_resolution
  CHECK ((resolved_at IS NULL) = (resolution IS NULL)
         AND (resolution IS NULL OR resolution IN ('reversed', 'tenant_deleted', 'referred')));

-- What reconciliation found, so a finding outlives the log line and the pass
-- that found it (IP-11 D3, D7). fingerprint names one disagreement, so a pass
-- that sees it again inserts nothing and the mismatch counter rises once.
CREATE TABLE IF NOT EXISTS reconcile_findings (
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

CREATE INDEX IF NOT EXISTS reconcile_findings_created_idx ON reconcile_findings (created_at DESC);

-- How far the in-service pass has read: the last usage_events seq and the
-- last usage_event_gaps id it verified. One row.
CREATE TABLE IF NOT EXISTS reconcile_checkpoint (
  id          boolean PRIMARY KEY DEFAULT true,
  events_seq  bigint NOT NULL DEFAULT 0,
  gaps_id     bigint NOT NULL DEFAULT 0,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT reconcile_checkpoint_one_row CHECK (id)
);

-- What the ledger lost (IP-11 D8): the events a writer dropped, tallied by
-- tenant and feature, kept outside the ledger because a synthetic event is
-- what consistency-and-recovery.md section 4.5 forbids. A NULL tenant_id means
-- every tenant in [first_at, last_at], which is what an overflowing tally
-- writes.
CREATE TABLE IF NOT EXISTS usage_event_gaps (
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

-- One row per ledger writer for its life (IP-11 D8). settled_through is an
-- instant before which every event the writer accepted has been written,
-- dead-lettered or tallied as a gap; a session left open whose last_seen_at
-- has gone stale is a crash, and everything it accepted from settled_through
-- on may be lost.
CREATE TABLE IF NOT EXISTS ledger_sessions (
  id               uuid PRIMARY KEY,
  host             text NOT NULL,
  started_at       timestamptz NOT NULL DEFAULT now(),
  settled_through  timestamptz NOT NULL DEFAULT now(),
  last_seen_at     timestamptz NOT NULL DEFAULT now(),
  closed_at        timestamptz
);
