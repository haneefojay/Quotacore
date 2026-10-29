-- +goose Up
-- data-model.md section 2.7: the ledger. Append-only (INV-U2); delta is never
-- zero, so a denial or an amount-0 call cannot produce a row. seq is the
-- insert order, because created_at is the transaction start and a batch shares
-- it. The partial unique index detects a duplicate after the fact (DR-030); it
-- never prevents one, and its violation is an anomaly, not a customer error.
CREATE TABLE IF NOT EXISTS usage_events (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  seq             bigint GENERATED ALWAYS AS IDENTITY,
  tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  feature_id      uuid NOT NULL REFERENCES features(id) ON DELETE RESTRICT,
  cycle_index     bigint NOT NULL,
  delta           bigint NOT NULL,
  balance_after   bigint NOT NULL,
  source          event_source NOT NULL,
  event_type      event_type NOT NULL,
  request_id      text NOT NULL,
  idempotency_key text,
  metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT usage_events_delta_nonzero CHECK (delta <> 0),
  CONSTRAINT usage_events_seq_unique UNIQUE (seq)
);

CREATE INDEX IF NOT EXISTS usage_events_tenant_time_idx
  ON usage_events (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS usage_events_tenant_cycle_idx
  ON usage_events (tenant_id, feature_id, cycle_index);
CREATE UNIQUE INDEX IF NOT EXISTS usage_events_idem_idx
  ON usage_events (tenant_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL AND event_type IN ('consumed', 'refunded');
