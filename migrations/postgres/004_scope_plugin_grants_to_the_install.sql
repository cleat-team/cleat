-- cleat#2402. `grant_plugin_to_tenant` and `revoke_plugin_from_tenant` walked
-- `admin.plugin_tables` by plugin NAME alone:
--
--     SELECT t.schema_name, t.table_name FROM admin.plugin_tables t
--     WHERE t.plugin_name = p_plugin_name
--
-- with no schema predicate, inside a SECURITY DEFINER function. Migration 066
-- gave that table a `schema_name` column for a stated reason -- `admin` is not
-- moved by `--schema`, so ONE REGISTRY CAN HOLD ENTRIES FROM MORE THAN ONE
-- INSTALL -- and moved the PK to (plugin_name, schema_name, table_name). So a
-- call naming one install's plugin granted the tenant's role access to EVERY
-- install's tables of that name, and revoked from all of them.
--
-- THE SIGNATURE CHANGES, SO THE OLD DEFINITION MUST BE DROPPED FIRST.
-- `CREATE OR REPLACE` cannot change a parameter list: given a third parameter it
-- creates an OVERLOAD, and the two-argument version stays installed, stays
-- SECURITY DEFINER, and stays unscoped -- so the hole remains reachable through
-- the old signature while every call to the new one passes. That is not
-- hypothetical here; it is the whole of cleat#2402, and
-- docs/contributor/migrations.md documents it under "The trap: a signature
-- change is not a replacement" using these two functions as its example.
--
-- The `SET search_path = pg_catalog` pin is RE-STATED on both replacements. A
-- CREATE OR REPLACE without a SET clause silently drops the pin (migration 070's
-- own warning), and engine/admin_functions_pin_their_search_path_test.go exists
-- to catch exactly that -- this migration is run against it, not assumed.
--
-- WHY A PARAMETER AND NOT `current_schema()`. The function cannot derive the
-- install: 070 pins `search_path = pg_catalog`, so `current_schema()` inside the
-- body resolves to pg_catalog, and there is no `cleat.schema` GUC (only
-- `cleat.tenant_id`). Migration 069's `p_schema` is the precedent for passing it.
--
-- SCOPE, DELIBERATELY UNCHANGED: the registry's `tenant_scoped` flag does NOT
-- gate the walk here. The reported defect is the missing SCOPE predicate, and
-- widening the change to the tenant-scoped dimension would be a second,
-- unreported change riding on this one. If that dimension turns out to be wrong
-- it is wrong independently, and it wants its own evidence.

-- The DROPs come first and are not optional. `IF EXISTS` because a database
-- that never had the pinned form still has to reach the CREATE below.
DROP FUNCTION IF EXISTS admin.grant_plugin_to_tenant(text, uuid);
DROP FUNCTION IF EXISTS admin.revoke_plugin_from_tenant(text, uuid);

CREATE OR REPLACE FUNCTION admin.grant_plugin_to_tenant(p_plugin_name text, p_tenant_id uuid, p_schema_name text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
    AS $$
DECLARE
    v_role_name TEXT;
    v_table RECORD;
BEGIN
    SELECT role_name INTO v_role_name FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    IF v_role_name IS NULL THEN
        RAISE WARNING 'grant_plugin_to_tenant: no role for tenant % -- skipping (single-tenant mode)', p_tenant_id;
        RETURN;
    END IF;

    -- BOTH predicates matter and they are different questions.
    --   t.schema_name = p_schema_name  -- WHICH INSTALL. The fix.
    --   v_table.schema_name in the GRANT -- WHICH SCHEMA to name, which is the
    --   registry's value and not `'tenant_' || uuid` (cleat#2389, migration 066).
    FOR v_table IN
        SELECT t.schema_name, t.table_name
        FROM admin.plugin_tables t
        WHERE t.plugin_name = p_plugin_name
          AND t.schema_name = p_schema_name
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.%I TO %I',
            v_table.schema_name, v_table.table_name, v_role_name);
    END LOOP;
END;
$$;

CREATE OR REPLACE FUNCTION admin.revoke_plugin_from_tenant(p_plugin_name text, p_tenant_id uuid, p_schema_name text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
    AS $$
DECLARE
    v_role_name TEXT;
    v_table RECORD;
BEGIN
    SELECT role_name INTO v_role_name FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    IF v_role_name IS NULL THEN
        RAISE WARNING 'revoke_plugin_from_tenant: no role for tenant % -- skipping (single-tenant mode)', p_tenant_id;
        RETURN;
    END IF;

    FOR v_table IN
        SELECT t.schema_name, t.table_name
        FROM admin.plugin_tables t
        WHERE t.plugin_name = p_plugin_name
          AND t.schema_name = p_schema_name
    LOOP
        EXECUTE format('REVOKE ALL ON %I.%I FROM %I',
            v_table.schema_name, v_table.table_name, v_role_name);
    END LOOP;
END;
$$;

-- The baseline's REVOKE/GRANT were attached to the two-argument signature, so
-- they went with the DROP. The new signature needs the same treatment, or the
-- fix replaces an over-broad grant with a routine PUBLIC can execute.
REVOKE ALL ON FUNCTION admin.grant_plugin_to_tenant(text, uuid, text) FROM PUBLIC;
GRANT ALL ON FUNCTION admin.grant_plugin_to_tenant(text, uuid, text) TO cleat_app;
REVOKE ALL ON FUNCTION admin.revoke_plugin_from_tenant(text, uuid, text) FROM PUBLIC;
GRANT ALL ON FUNCTION admin.revoke_plugin_from_tenant(text, uuid, text) TO cleat_app;
