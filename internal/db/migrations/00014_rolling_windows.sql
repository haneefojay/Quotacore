-- +goose Up
-- data-model.md section 2.3, amended by IP-19. A rolling feature (DR-052,
-- ADR-0008) is counted against a limit over a sliding window, with no calendar
-- cycle, so its plan_entitlements row holds a limit and a window and no
-- interval. The window is stored as its length in seconds, and only the seven
-- the contract names are representable.
--
-- The three shapes are: nothing (boolean), a limit and an interval (metered),
-- and a limit and a window (rolling). Which shape belongs to which kind is a
-- rule across two tables and is enforced in internal/db, as IP-17's is.
ALTER TYPE feature_kind ADD VALUE IF NOT EXISTS 'rolling';
ALTER TABLE plan_entitlements ADD COLUMN rolling_window_seconds integer
  CHECK (rolling_window_seconds IN (300, 900, 3600, 21600, 86400, 604800, 2592000));
ALTER TABLE plan_entitlements DROP CONSTRAINT plan_entitlements_limit_pair;
ALTER TABLE plan_entitlements ADD CONSTRAINT plan_entitlements_shape
  CHECK ((limit_value IS NULL AND interval IS NULL AND rolling_window_seconds IS NULL)
      OR (limit_value IS NOT NULL AND interval IS NOT NULL AND rolling_window_seconds IS NULL)
      OR (limit_value IS NOT NULL AND interval IS NULL AND rolling_window_seconds IS NOT NULL));
