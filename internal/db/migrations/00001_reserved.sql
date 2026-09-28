-- +goose Up
-- This file creates no schema. The control-plane schema is IP-08's, and
-- data-model.md owns every table, index and constraint. The migration set is
-- not empty for one reason: the runner, the advisory lock, the checksum record
-- and the current-version check are IP-01's and are only provable against a
-- set that exists. This file applies cleanly, records a version, and asserts
-- nothing.
SELECT 1;
-- +goose Down
SELECT 1;
