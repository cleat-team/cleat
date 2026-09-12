-- cleat consolidated defaults (002)
-- Combines: 003_defaults, password sync from 011
--
-- Inserts the default tenant, backfills tenant_id on any legacy NULL rows,
-- creates tenant roles for all tenants, and resyncs stored passwords with
-- pg_roles to handle database drop/recreate scenarios.

-- ── Default tenant ───────────────────────────────────────────────────────────
-- Unqualified names below resolve through search_path, which migration.Runner
-- sets to the configured schema before applying this file (and which
-- deploy/postgres/100-apply-migrations.sh sets through PGOPTIONS on the psql
-- path). This file used to pin it to the literal `public` itself; see the
-- WithSchema comment in migration/runner.go for why that had to stop.

INSERT INTO admin.tenants (tenant_id, name, display_name)
    VALUES ('00000000-0000-0000-0000-000000000000', 'default', 'Default Tenant')
    ON CONFLICT (tenant_id) DO NOTHING;

-- ── Backfill tenant_id where NULL (safe to re-run) ──────────────────────────
UPDATE workflow_defs SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE workflow_instances SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE event_history SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE workflow_signals SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE workflow_schedules SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE concurrency_keys SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;

-- ── Create tenant roles for every tenant ────────────────────────────────────
DO $$
DECLARE
    t RECORD;
BEGIN
    FOR t IN SELECT tenant_id FROM admin.tenants LOOP
        IF NOT EXISTS (SELECT 1 FROM admin.tenant_roles WHERE tenant_id = t.tenant_id) THEN
            BEGIN
                -- The ONE-ARGUMENT form, which migration 064 drops: passwords
                -- are derived by the caller now, and 002 has no key to derive
                -- with. On a fresh database this still runs -- 002 executes
                -- long before 064 -- and on an existing one 002 never re-runs.
                --
                -- Re-applied against a post-064 database it raises
                -- "function admin.create_tenant_role(uuid) does not exist",
                -- which the handler below turns into a WARNING and a skip. That
                -- is the right outcome rather than a papered-over one:
                -- provisioning moves to the worker, which has the key, so a
                -- backfill here would have nothing to offer. The warning is
                -- expected on such a run and is not a fault (cleat#1307).
                PERFORM admin.create_tenant_role(t.tenant_id);
            EXCEPTION WHEN OTHERS THEN
                RAISE WARNING 'create_tenant_role failed for tenant % (SQLSTATE: %) -- skipping (single-tenant mode)', t.tenant_id, SQLSTATE;
            END;
        END IF;
    END LOOP;
END $$;

-- ── Password sync: align pg_roles with admin.tenant_roles ───────────────────
-- This fixes a mismatch that occurs when the database is dropped and recreated:
-- the roles persist at the server level with old passwords, but the migration
-- regenerates passwords and stores new ones in admin.tenant_roles.
--
-- GUARDED ON THE COLUMN EXISTING, because migration 064 drops it: tenant role
-- passwords are derived from the worker's key (plugin.TenantRolePassword)
-- rather than stored, so there is no longer a value to sync from.
--
-- The guard is not cosmetic. On a FRESH database this block still runs -- 002
-- executes long before 064 -- and on an existing one 002 is already recorded
-- and never re-runs. The case it exists for is anything that re-applies the
-- whole set against a database where 064 HAS run, which is what
-- engine/testutil's SetupFullSchema does on every test that asks for a
-- PostgreSQL schema. Without this, dropping the column turns one migration
-- into a failure in several hundred tests (cleat#1307).
--
-- Derivation makes the drift this block repairs self-healing anyway: a worker
-- re-derives from its key and re-ALTERs, so a dropped-and-recreated database
-- converges at the next boot rather than needing a stored value to converge
-- towards.
DO $$
DECLARE
    r record;
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'admin' AND table_name = 'tenant_roles'
          AND column_name = 'password'
    ) THEN
        RAISE NOTICE 'password sync: admin.tenant_roles.password is absent; passwords are '
            'derived (see migration 064) -- nothing to sync';
        RETURN;
    END IF;

    FOR r IN EXECUTE 'SELECT role_name, password FROM admin.tenant_roles'
    LOOP
        BEGIN
            EXECUTE format('ALTER ROLE %I WITH PASSWORD %L', r.role_name, r.password);
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'password sync: cannot alter password for role % (SQLSTATE: %)', r.role_name, SQLSTATE;
        END;
    END LOOP;
END;
$$;
