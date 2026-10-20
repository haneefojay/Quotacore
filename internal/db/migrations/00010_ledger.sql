-- +goose Up
-- data-model.md sections 2.7 and 2.12, added by IP-12.
--
-- cycle_start is the start of the window the balance hash was in when the
-- mutation was applied, taken from the same script reply as cycle_index, so
-- the two always agree. An index alone does not name a cycle: a re-anchor
-- restarts it at 0 and a force-rollover can lower it (C-5, C-7), so a cycle is
-- (tenant_id, feature_id, cycle_start, cycle_index). It is nullable only
-- because a row written before this migration has no window; the writer always
-- fills it. It is added last, so a fresh database and an upgraded one have the
-- same column order.
ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS cycle_start timestamptz;

-- The time index the retention job deletes by, as ADR-0016 names it. created_at
-- is written by the writer from the instant the request accepted, so it rises
-- with the insert order to within the drain lag, which is what BRIN needs.
CREATE INDEX IF NOT EXISTS usage_events_created_brin ON usage_events USING brin (created_at);

-- An event the ledger refused, kept whole under its own event_id so it is
-- visible and replayable rather than lost (INV-U3). duplicate_idempotency_key is
-- a second deduction the partial unique index refused (FS-22), reconciliation's
-- input for the reversal (DR-049), and is never replayed into the ledger;
-- rejected is any other permanent refusal, which the retention job re-attempts
-- on every tick. This table is not the ledger and is not append-only.
CREATE TABLE IF NOT EXISTS usage_event_dead_letters (
  event_id    uuid PRIMARY KEY,
  reason      text NOT NULL,
  payload     jsonb NOT NULL,
  error       text NOT NULL,
  attempts    integer NOT NULL DEFAULT 1,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT usage_event_dead_letters_reason
    CHECK (reason IN ('duplicate_idempotency_key', 'rejected'))
);

CREATE INDEX IF NOT EXISTS usage_event_dead_letters_created_idx ON usage_event_dead_letters (created_at);
