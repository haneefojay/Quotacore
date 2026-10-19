-- +goose Up
-- data-model.md sections 2.4 and 2.5. plan_id is a single NOT NULL column, so
-- a second simultaneous plan is unrepresentable (DR-011). The external_id
-- character class excludes '@', so an email address cannot be stored as a
-- tenant identifier (DR-034, ADR-0013, NFR-S2).
CREATE TABLE IF NOT EXISTS tenants (
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

CREATE INDEX IF NOT EXISTS tenants_plan_idx ON tenants (plan_id);

-- Either column may be null, meaning "inherit from the plan for that aspect";
-- both null is refused by tenant_overrides_not_empty (api-conventions.md 7.4).
CREATE TABLE IF NOT EXISTS tenant_entitlement_overrides (
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
