-- +goose Up
-- data-model.md section 2.3, amended by IP-17. A boolean entitlement is an
-- inclusion with no counter (DR-050), so its plan_entitlements row holds no
-- limit and no interval. The two are null together or not at all; that a null
-- pair belongs to a boolean feature, and a value pair to a metered one, is a
-- rule across two tables and is enforced in internal/db, where the feature's
-- kind is read in the same transaction.
ALTER TABLE plan_entitlements ALTER COLUMN limit_value DROP NOT NULL;
ALTER TABLE plan_entitlements ALTER COLUMN interval DROP NOT NULL;
ALTER TABLE plan_entitlements ADD CONSTRAINT plan_entitlements_limit_pair
  CHECK ((limit_value IS NULL) = (interval IS NULL));
