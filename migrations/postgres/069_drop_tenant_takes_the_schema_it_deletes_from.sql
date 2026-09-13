-- cleat migration 069 (postgres): admin.drop_tenant takes the schema it deletes from
--
-- admin.drop_tenant deleted from seven core tables by UNQUALIFIED name and
-- carried no search_path of its own, so every one of those DELETEs resolved
-- through the CALLER's search_path. cleat#1363.
--
-- WHAT WAS MEASURED, and it is enough on its own: the function resolved its
-- DELETEs through the caller's search_path, deleted a decoy table in another
-- schema, left the real rows in place, and REPORTED SUCCESS. An operator saw a
-- tenant dropped; the tenant's data was still there.
--
-- Not claimed here: any specific shadowing mechanism. The probe that would have
-- established one errored on its own fixture SQL before reaching the
-- interesting part, so it produced nothing either way -- UNMEASURED rather than
-- negative. The behaviour above is observed and does not depend on knowing how
-- a hostile search_path would be arranged.
--
-- THE FIX IS THE SIGNATURE, because the schema is a property of the CALLER.
-- --schema (cleat#1287) puts cleat's tables somewhere other than public, and
-- which schema that is is not knowable when this migration is written -- it is
-- chosen per install, at connection time. Resolving it at migration time, by
-- baking in current_schema() or a literal, answers a question nobody asked.
-- admin.drop_tenant(p_tenant_id, p_schema) asks the caller, which is where the
-- variation lives. It is the same shape migration 066's plugin loop already
-- uses: format('%I.%I', schema_name, table_name) out of admin.plugin_tables.
--
-- SET search_path = pg_catalog IS REQUIRED, NOT BELT-AND-BRACES, and the reason
-- is verifiability rather than defence in depth. With every name qualified
-- there is nothing left for it to affect -- which is exactly why it costs
-- nothing, and exactly why leaving it out is expensive: an unqualified name
-- that survives this rewrite then FAILS LOUDLY instead of silently resolving
-- through the caller. Without it, a DELETE I missed stays exploitable and looks
-- fine.
--
-- NO COMPATIBILITY OVERLOAD. The one-argument form is DROPped rather than kept
-- for a release. Multi-pool is a future feature, so there is no installed base
-- a shim would protect -- and the old arity is the vulnerable one, so carrying
-- it would mean shipping a deprecated path that still has the defect. A DEFAULT
-- on p_schema would be the same mistake wearing a different hat: the only
-- sensible default is current_schema(), which resolves through the caller's
-- search_path and is the bug.

-- Before the CREATE: adding a parameter makes an OVERLOAD, not a replacement,
-- so without this both arities would exist and the vulnerable one would remain
-- callable by anything that had not been updated.
DROP FUNCTION IF EXISTS admin.drop_tenant(UUID);

CREATE OR REPLACE FUNCTION admin.drop_tenant(p_tenant_id UUID, p_schema TEXT) RETURNS void AS $$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
    v_plugin_table RECORD;
    v_deleted BIGINT;
    v_core_table TEXT;
BEGIN
    IF p_schema IS NULL OR p_schema = '' THEN
        RAISE EXCEPTION 'admin.drop_tenant: p_schema is required -- it names the schema holding this deployment''s cleat tables, and must match the --schema cleat-worker was started with. Passing it is what stops the DELETEs resolving through the caller''s search_path (cleat#1363)';
    END IF;

    IF p_tenant_id = '00000000-0000-0000-0000-000000000000' THEN
        RAISE EXCEPTION 'admin.drop_tenant: refusing to delete the default tenant (00000000-0000-0000-0000-000000000000) -- it is shared by every single-tenant deployment and by workflow_defs/plugin_defs, which are not tenant-owned data';
    END IF;

    IF to_regnamespace(p_schema) IS NULL THEN
        RAISE EXCEPTION 'admin.drop_tenant: schema % does not exist. Deleting nothing and reporting success is the failure this function was fixed to stop, so it refuses instead', p_schema;
    END IF;

    -- Makes every DELETE below correct regardless of whether the owning or
    -- executing role is a superuser.
    PERFORM set_config('cleat.tenant_id', p_tenant_id::text, true);

    v_schema_name := 'tenant_' || replace(p_tenant_id::text, '-', '_');
    v_role_name := 'cleat_tenant_' || replace(p_tenant_id::text, '-', '_');

    -- The seven core tables, in dependency order, each schema-qualified from
    -- p_schema. The order is 066's and is load bearing:
    --
    --   event_history       no FK/CASCADE back to workflow_instances (dropped
    --                       deliberately by 003_procedures.sql), so it must go
    --                       first or it is orphaned.
    --   workflow_instances  cascades workflow_signals, workflow_promises,
    --                       concurrency_keys and workflow_update_requests via
    --                       real ON DELETE CASCADE FKs.
    --   workflow_schedules  no FK to workflow_instances; own tenant_id.
    --   workflow_tags       FK to workflow_defs (NO ACTION), own tenant_id.
    --   workflow_routing    as workflow_tags.
    --   idempotency_keys    own tenant_id (010); result/error_msg hold a
    --                       workflow's actual output.
    --   workflow_defs       LAST of the seven: workflow_instances carries an FK
    --                       to it (NO ACTION), so it cannot be deleted while an
    --                       instance references it. By here there are none.
    --
    -- plugin_defs is deliberately absent: it has no tenant_id column at all
    -- (PRIMARY KEY (name, version)) and is genuinely shared.
    FOREACH v_core_table IN ARRAY ARRAY[
        'event_history',
        'workflow_instances',
        'workflow_schedules',
        'workflow_tags',
        'workflow_routing',
        'idempotency_keys',
        'workflow_defs'
    ] LOOP
        IF to_regclass(format('%I.%I', p_schema, v_core_table)) IS NULL THEN
            RAISE EXCEPTION 'admin.drop_tenant: %.% does not exist. Either p_schema is wrong or this deployment is not fully migrated; either way, continuing would delete some of this tenant''s data and report success', p_schema, v_core_table;
        END IF;
        EXECUTE format('DELETE FROM %I.%I WHERE tenant_id = $1', p_schema, v_core_table)
            USING p_tenant_id;
        GET DIAGNOSTICS v_deleted = ROW_COUNT;
        RAISE DEBUG 'admin.drop_tenant: deleted % row(s) from %.%',
            v_deleted, p_schema, v_core_table;
    END LOOP;

    -- cleat#1289. Every table a plugin declared TenantScoped, which is the same
    -- declaration that gave it a row-level security policy. Schema-qualified
    -- from the registry, which is where a plugin table's schema is recorded --
    -- NOT from p_schema, because a plugin table need not live in the same
    -- schema as the core tables.
    FOR v_plugin_table IN
        SELECT schema_name, table_name
        FROM admin.plugin_tables
        WHERE tenant_scoped
        ORDER BY schema_name, table_name
    LOOP
        -- A registry row can outlive its table: a plugin is removed from the
        -- build, or its migration is reversed, and nothing deletes the row.
        -- Without this guard the first such row makes admin.drop_tenant raise
        -- and TENANT DELETION STOPS WORKING ENTIRELY, for every tenant.
        --
        -- A warning rather than silence, because the other reading of a missing
        -- table is a registration that recorded the wrong schema -- in which
        -- case rows DO survive, and the skip is the only evidence.
        IF to_regclass(format('%I.%I', v_plugin_table.schema_name,
                              v_plugin_table.table_name)) IS NULL THEN
            RAISE WARNING 'admin.drop_tenant: admin.plugin_tables names %.%, which does not exist -- skipping. If that table does exist under another schema, this tenant''s rows in it are NOT deleted.',
                v_plugin_table.schema_name, v_plugin_table.table_name;
            CONTINUE;
        END IF;

        EXECUTE format('DELETE FROM %I.%I WHERE tenant_id = $1',
                       v_plugin_table.schema_name, v_plugin_table.table_name)
            USING p_tenant_id;
        GET DIAGNOSTICS v_deleted = ROW_COUNT;
        RAISE DEBUG 'admin.drop_tenant: deleted % row(s) from %.%',
            v_deleted, v_plugin_table.schema_name, v_plugin_table.table_name;
    END LOOP;

    -- Must precede the admin.tenants delete: admin.tenant_api_keys' FK to
    -- admin.tenants has no ON DELETE clause (NO ACTION), so deleting
    -- admin.tenants first fails for any tenant that ever had a key issued.
    DELETE FROM admin.tenant_api_keys WHERE tenant_id = p_tenant_id;

    -- DROP ROLE refuses to drop a role that still holds privileges anywhere,
    -- and that error aborts the whole function -- rolling back every DELETE
    -- above with it, since a single CALL is one transaction. DROP OWNED BY
    -- strips the grants first. Guarded on existence because DROP OWNED BY has
    -- no IF EXISTS form and the role may legitimately not exist (single-tenant
    -- mode warns and skips role creation).
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role_name) THEN
        EXECUTE format('DROP OWNED BY %I', v_role_name);
    END IF;
    EXECUTE format('DROP SCHEMA IF EXISTS %I CASCADE', v_schema_name);
    EXECUTE format('DROP ROLE IF EXISTS %I', v_role_name);

    DELETE FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    DELETE FROM admin.tenants WHERE tenant_id = p_tenant_id;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog;

