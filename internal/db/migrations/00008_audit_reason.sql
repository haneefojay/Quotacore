-- +goose Up
-- data-model.md section 2.8, amended by IP-09. The reason a caller gave for
-- an archive, delete, suspend or resume, which every one of those routes
-- requires and which had nowhere to go. Nullable, because the other
-- mutations take no reason. Added last, so an upgraded database and a fresh
-- one have the same column order.
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS reason text;
