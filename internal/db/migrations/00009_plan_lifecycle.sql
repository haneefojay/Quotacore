-- +goose Up
-- data-model.md sections 2.10 and 2.11, added by IP-10.
--
-- tenant_entitlement_pins is what a tenant was anchored on: for each feature,
-- the cadence its cycle index is counted in. A plan edit changes the offer, and
-- a tenant keeps its cadence and its feature set until its next re-anchor,
-- because a cycle index only means something under one anchor and one interval
-- (ADR-0006, E-3). A feature removed from the plan is retired rather than
-- dropped, and stays until retired_until, the end of the window it was removed
-- in (E-4, INV-E1); retired_limit is the limit it had, which a roll inside that
-- window still needs. A retired `never` feature has no window end and stays
-- until the re-anchor.
CREATE TABLE IF NOT EXISTS tenant_entitlement_pins (
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

CREATE INDEX IF NOT EXISTS tenant_entitlement_pins_feature_idx ON tenant_entitlement_pins (feature_id);

-- pending_reanchors is a re-anchor committed in Postgres whose balance hashes
-- have not yet been rewritten. The row is written in the transaction that
-- moves the anchor and deleted in the one that finishes the rewrite, so a
-- crash between the two stores leaves work to do rather than a tenant served
-- under the wrong numbering (consistency-and-recovery.md section 3).
CREATE TABLE IF NOT EXISTS pending_reanchors (
  tenant_id     uuid PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  anchor_at     timestamptz NOT NULL,
  kind          text NOT NULL,
  request_id    text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT pending_reanchors_kind CHECK (kind IN ('plan_change', 'apply_now'))
);

CREATE INDEX IF NOT EXISTS pending_reanchors_created_idx ON pending_reanchors (created_at);

-- Every existing tenant is pinned to what it was provisioned with. No route
-- could edit a plan or write an override before this migration, so the plan's
-- entitlements, with any override's cadence, are exactly that.
INSERT INTO tenant_entitlement_pins (tenant_id, feature_id, interval)
SELECT t.id, e.feature_id, coalesce(o.interval, e.interval)
  FROM tenants t
  JOIN plan_entitlements e ON e.plan_id = t.plan_id
  LEFT JOIN tenant_entitlement_overrides o ON o.tenant_id = t.id AND o.feature_id = e.feature_id
ON CONFLICT DO NOTHING;
