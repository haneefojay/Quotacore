-- +goose Up
-- data-model.md sections 2.2 and 2.6, amended by IP-20.
--
-- A runtime key may be bound to one tenant (DR-053). The binding is the
-- stored row's, never the token's, as the scope is (ADR-0018), and only a
-- runtime key can carry one: an admin key that reached one tenant would still
-- reach the whole configuration. The column is indexed for the per-tenant
-- listing and for the soft delete that revokes a tenant's keys (INV-X7). A
-- tenant row is never removed, so the reference restricts.
ALTER TABLE api_keys ADD COLUMN tenant_id uuid REFERENCES tenants(id) ON DELETE RESTRICT;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_tenant_runtime_only
  CHECK (tenant_id IS NULL OR scope = 'runtime');
CREATE INDEX IF NOT EXISTS api_keys_tenant_id ON api_keys (tenant_id) WHERE tenant_id IS NOT NULL;

-- An allocation cap is a metered feature that counts entities (UC-10,
-- DR-054). It is fixed at create, as kind is, and only a metered feature can
-- be one; that its entitlements never reset is a rule across two tables and is
-- enforced in internal/db, as the kind's shape is.
ALTER TABLE features ADD COLUMN allocation_cap boolean NOT NULL DEFAULT false;
ALTER TABLE features ADD CONSTRAINT features_allocation_cap_metered
  CHECK (NOT allocation_cap OR kind = 'metered');
