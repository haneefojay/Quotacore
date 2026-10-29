-- +goose Up
-- data-model.md section 2.1, and override_apply_at from section 2.5. Lifecycle
-- states are real enums so an illegal value cannot be written even by a bug.
-- CREATE TYPE has no IF NOT EXISTS; the file runs in one transaction, so a
-- partial application cannot persist and a re-run is never half-done.
CREATE TYPE tenant_state      AS ENUM ('active', 'suspended', 'deleted');
CREATE TYPE archive_state     AS ENUM ('active', 'archived');
CREATE TYPE feature_kind      AS ENUM ('metered', 'boolean');
CREATE TYPE reset_interval    AS ENUM
  ('hourly', 'daily', 'weekly', 'monthly', 'yearly', 'never');
CREATE TYPE key_scope         AS ENUM ('runtime', 'admin');
CREATE TYPE key_state         AS ENUM ('active', 'revoked', 'expired');
CREATE TYPE event_source      AS ENUM ('api', 'admin', 'system');
CREATE TYPE event_type        AS ENUM (
  'consumed', 'refunded', 'granted', 'set',
  'plan_changed', 'cycle_rolled_over', 'force_rolled_over'
);
CREATE TYPE override_apply_at AS ENUM ('immediate', 'next_boundary');
