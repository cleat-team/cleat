-- cleat consolidated defaults (002)
--
-- The data and behaviour the schema needs on top of 001's structure. A catalog
-- dump carries no rows, so unlike 001 and 003 -- which are generated from a
-- pg_dump of the fully-migrated database -- this file is hand-assembled from
-- the two migrations that carried top-level DML: the old 002_defaults.sql and
-- 091, which introduced admin.orgs and seeded the default.
--
-- What is here, and why each part is still needed on a FRESH database -- the
-- only kind 0.3.0 supports, since the rebaseline is deliberately not
-- upgradeable in place:
--
--   * the default org and the default tenant: the two zero-UUID rows every
--     other table's tenant_id eventually refers to;
--   * a tenant_roles row per tenant;
--   * tenant_id backfills, which are no-ops on a fresh database.
--
-- What is NOT here, and it is the rebaseline's main simplification: the
-- password sync that used to close this file. It ALTERed each tenant role's
-- server-level password from a copy stored in admin.tenant_roles.password -- a
-- column migration 064 dropped -- so under the final schema it could only fall
-- through its own guard and RAISE NOTICE every time. It existed to protect
-- engine/testutil's SetupFullSchema, which re-applies the whole set, and it has
-- been inert in every tree since 064 landed. Roles and their passwords belong
-- to the worker now, which derives from its key.
--
-- Unqualified names below resolve through search_path, which migration.Runner
-- sets to the configured schema before applying this file. `admin.` is not the
-- configured schema and stays qualified.

-- ── Default org ─────────────────────────────────────────────────────────────
-- Before the tenant, because admin.tenants carries an FK to admin.orgs
-- (tenants_org_id_fkey): seeding the tenant first fails with 23503. That
-- dependency is invisible to the catalog diff -- it is a row-level constraint
-- and the diff compares structure -- and it surfaced only when the migration
-- actually ran.
INSERT INTO admin.orgs (org_id, name)
    VALUES ('00000000-0000-0000-0000-000000000000', 'default')
    ON CONFLICT (org_id) DO NOTHING;

-- ── Default tenant ──────────────────────────────────────────────────────────
INSERT INTO admin.tenants (tenant_id, name, display_name)
    VALUES ('00000000-0000-0000-0000-000000000000', 'default', 'Default Tenant')
    ON CONFLICT (tenant_id) DO NOTHING;

-- ── tenant_id backfills: REMOVED by the cleat#2059 rebaseline ───────────────
-- This file used to set tenant_id to the default on six tables where it was
-- NULL, for databases that predated the column. Two independent reasons it is
-- gone rather than ported:
--
--   1. It cannot do anything. 0.3.0 requires a fresh database, and on a fresh
--      database every one of those tables is empty -- the statements were
--      already no-ops. There is no 0.3.0 deployment in which they have a row to
--      touch, because the rebaseline is deliberately not upgradeable in place.
--
--   2. It does HARM, and that is how it was found. This consolidation installs
--      every RLS policy in 001, so by the time 002 runs the policies are already
--      in force -- whereas in the old chain the policies arrived in 031/061/083,
--      long AFTER 002. An UPDATE over an RLS table with no tenant in context now
--      raises
--
--        pq: cleat.tenant_id is not set -- tenant context required for RLS-scoped
--        query (P0001)
--
--      from cleat.assert_tenant_set(), and TestTheMigrationSetAppliesWithoutASuperuser
--      fails on a deployment that applies the set as a non-superuser -- the
--      configuration the whole cleat_app role exists to make possible.
--
-- The lesson generalises past this block: consolidating migrations re-orders
-- them, and a statement that was harmless where it used to sit can be a failure
-- where it lands. Anything in this file that touches a table runs AFTER every
-- policy 001 installs.

-- ── A tenant_roles row per tenant ───────────────────────────────────────────
-- This used to call admin.create_tenant_role(tenant_id) -- the ONE-ARGUMENT
-- form, which 064 DROPS in favour of the two-argument one taking the password
-- the worker derives. That call worked in the old chain only because this file
-- ran before 064, so the one-arg form was still installed; here 001 already
-- carries 064's FINAL schema, the function does not exist, the call raised, and
-- the EXCEPTION handler turned a missing seed into a WARNING nobody reads. The
-- row silently never appeared, and the catalog diff could not see it: a dropped
-- seed makes a catalog comparison EASIER to pass.
--
-- Inserting the row directly also drops a dependency that should not exist.
-- role_name is a pure function of tenant_id -- 064's own INSERT uses exactly
-- this expression -- and it needs no password, which is the thing this file
-- never had.
--
-- The DATABASE ROLE is not created here. Provisioning moved to the worker,
-- which has the key to derive the password from. The default tenant's role is
-- the one exception, and 001 creates it, because 001's own GRANTs name it. The
-- visible consequence: after migrations alone a deployment has no tenant login
-- role until a worker boots.
DO $$
DECLARE
    t RECORD;
BEGIN
    FOR t IN SELECT tenant_id FROM admin.tenants LOOP
        INSERT INTO admin.tenant_roles (tenant_id, role_name)
        VALUES (t.tenant_id, 'cleat_tenant_' || replace(t.tenant_id::text, '-', '_'))
        ON CONFLICT (tenant_id) DO NOTHING;
    END LOOP;
END $$;
