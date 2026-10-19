-- +goose Up
-- data-model.md section 2.6. Authentication looks up by prefix, which is
-- unique and indexed, then verifies the Argon2id hash. The hash is
-- deliberately not indexed: indexing a verifier invites a timing oracle.
CREATE TABLE IF NOT EXISTS api_keys (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name          text NOT NULL,
  prefix        text NOT NULL,
  key_hash      bytea NOT NULL,
  scope         key_scope NOT NULL,
  state         key_state NOT NULL DEFAULT 'active',
  expires_at    timestamptz,
  last_used_at  timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  revoked_at    timestamptz,
  UNIQUE (prefix),
  CONSTRAINT api_keys_name_unique UNIQUE (name)
);