-- A NEW SIGNATURE IS A NEW FUNCTION, and PostgreSQL grants EXECUTE on a new
-- function to PUBLIC by default. 065 revoked that for the one-argument form and
-- added ALTER DEFAULT PRIVILEGES IN SCHEMA admin so later functions inherit the
-- revocation.
--
-- MEASURED, AND THE REVOKE BELOW IS REDUNDANT IN THE ORDINARY CASE. With this
-- line removed and the migrations applied by the usual runner,
-- has_function_privilege('public', 'admin.drop_tenant(uuid,text)', 'EXECUTE')
-- is already false -- 065's default privileges cover it, because this migration
-- runs as the same role that ran 065.
--
-- Kept anyway, and stated as redundant rather than sold as the fix, because
-- default privileges are recorded PER GRANTING ROLE -- 065's own comment says
-- so and adds that the test asserts the property rather than trusting the line.
-- A function created by a different role is not covered. One statement buys
-- independence from that, and no test in the tree distinguishes the two cases,
-- so nothing would report the difference if the assumption stopped holding.
REVOKE ALL ON FUNCTION admin.drop_tenant(UUID, TEXT) FROM PUBLIC;

-- cleat_app keeps EXECUTE, as 005_app_role.sql grants it and 065 preserves it.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_app') THEN
        EXECUTE 'GRANT EXECUTE ON FUNCTION admin.drop_tenant(UUID, TEXT) TO cleat_app';
    END IF;
END $$;
