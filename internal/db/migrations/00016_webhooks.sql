-- +goose Up
-- data-model.md section 2.9, added by IP-21 (DR-055, ADR-0019).
--
-- A tenant's one webhook: its endpoint and the subscription set, validated by
-- internal/admin and stored as the API renders it. The signing secret is not a
-- column. It is derived from QUOTACORE_WEBHOOK_SIGNING_KEY, the row's id and
-- secret_generation, so the database holds the generation and never the secret
-- (Q-40, NFR-S1). A tenant row is never removed, so the reference restricts;
-- a soft delete removes the webhook row in its own transaction.
CREATE TABLE IF NOT EXISTS webhooks (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL UNIQUE REFERENCES tenants(id) ON DELETE RESTRICT,
  url               text NOT NULL,
  subscriptions     jsonb NOT NULL,
  secret_generation bigint NOT NULL DEFAULT 1,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT webhooks_url_https CHECK (url LIKE 'https://%' AND char_length(url) <= 2048),
  CONSTRAINT webhooks_subscriptions_array CHECK (jsonb_typeof(subscriptions) = 'array'),
  CONSTRAINT webhooks_generation_positive CHECK (secret_generation >= 1)
);

-- A webhook change is audited with the webhook as its subject.
ALTER TABLE audit_log DROP CONSTRAINT IF EXISTS audit_log_subject_type;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_subject_type
  CHECK (subject_type IN ('tenant', 'plan', 'feature', 'api_key', 'system', 'webhook'));
