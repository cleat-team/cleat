-- 011_update_requests_tenant_id.sql:
--   1. Add tenant_id column to workflow_update_requests for tenant-scoped RLS.
--   2. Sync passwords for existing tenant roles that were created before the
--      create_tenant_role function was updated to ALTER ROLE on subsequent runs.

ALTER TABLE workflow_update_requests
    ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000';

-- Re-sync tenant role passwords so admin.tenant_roles matches pg_roles.
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
            RAISE WARNING '011_update_requests_tenant_id: cannot sync password for role % (SQLSTATE: %)', r.role_name, SQLSTATE;
        END;
    END LOOP;
END;
$$;
