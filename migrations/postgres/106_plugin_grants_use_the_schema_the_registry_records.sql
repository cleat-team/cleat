-- cleat migration 106 (postgres): the plugin grant/revoke functions use the
-- schema the registry records, not one derived from the tenant id.
--
-- cleat#2389. Both functions computed the schema as 'tenant_<uuid>' and ignored
-- admin.plugin_tables.schema_name -- the column migration 066 added for exactly
-- this. Measured against a plugin whose registry row records `public`:
--
--   ERROR:  relation "tenant_617aa860_cf41_4f27_85e9_aaa8aa478c59.tenant_quota"
--           does not exist
--   CONTEXT:  ... GRANT SELECT, INSERT, UPDATE, DELETE ON
--             tenant_617aa860_cf41_4f27_85e9_aaa8aa478c59.tenant_quota TO
--             cleat_tenant_617aa860_... PL/pgSQL function
--             admin.grant_plugin_to_tenant(text,uuid) line 18 at EXECUTE
--
-- `public` is the ordinary case, not an edge one: plugin migrations pin
-- search_path = public (plugin/migration.go), so a plugin table lands there
-- whatever the worker's --schema says. Against a registry that records that,
-- these functions raised for every plugin instead of granting anything.
--
-- WHY IT WAS INVISIBLE UNTIL NOW. The bodies were always wrong; they were
-- LATENT. Nothing populated the registry until cleat#2343 (which landed in
-- cleat#2367) made plugin registration real, so this loop was zero-iteration and
-- returned success without attempting anything -- the state the stale sentence
-- in docs/contributor/plugins/plugin-security.md describes. Reading the function
-- said it was probably wrong; only running it said which of an empty loop, a
-- tenant-schema row, and a public-schema row actually fails.
--
-- THE FIX IS THE COLUMN THAT WAS ALREADY THERE. registerTenantScopedTables'
-- own comment gives its purpose: the schema is recorded alongside the name
-- "because --schema puts plugin tables somewhere other than public while
-- admin.plugin_tables stays in the admin schema". admin.drop_tenant reads it.
-- These two did not.
--
-- NOT CHANGED HERE, deliberately. A registry row whose table no longer exists
-- still raises from these functions, where admin.drop_tenant guards the same
-- case with a WARNING and a CONTINUE (066). That guard arguably belongs here
-- too, but it is a separate behaviour change from the wrong-schema bug and is
-- not ridden in on this fix.
--
-- `SET search_path = pg_catalog` IS RESTATED ON PURPOSE, and this migration is
-- the case migration 070 predicted in writing: it pinned both functions with
-- ALTER FUNCTION and warned that "a later migration that does CREATE OR REPLACE
-- these without a SET clause will silently drop the pin". The bodies have to be
-- restated to fix them, so the clause is carried here rather than dropped, and
-- engine/admin_functions_pin_their_search_path_test.go reads pg_proc (not these
-- files) to check it survived.

CREATE OR REPLACE FUNCTION admin.grant_plugin_to_tenant(p_plugin_name TEXT, p_tenant_id UUID) RETURNS void AS $$
DECLARE
    v_role_name TEXT;
    v_table RECORD;
BEGIN
    SELECT role_name INTO v_role_name FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    IF v_role_name IS NULL THEN
        RAISE WARNING 'grant_plugin_to_tenant: no role for tenant % -- skipping (single-tenant mode)', p_tenant_id;
        RETURN;
    END IF;

    -- schema_name from the registry, not 'tenant_' || uuid. See the header.
    FOR v_table IN
        SELECT t.schema_name, t.table_name
        FROM admin.plugin_tables t
        WHERE t.plugin_name = p_plugin_name
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.%I TO %I',
            v_table.schema_name, v_table.table_name, v_role_name);
    END LOOP;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog;

CREATE OR REPLACE FUNCTION admin.revoke_plugin_from_tenant(p_plugin_name TEXT, p_tenant_id UUID) RETURNS void AS $$
DECLARE
    v_role_name TEXT;
    v_table RECORD;
BEGIN
    SELECT role_name INTO v_role_name FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    IF v_role_name IS NULL THEN
        RAISE WARNING 'revoke_plugin_from_tenant: no role for tenant % -- skipping (single-tenant mode)', p_tenant_id;
        RETURN;
    END IF;

    -- schema_name from the registry, not 'tenant_' || uuid. See the header.
    FOR v_table IN
        SELECT t.schema_name, t.table_name
        FROM admin.plugin_tables t
        WHERE t.plugin_name = p_plugin_name
    LOOP
        EXECUTE format('REVOKE ALL ON %I.%I FROM %I',
            v_table.schema_name, v_table.table_name, v_role_name);
    END LOOP;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog;
