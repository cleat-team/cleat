-- cleat migration 066 (postgres): a dropped tenant's plugin rows go with it
--
-- admin.drop_tenant deleted from ten core relations and no plugin table, so a
-- dropped tenant's rows in kv_store and every other plugin table survived
-- indefinitely. cleat#1289.
--
-- MEASURED before this migration, on a database built from these migrations,
-- with a plugin declaring one TenantScoped table and two tenants seeded:
--
--   SEEDED  A=1 B=1
--   AFTER   probe_1289 A=1 B=1   admin.tenants A=0
--
-- Three numbers rather than one, and the two extra ones are the point. A=1
-- after is the bug. B=1 says the fix must not become "delete everything".
-- admin.tenants A=0 says the sweep genuinely RAN -- without it, a drop that
-- silently did nothing would produce the same surviving row and read as the
-- same bug. That is the shape cleat#1265 was wrong about for a third of its
-- tables: a table that was never seeded and a table that was correctly emptied
-- both count zero.
--
-- #1280 changed what the residue costs. A plugin table declared TenantScoped
-- now carries ENABLE + FORCE row-level security with a policy keyed on the
-- tenant, so after the drop those rows sit behind a policy naming a tenant
-- that no longer exists: unreadable as well as undeleted. The mechanism that
-- improved isolation is what made the residue harder to find.
--
-- WHY THE REGISTRY AND NOT A LIST. admin.plugin_tables has existed since
-- 001_schema.sql and RegisterPluginTables has existed to fill it, and nothing
-- ever called it -- its doc comment says "Called during plugin Init after
-- migrations run", describing a call that does not exist. Meanwhile
-- Migration.TenantScoped IS a maintained declaration of which plugin tables
-- hold tenant-owned rows, because a policy depends on it. This migration and
-- the RunMigrations change beside it join the two: the declaration that earns
-- a table its policy now also earns it a row in the registry, and this
-- function reads the registry. A plugin that declares TenantScoped gets
-- tenant deletion without anyone editing this file.
--
-- TWO NEW COLUMNS, and neither is optional:
--
--   tenant_scoped  -- admin.plugin_tables was built for GRANTs
--                     (admin.grant_plugin_to_tenant reads it), and a table
--                     registered for that purpose need not have a tenant_id
--                     column at all. DELETE ... WHERE tenant_id would fail on
--                     one. The flag says which rows this function may use.
--
--   schema_name    -- --schema (cleat#1287) puts plugin tables somewhere other
--                     than public. admin.plugin_tables lives in the admin
--                     schema, which --schema does not move, so one registry
--                     can hold entries from more than one install; and this
--                     function carries no search_path of its own (cleat#1363),
--                     so an unqualified name would resolve against whatever
--                     the CALLER had set. The name has to be qualified from
--                     the registry.
--
-- Extending the primary key follows from schema_name: (plugin_name,
-- table_name) would collide between two --schema installs sharing a database.
-- Safe to rewrite here because the table is empty in every deployment -- it
-- has never had a producer.

-- current_schema() rather than a literal 'public', and the schema guard in
-- migration/migrations_do_not_hardcode_the_schema_test.go caught the literal
-- on the first full run. The default is evaluated at INSERT time against the
-- inserter's search_path, which is the right answer for a --schema install and
-- is what a hand-written insert would mean. registerTenantScopedTables always
-- supplies the value explicitly, so this only covers inserts that do not.
ALTER TABLE admin.plugin_tables
    ADD COLUMN IF NOT EXISTS schema_name TEXT NOT NULL DEFAULT current_schema();
ALTER TABLE admin.plugin_tables
    ADD COLUMN IF NOT EXISTS tenant_scoped BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE admin.plugin_tables DROP CONSTRAINT IF EXISTS plugin_tables_pkey;
ALTER TABLE admin.plugin_tables
    ADD CONSTRAINT plugin_tables_pkey PRIMARY KEY (plugin_name, schema_name, table_name);

-- CREATE OR REPLACE, so this is 059's body with one loop added. Diff it
-- against 059 rather than reading it whole; everything else is unchanged.
CREATE OR REPLACE FUNCTION admin.drop_tenant(p_tenant_id UUID) RETURNS void AS $$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
    v_plugin_table RECORD;
    v_deleted BIGINT;
BEGIN
    IF p_tenant_id = '00000000-0000-0000-0000-000000000000' THEN
        RAISE EXCEPTION 'admin.drop_tenant: refusing to delete the default tenant (00000000-0000-0000-0000-000000000000) -- it is shared by every single-tenant deployment and by workflow_defs/plugin_defs, which are not tenant-owned data';
    END IF;

    -- See "RLS" above: makes every DELETE below correct regardless of
    -- whether the owning/executing role is a superuser.
    PERFORM set_config('cleat.tenant_id', p_tenant_id::text, true);

    v_schema_name := 'tenant_' || replace(p_tenant_id::text, '-', '_');
    v_role_name := 'cleat_tenant_' || replace(p_tenant_id::text, '-', '_');

    -- event_history has no FK/CASCADE back to workflow_instances (dropped
    -- deliberately by 003_procedures.sql); must be deleted explicitly or it
    -- is orphaned the instant the workflow_instances rows below are gone.
    DELETE FROM event_history WHERE tenant_id = p_tenant_id;

    -- Cascades workflow_signals, workflow_promises, concurrency_keys,
    -- workflow_update_requests via their real ON DELETE CASCADE FKs.
    DELETE FROM workflow_instances WHERE tenant_id = p_tenant_id;

    -- No FK relationship to workflow_instances at all; own tenant_id column.
    DELETE FROM workflow_schedules WHERE tenant_id = p_tenant_id;

    -- FK to workflow_defs (NO ACTION), not workflow_instances; own tenant_id
    -- column, so a straight tenant_id predicate is correct and does not
    -- touch workflow_defs itself.
    DELETE FROM workflow_tags WHERE tenant_id = p_tenant_id;
    DELETE FROM workflow_routing WHERE tenant_id = p_tenant_id;

    -- Not in Finding S3's original table list; added because it carries a
    -- tenant_id column (010) and its result/error_msg columns hold a
    -- workflow's actual output. See the comment block above.
    DELETE FROM idempotency_keys WHERE tenant_id = p_tenant_id;

    -- cleat#1289. Every table a plugin declared TenantScoped, which is the
    -- same declaration that gave it a row-level security policy, so the set
    -- is exactly the set of plugin tables holding rows owned by one tenant.
    --
    -- Here rather than in the enumerated list above because the set is not
    -- known when this migration is written: it is whatever plugins the
    -- deployment loaded. That is the whole point -- a plugin declaring
    -- TenantScoped gets tenant deletion without anyone editing this function.
    --
    -- Schema-qualified from the registry rather than left to search_path:
    -- --schema puts plugin tables somewhere other than public, this function
    -- carries no search_path of its own (cleat#1363), and an unqualified name
    -- here would resolve against the CALLER's search_path.
    --
    -- Before the role and schema drops below, so a failure here rolls the
    -- whole thing back while it still can. DROP ROLE is not undone by a
    -- rollback of the surrounding statement in any useful sense -- see the
    -- long note further down about what an abort part-way through costs.
    FOR v_plugin_table IN
        SELECT schema_name, table_name
        FROM admin.plugin_tables
        WHERE tenant_scoped
        ORDER BY schema_name, table_name
    LOOP
        -- A registry row can outlive its table: a plugin is removed from the
        -- build, or its migration is reversed, and nothing deletes the row.
        -- Without this guard the first such row makes admin.drop_tenant raise
        -- `relation "public.x" does not exist` and TENANT DELETION STOPS
        -- WORKING ENTIRELY, for every tenant, with a remedy no operator would
        -- guess. Measured: a stale row left by an earlier test aborted the
        -- drop on the very first run after this loop was added.
        --
        -- A warning rather than silence, because the other reading of a
        -- missing table is a registration that recorded the wrong schema --
        -- in which case rows DO survive, and the skip is the only evidence.
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

    -- cleat#1201. A definition is the tenant's PROGRAM -- the most sensitive
    -- artifact they hand cleat -- and it was the one thing this function kept.
    --
    -- Safe here and not earlier: workflow_instances carries an FK to
    -- workflow_defs (NO ACTION), so a definition cannot be deleted while an
    -- instance references it. The instances of this tenant are deleted at the
    -- top of this function, so by now there are none.
    --
    -- plugin_defs is deliberately NOT touched: it has no tenant_id column at
    -- all (PRIMARY KEY (name, version)) and is genuinely shared, which is the
    -- half of the old "shared registry" comment that was true.
    DELETE FROM workflow_defs WHERE tenant_id = p_tenant_id;

    -- Must precede the admin.tenants delete below: admin.tenant_api_keys'
    -- FK to admin.tenants has no ON DELETE clause (NO ACTION), so deleting
    -- admin.tenants first would fail with a foreign key violation for any
    -- tenant that had ever had a key issued.
    DELETE FROM admin.tenant_api_keys WHERE tenant_id = p_tenant_id;

    -- Plugin data and role/schema cleanup (pre-existing), plus a bug found
    -- verifying this migration end-to-end: admin.create_tenant_role grants
    -- the tenant role SELECT/INSERT/UPDATE/DELETE on workflow_defs,
    -- workflow_instances, event_history, workflow_signals,
    -- workflow_promises, and workflow_schedules (001_schema.sql), and
    -- admin.grant_plugin_to_tenant grants it privileges on the tenant's own
    -- plugin tables too. DROP ROLE refuses to drop a role that still holds
    -- privileges anywhere:
    --
    --   ERROR:  role "cleat_tenant_..." cannot be dropped because some
    --   objects depend on it
    --   DETAIL:  privileges for schema public
    --            privileges for table workflow_instances ...
    --
    -- which is not a warning-and-continue error inside a PL/pgSQL function
    -- with no exception handler -- it aborts the whole function, and
    -- because a single top-level CALL is one transaction, every DELETE
    -- above rolls back with it. So the original admin.drop_tenant, called
    -- against any tenant that had ever been through
    -- create_tenant_role/grant_plugin_to_tenant (i.e. any tenant onboarded
    -- through the normal path), would have deleted nothing at all and
    -- surfaced a DROP ROLE error instead of the silent-almost-no-op Finding
    -- S3 describes -- loud rather than silent, but still a full failure of
    -- the one thing this function exists to do. Verified directly: the
    -- unmodified EXECUTE format('DROP ROLE IF EXISTS %I', ...) below,
    -- called against a role created by admin.create_tenant_role, raises
    -- exactly the error above and every DELETE in this function is rolled
    -- back with it -- confirmed by counting workflow_instances /
    -- event_history for the tenant afterward and finding them still
    -- present. DROP OWNED BY strips every privilege grant (and would drop
    -- any objects the role owned, though this role owns none -- it only
    -- has GRANTs) so the subsequent DROP ROLE succeeds. Guarded on
    -- existence first: DROP OWNED BY has no IF EXISTS form, and the role
    -- may legitimately not exist (grant_plugin_to_tenant and
    -- create_tenant_role both warn-and-skip rather than fail in
    -- single-tenant mode, per their own comments in 001_schema.sql).
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role_name) THEN
        EXECUTE format('DROP OWNED BY %I', v_role_name);
    END IF;
    EXECUTE format('DROP SCHEMA IF EXISTS %I CASCADE', v_schema_name);
    EXECUTE format('DROP ROLE IF EXISTS %I', v_role_name);

    DELETE FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    DELETE FROM admin.tenants WHERE tenant_id = p_tenant_id;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;
