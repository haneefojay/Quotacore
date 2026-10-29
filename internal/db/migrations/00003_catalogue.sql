-- +goose Up
-- data-model.md sections 2.2 and 2.3: the feature catalogue, plans, and the
-- entitlement joining them. key is immutable (INV-F3) and unit is immutable
-- once referenced (INV-F2); both are enforced in internal/db, not here.
CREATE TABLE IF NOT EXISTS features (
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

CREATE TABLE IF NOT EXISTS plans (
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

-- limit_value is bounded to 2^53-1 because the enforcing script's numbers are
-- doubles; the database, the API and the script reject the same values.
CREATE TABLE IF NOT EXISTS plan_entitlements (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  plan_id          uuid NOT NULL REFERENCES plans(id) ON DELETE RESTRICT,
  feature_id       uuid NOT NULL REFERENCES features(id) ON DELETE RESTRICT,
  limit_value      bigint NOT NULL,
  interval         reset_interval NOT NULL,
  UNIQUE (plan_id, feature_id),
  CONSTRAINT plan_entitlements_limit_nonneg CHECK (limit_value >= 0),
  CONSTRAINT plan_entitlements_limit_range
    CHECK (limit_value <= 9007199254740991)
);

CREATE INDEX IF NOT EXISTS plan_entitlements_plan_idx    ON plan_entitlements (plan_id);
CREATE INDEX IF NOT EXISTS plan_entitlements_feature_idx ON plan_entitlements (feature_id);
