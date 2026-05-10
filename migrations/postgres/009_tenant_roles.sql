-- cleat tenant roles and admin schema migration
-- Creates per-tenant PostgreSQL LOGIN roles and tenant-owned schemas for plugin tables.
--
-- Idempotent: all statements use IF EXISTS / IF NOT EXISTS where applicable.

-- 1. Create admin schema for cleat-internal tables.
CREATE SCHEMA IF NOT EXISTS admin;

-- 2. Move tenants and tenant_api_keys from public to admin schema.
--    Order matters: tenants first (tenant_api_keys has FK to tenants).
ALTER TABLE IF EXISTS public.tenants SET SCHEMA admin;
ALTER TABLE IF EXISTS public.tenant_api_keys SET SCHEMA admin;

--    If the tables were somehow already in admin (or never created in public),
--    create them in admin so subsequent FK references are valid.
CREATE TABLE IF NOT EXISTS admin.tenants (
    tenant_id   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    suspended   BOOLEAN NOT NULL DEFAULT false
);

CREATE TABLE IF NOT EXISTS admin.tenant_api_keys (
    key_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES admin.tenants(tenant_id),
    key_hash    BYTEA NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

-- 3. Table to track plugin tables per plugin (for GRANT management).
CREATE TABLE IF NOT EXISTS admin.plugin_tables (
    plugin_name TEXT NOT NULL,
    table_name  TEXT NOT NULL,
    PRIMARY KEY (plugin_name, table_name)
);

-- 4. Table to store tenant role credentials.
CREATE TABLE IF NOT EXISTS admin.tenant_roles (
    tenant_id UUID PRIMARY KEY REFERENCES admin.tenants(tenant_id),
    role_name TEXT NOT NULL UNIQUE,
    password  TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 5. Function to create a tenant role and schema.
CREATE OR REPLACE FUNCTION admin.create_tenant_role(p_tenant_id UUID) RETURNS TEXT AS $$
DECLARE
    v_role_name TEXT;
    v_password TEXT;
BEGIN
    v_role_name := 'cleat_tenant_' || replace(p_tenant_id::text, '-', '_');
    v_password := encode(gen_random_bytes(32), 'hex');

    -- Create the login role (idempotent: skip if already exists).
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role_name) THEN
        EXECUTE format(
            'CREATE ROLE %I WITH LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT CONNECTION LIMIT 10',
            v_role_name, v_password
        );
    END IF;

    -- Create the tenant's schema, owned by the tenant role (idempotent).
    EXECUTE format('CREATE SCHEMA IF NOT EXISTS %I AUTHORIZATION %I',
        'tenant_' || replace(p_tenant_id::text, '-', '_'), v_role_name);

    -- Set search_path and tenant_id session variable.
    EXECUTE format('ALTER ROLE %I SET search_path = %L, public', v_role_name,
        'tenant_' || replace(p_tenant_id::text, '-', '_'));
    EXECUTE format('ALTER ROLE %I SET cleat.tenant_id = %L', v_role_name, p_tenant_id);

    -- Grant USAGE on public schema.
    EXECUTE format('GRANT USAGE ON SCHEMA public TO %I', v_role_name);
    EXECUTE format('GRANT USAGE ON SCHEMA admin TO %I', v_role_name);

    -- Grant DML on public core tables (RLS filters by tenant_id).
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_defs TO %I', v_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_instances TO %I', v_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.event_history TO %I', v_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_signals TO %I', v_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_schedules TO %I', v_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_promises TO %I', v_role_name);

    -- UPSERT credentials (idempotent: update password if role already existed).
    INSERT INTO admin.tenant_roles (tenant_id, role_name, password)
    VALUES (p_tenant_id, v_role_name, v_password)
    ON CONFLICT (tenant_id) DO UPDATE SET
        role_name = EXCLUDED.role_name,
        password  = EXCLUDED.password;

    RETURN v_role_name;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;

-- 6. Function to grant a plugin's tables to a tenant.
CREATE OR REPLACE FUNCTION admin.grant_plugin_to_tenant(p_plugin_name TEXT, p_tenant_id UUID) RETURNS void AS $$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
    v_table_name TEXT;
BEGIN
    SELECT role_name INTO v_role_name FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    IF v_role_name IS NULL THEN
        RAISE EXCEPTION 'no role for tenant %', p_tenant_id;
    END IF;

    v_schema_name := 'tenant_' || replace(p_tenant_id::text, '-', '_');

    FOR v_table_name IN
        SELECT t.table_name FROM admin.plugin_tables t WHERE t.plugin_name = p_plugin_name
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.%I TO %I',
            v_schema_name, v_table_name, v_role_name);
    END LOOP;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;

-- 7. Function to revoke a plugin from a tenant.
CREATE OR REPLACE FUNCTION admin.revoke_plugin_from_tenant(p_plugin_name TEXT, p_tenant_id UUID) RETURNS void AS $$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
    v_table_name TEXT;
BEGIN
    SELECT role_name INTO v_role_name FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    IF v_role_name IS NULL THEN
        RAISE EXCEPTION 'no role for tenant %', p_tenant_id;
    END IF;

    v_schema_name := 'tenant_' || replace(p_tenant_id::text, '-', '_');

    FOR v_table_name IN
        SELECT t.table_name FROM admin.plugin_tables t WHERE t.plugin_name = p_plugin_name
    LOOP
        EXECUTE format('REVOKE ALL ON %I.%I FROM %I',
            v_schema_name, v_table_name, v_role_name);
    END LOOP;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;

-- 8. Function to drop a tenant (schema + role + metadata).
CREATE OR REPLACE FUNCTION admin.drop_tenant(p_tenant_id UUID) RETURNS void AS $$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
BEGIN
    v_schema_name := 'tenant_' || replace(p_tenant_id::text, '-', '_');
    v_role_name := 'cleat_tenant_' || replace(p_tenant_id::text, '-', '_');

    EXECUTE format('DROP SCHEMA IF EXISTS %I CASCADE', v_schema_name);
    EXECUTE format('DROP ROLE IF EXISTS %I', v_role_name);

    DELETE FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    DELETE FROM admin.tenants WHERE tenant_id = p_tenant_id;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;

-- 9. Backfill: create roles for existing tenants.
--    This runs once during migration. New tenants get roles via create_tenant_role().
--    Reference admin.tenants because the table was moved (or created) in admin above.
DO $$
DECLARE
    t RECORD;
BEGIN
    FOR t IN SELECT tenant_id FROM admin.tenants LOOP
        IF NOT EXISTS (SELECT 1 FROM admin.tenant_roles WHERE tenant_id = t.tenant_id) THEN
            PERFORM admin.create_tenant_role(t.tenant_id);
        END IF;
    END LOOP;
END $$;
