-- +goose Up
-- data-model.md section 2.8, amended by IP-16 (Q-28). An audit row whose
-- subject is the installation itself rather than one resource: the note an
-- operator records after a Postgres restore that the history before it was
-- truncated (consistency-and-recovery.md section 4.2, step 6), written by
-- `quotacore audit note`. Its subject_id is null. The constraint is replaced
-- rather than altered, which Postgres has no syntax for.
ALTER TABLE audit_log DROP CONSTRAINT IF EXISTS audit_log_subject_type;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_subject_type
  CHECK (subject_type IN ('tenant', 'plan', 'feature', 'api_key', 'system'));
