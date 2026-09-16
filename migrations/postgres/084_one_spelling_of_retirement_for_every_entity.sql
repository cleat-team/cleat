-- cleat migration 084 (postgres): one spelling of retirement for every entity
--
-- cleat#1702, on the repository owner's decision of 2026-09-16. This is the
-- first of the conversion migrations: it adds the column to all ten members of
-- the non-workflow entity class. It does NOT remove the legacy spellings --
-- `revoked_at`, `deprecated` and `enabled` are still there and still the ones
-- the code reads. Those come out per-table, behind their own migrations, and
-- `workflow_schedules.enabled` carries an accepted API break that should not
-- ride along with nine tables that have no API consequence at all.
--
-- WHY `disabled_at TIMESTAMPTZ` AND NOT A BOOLEAN. The class had four spellings
-- of one concept and one of them was inverted: `revoked_at` (set = retired),
-- `deprecated BOOLEAN` (true = retired), `enabled BOOLEAN` (true = LIVE), and
-- seven members with no mechanism at all. A generic "is this live?" helper over
-- that set is right for three tables and backwards for one, and nothing in the
-- schema says which. A timestamp has no polarity to get backwards, it records
-- WHEN rather than only WHETHER, and it matches `revoked_at`, the one member
-- that already had the better shape.
--
-- NULLABLE, AND NULL MEANS LIVE. There is no "unknown" state: an entity either
-- has been retired at some instant or has not. That makes NULL the correct
-- spelling of the common case rather than a placeholder, and it is why there is
-- no DEFAULT and no NOT NULL.
--
-- NO BACKFILL, DELIBERATELY. Every existing row is live under the legacy
-- spelling or retired under it, and this migration does not read the legacy
-- column to decide. Copying `revoked_at` into `disabled_at` here would make the
-- two columns disagree the moment anything writes one and not the other, and
-- for the whole window in which both exist the legacy column is still the
-- authority. The conversion per table is where the value moves across, in the
-- same change that stops the code reading the old one.
--
-- WHY EVERY DIALECT GETS IT NOW even though scripts/check-entity-contract.py
-- only enforces clauses against Postgres: the guard is Postgres-only because
-- TIMESTAMPTZ is a Postgres spelling, not because the contract is. A column
-- that lands in one dialect and waits for another is the exact shape cleat#1719
-- was filed about.
--
-- Guarded with IF NOT EXISTS so this is safe against a database whose schema
-- engine/testutil built with the column already present.
-- ===========================================================================

ALTER TABLE admin.tenant_api_keys     ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;
ALTER TABLE admin.tenant_roles        ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;
ALTER TABLE admin.tenant_egress_allow ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;
ALTER TABLE workflow_defs             ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;
ALTER TABLE workflow_schedules        ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;
ALTER TABLE workflow_routing          ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;
ALTER TABLE workflow_tags             ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;
ALTER TABLE tenant_settings           ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;
ALTER TABLE tenant_secrets            ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;
ALTER TABLE tenant_domains            ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;

COMMENT ON COLUMN admin.tenant_api_keys.disabled_at IS
    'cleat#1702: when this entity was retired. NULL = live. The contract spelling; admin.tenant_api_keys.revoked_at is still the authority until its own conversion migration.';
COMMENT ON COLUMN admin.tenant_roles.disabled_at IS
    'cleat#1702: when this entity was retired. NULL = live.';
COMMENT ON COLUMN admin.tenant_egress_allow.disabled_at IS
    'cleat#1702: when this entity was retired. NULL = live.';
COMMENT ON COLUMN workflow_defs.disabled_at IS
    'cleat#1702: when this entity was retired. NULL = live. workflow_defs.deprecated is still the authority until its own conversion migration.';
COMMENT ON COLUMN workflow_schedules.disabled_at IS
    'cleat#1702: when this entity was retired. NULL = live. workflow_schedules.enabled is still the authority until its own conversion migration, which carries the accepted /api/schedules/{id}/enable and /disable break.';
COMMENT ON COLUMN workflow_routing.disabled_at IS
    'cleat#1702: when this entity was retired. NULL = live.';
COMMENT ON COLUMN workflow_tags.disabled_at IS
    'cleat#1702: when this entity was retired. NULL = live.';
COMMENT ON COLUMN tenant_settings.disabled_at IS
    'cleat#1702: when this entity was retired. NULL = live.';
COMMENT ON COLUMN tenant_secrets.disabled_at IS
    'cleat#1702: when this entity was retired. NULL = live.';
COMMENT ON COLUMN tenant_domains.disabled_at IS
    'cleat#1702: when this entity was retired. NULL = live.';
