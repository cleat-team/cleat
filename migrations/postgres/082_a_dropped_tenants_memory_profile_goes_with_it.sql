-- cleat migration 082 (postgres): a dropped tenant's memory profile goes with it
--
-- cleat#1644. admin.drop_tenant deleted from a list of seven core tables and
-- named neither workflow_memory_stats nor workflow_memory_samples. Both have
-- carried tenant_id since 056 (cleat#1040), and neither has a foreign key to
-- anything, so nothing cascaded them either.
--
-- MEASURED before this migration, on a database built from these migrations,
-- two tenants seeded, in 066's own three-number shape:
--
--   SEEDED  stats A=1 B=1   samples A=1 B=1
--   AFTER   stats A=1 B=1   samples A=1 B=1   admin.tenants A=0 B=1
--
-- A=1 after is the bug. B=1 says the fix must not become "delete everything".
-- admin.tenants A=0 says the drop genuinely RAN -- without it, a drop that
-- silently did nothing produces the same surviving rows and reads as the same
-- bug.
--
-- WHAT SURVIVES IS NOT AN IMPLEMENTATION DETAIL. workflow_memory_samples is
-- keyed (tenant_id, def_name) and def_name is the tenant's own workflow name,
-- so what outlived the tenant was a list of the workflows it ran and how much
-- memory each used. That is the category cleat#1201 was filed about for
-- workflow_defs.
--
-- THE TWO NAMES ARE THE SMALL HALF OF THIS CHANGE. The list in this function is
-- hand-maintained, and so is dropTenantTables in cmd/cleatctl/droptenant.go,
-- and both have now drifted three times:
--
--   tenant_settings                    039   missing for "twenty migrations",
--                                            per droptenant.go's own comment
--   workflow_defs                      001   cleat#1201
--   workflow_memory_{stats,samples}    056   this
--
-- Migration 056 named the structural reason, about a different guard:
--
--     so it answers "is every statement against a KNOWN tenant-scoped table
--     scoped?" and cannot answer "is every table that should be tenant-scoped
--     actually one?"
--
-- This function has the same blind spot with the roles reversed: it deletes
-- from the tables it knows about, and a new tenant_id column is not in its
-- universe. So the durable half of cleat#1644 is a test, not these two lines --
-- engine/a_dropped_tenants_rows_all_go_with_it_test.go derives the universe
-- from information_schema.columns, requires a seed for every member, and
-- asserts every one is empty for the dropped tenant afterwards. A new
-- tenant-owned table fails it in the seed precondition, naming itself.
--
-- WHY NOT DERIVE THE LIST HERE, as migrations/mssql/074 does. That procedure
-- sweeps sys.columns and needs no list at all, and the asymmetry is
-- deliberate rather than an oversight:
--
--   * --schema (cleat#1287) means a PostgreSQL install's tables are not all in
--     one known schema, and per-tenant `tenant_<uuid>` schemas hold tables with
--     a tenant_id column belonging to OTHER tenants -- two exist on a test
--     database right now. A catalogue sweep here has to decide which schemas it
--     may touch, which is the question admin.plugin_tables already answers.
--   * This function works, is SECURITY DEFINER, and is the most destructive
--     routine in the schema. Rewriting how it chooses tables is a change with a
--     different risk profile from adding two names, and the test above closes
--     the drift class either way.
--
-- NO REVOKE/GRANT PAIR, unlike 069, and measured rather than assumed. 069 needed
-- one because it introduced a NEW SIGNATURE, and PostgreSQL grants EXECUTE on a
-- new function to PUBLIC. CREATE OR REPLACE on an existing signature keeps the
-- existing ACL, so the revocation 069 performed still stands. Checked either
-- side of applying this file:
--
--   before  postgres=X/postgres | cleat_app=X/postgres
--   after   postgres=X/postgres | cleat_app=X/postgres
--
-- PUBLIC absent both times. Re-derive with:
--
--   SELECT proacl FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
--   WHERE n.nspname = 'admin' AND p.proname = 'drop_tenant' AND pronargs = 2;
--
-- CREATE OR REPLACE, so this is 069's body with two entries added to one array.
-- Diff it against 069 rather than reading it whole; everything else is
-- unchanged, including the dependency ordering the array's comment explains and
-- the SECURITY DEFINER / search_path pinning from 070.

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
        -- cleat#1644. Both carry tenant_id since 056 and neither has a foreign
        -- key to anything, so nothing cascaded them either: a dropped tenant's
        -- memory profile -- the names of the workflows it ran and how much
        -- memory each used -- survived indefinitely. Position in this array is
        -- free for exactly that reason; they are here rather than at the end so
        -- that workflow_defs stays visibly last.
        'workflow_memory_samples',
        'workflow_memory_stats',
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
