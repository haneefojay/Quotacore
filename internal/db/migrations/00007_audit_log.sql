-- +goose Up
-- data-model.md section 2.8. Written in the same transaction as the mutation
-- it records (NFR-D4, INV-X3) and never updated or deleted by the application
-- (DR-044). actor_key_id's ON DELETE SET NULL never fires, because no path
-- deletes an api_keys row: a key is revoked, which is a state change.
CREATE TABLE IF NOT EXISTS audit_log (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  actor_key_id uuid REFERENCES api_keys(id) ON DELETE SET NULL,
  actor        text NOT NULL,
  action       text NOT NULL,
  subject_type text NOT NULL,
  subject_id   uuid,
  before       jsonb,
  after        jsonb,
  request_id   text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT audit_log_subject_type
    CHECK (subject_type IN ('tenant', 'plan', 'feature', 'api_key'))
);

CREATE INDEX IF NOT EXISTS audit_log_subject_idx ON audit_log (subject_type, subject_id, created_at DESC);
CREATE INDEX IF NOT EXISTS audit_log_time_idx    ON audit_log (created_at DESC);
