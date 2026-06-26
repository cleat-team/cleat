-- cleat consolidated defaults (002)
-- Combines: 003_defaults, password sync from 011
--
-- Inserts the default tenant, backfills tenant_id on any legacy NULL rows,
-- creates tenant roles for all tenants, and resyncs stored passwords with
-- pg_roles to handle database drop/recreate scenarios.

-- ── Default tenant ───────────────────────────────────────────────────────────
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
DO $$
DECLARE
    r record;
BEGIN
    FOR r IN SELECT role_name, password FROM admin.tenant_roles
    LOOP
        BEGIN
            EXECUTE format('ALTER ROLE %I WITH PASSWORD %L', r.role_name, r.password);
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'password sync: cannot alter password for role % (SQLSTATE: %)', r.role_name, SQLSTATE;
        END;
    END LOOP;
END;
$$;
