-- A tenant login role could not touch eleven of the seventeen tenant-scoped
-- tables. cleat#1307.
--
-- admin.create_tenant_role (001_schema.sql) grants SELECT/INSERT/UPDATE/DELETE
-- on six tables: workflow_defs, workflow_instances, event_history,
-- workflow_signals, workflow_schedules, workflow_promises. The schema has grown
-- since, and the grant list did not:
--
--   SELECT table_name FROM information_schema.columns
--   WHERE table_schema='public' AND column_name='tenant_id';
--
-- returns seventeen tables. So a connection authenticating AS a tenant role --
-- which is the whole point of plugin.TenantPools, "the connection IS the
-- tenant" -- would fail on the first thing any workflow does: StartNewRun
-- writes an idempotency key.
--
-- WHY THIS IS SEPARATE FROM WIRING TenantPools UP. The pools were constructed
-- only inside an unreachable branch (removed in #1350), so nothing has ever run
-- on a tenant connection and this gap has never fired. Closing it first means
-- the wiring change is about wiring rather than about grants, and this
-- migration is safe on its own: it grants privileges to roles that, on a
-- single-tenant deployment, do not exist.
--
-- FOUR OF THE ELEVEN ARE DELIBERATELY WITHHELD, and the omission is the
-- interesting half of this file:
--
--   workflow_routing          resolved during start on the owner pool
--                             (engine/store_versioning.go), not per workflow
--   workflow_memory_samples   written by the worker's memory controller
--   workflow_memory_stats     (cmd/cleat-worker/memory_controller.go), which
--                             is worker-scoped and not on a tenant connection
--   tenant_settings           SELECT only, below -- a tenant must be able to
--                             READ its own limits (engine/run_limits.go) and
--                             must not be able to raise them
--                             (engine/store_tenant_settings_write.go)
--
-- If one of those judgements is wrong, a tenant connection fails with
-- "permission denied for table ..." -- loud, specific, and the correct
-- direction for a least-privilege mechanism. Granting them speculatively would
-- make the mechanism weaker in exchange for a quieter bug.
--
-- The other three tenant-scoped tables -- schedules, kv_store, feature_flags --
-- are PLUGIN-owned (plugins/scheduler, plugins/kvstore, plugins/featureflags)
-- and are admin.grant_plugin_to_tenant's job, per plugin per tenant. They are
-- correctly absent here.
--
-- PostgreSQL only, like 005_app_role.sql: login roles are the mechanism, and
-- MySQL is single-tenant by decision (tiers.yaml) while SQL Server uses session
-- context. There is no mysql/064 or mssql/064 and there should not be.

-- NO `SET search_path` HERE: the runner owns it (cleat#1287, enforced by
-- migration/TestMigrationsDoNotHardcodeTheSchema), and every table below is
-- addressed through current_schema() rather than a literal `public`.

CREATE OR REPLACE FUNCTION admin.grant_core_tables_to_tenant_role(p_role_name TEXT)
RETURNS void AS $$
BEGIN
    -- The six from 001, restated so the core grant set lives in one place.
    -- create_tenant_role calls this below; a re-grant is idempotent.
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_defs TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_instances TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.event_history TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_signals TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_schedules TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_promises TO %I', current_schema(), p_role_name);

    -- The four this migration adds. Every one is on the per-workflow engine
    -- path, so a tenant connection cannot run a workflow without them.
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.idempotency_keys TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.concurrency_keys TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_update_requests TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_tags TO %I', current_schema(), p_role_name);

    -- Read-only: a run reads its own limits and must not raise them.
    EXECUTE format('GRANT SELECT ON %I.tenant_settings TO %I', current_schema(), p_role_name);

    -- Sequence usage for the tables above that have one. workflow_signals.id is
    -- a bigint identity, and an INSERT without USAGE on its sequence fails with
    -- "permission denied for sequence" -- a different error from the table
    -- grant, and easy to mistake for it.
    EXECUTE format('GRANT USAGE ON ALL SEQUENCES IN SCHEMA %I TO %I', current_schema(), p_role_name);
END;
$$ LANGUAGE plpgsql SECURITY DEFINER
-- FROM CURRENT, for the reason 001 records at length on create_tenant_role: a
-- plpgsql function has no search_path of its own, so current_schema() in the
-- body would otherwise be evaluated with the CALLER's -- and this is called at
-- runtime by a connection that is not the migration connection. In the shipped
-- cluster that caller is role "cleat" against a database where a schema of that
-- name exists, so every GRANT above would address cleat.workflow_defs and fail.
-- Freezing the migration-time path here is what makes current_schema() mean the
-- schema these tables were created in.
SET search_path FROM CURRENT;

CREATE OR REPLACE FUNCTION admin.create_tenant_role(p_tenant_id UUID, p_password TEXT)
RETURNS TEXT AS $$
DECLARE
    v_role_name TEXT;
BEGIN
    IF p_password IS NULL OR length(p_password) < 32 THEN
        RAISE EXCEPTION 'create_tenant_role: password must be at least 32 '
            'characters (got %); derive it with plugin.TenantRolePassword',
            coalesce(length(p_password), 0);
    END IF;

    v_role_name := 'cleat_tenant_' || replace(p_tenant_id::text, '-', '_');

    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role_name) THEN
        BEGIN
    IF p_password IS NULL OR length(p_password) < 32 THEN
        RAISE EXCEPTION 'create_tenant_role: password must be at least 32 '
            'characters (got %); derive it with plugin.TenantRolePassword',
            coalesce(length(p_password), 0);
    END IF;

            EXECUTE format(
                'CREATE ROLE %I WITH LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT CONNECTION LIMIT 10',
                v_role_name, p_password
            );
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'create_tenant_role: cannot create role % (SQLSTATE: %) -- skipping (single-tenant mode)', v_role_name, SQLSTATE;
            RETURN NULL;
        END;
    ELSE
        BEGIN
    IF p_password IS NULL OR length(p_password) < 32 THEN
        RAISE EXCEPTION 'create_tenant_role: password must be at least 32 '
            'characters (got %); derive it with plugin.TenantRolePassword',
            coalesce(length(p_password), 0);
    END IF;

            EXECUTE format('ALTER ROLE %I WITH PASSWORD %L', v_role_name, p_password);
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'create_tenant_role: cannot alter password for role % (SQLSTATE: %)', v_role_name, SQLSTATE;
        END;
    END IF;

    EXECUTE format('CREATE SCHEMA IF NOT EXISTS %I AUTHORIZATION %I',
        'tenant_' || replace(p_tenant_id::text, '-', '_'), v_role_name);

    -- current_schema() rather than a literal: the schema cleat's tables live
    -- in is --schema's to choose (cleat#1287). Asking rather than stating is
    -- also what makes this work under every applier of these files, none of
    -- which substitutes anything.
    EXECUTE format('ALTER ROLE %I SET search_path = %L, %I', v_role_name,
        'tenant_' || replace(p_tenant_id::text, '-', '_'), current_schema());
    EXECUTE format('ALTER ROLE %I SET cleat.tenant_id = %L', v_role_name, p_tenant_id);

    EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', current_schema(), v_role_name);
    EXECUTE format('GRANT USAGE ON SCHEMA admin TO %I', v_role_name);

    PERFORM admin.grant_core_tables_to_tenant_role(v_role_name);

    -- role_name only; the password column is dropped at the end of this file.
    INSERT INTO admin.tenant_roles (tenant_id, role_name)
    VALUES (p_tenant_id, v_role_name)
    ON CONFLICT (tenant_id) DO UPDATE SET role_name = EXCLUDED.role_name;

    RETURN v_role_name;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER
-- FROM CURRENT freezes the migration-time search_path onto this function, and
-- it is what makes the current_schema() calls in the body mean what they say.
--
-- Without it a plpgsql function has no search_path of its own and resolves
-- names -- and evaluates current_schema() -- with the CALLER's. This function
-- is called at runtime, by a connection that is not the migration connection,
-- and in the shipped cluster that caller is role "cleat" against a database
-- where 001 has created a schema of that name. So current_schema() returns
-- "cleat" and the GRANTs above address cleat.workflow_defs, which does not
-- exist:
--
--   create_tenant_role(a): pq: relation "cleat.workflow_defs" does not exist
--
-- Measured in CI on cleat#1287's first attempt, where the body had just been
-- changed from the literal public.* to current_schema(). The literal did not
-- have this problem because it did not ask a question; asking one moved the
-- answer to call time, and this moves it back to creation time.
--
-- It is also the standard hardening for SECURITY DEFINER, which this function
-- has wanted since it was written: it creates roles and grants privileges.
SET search_path FROM CURRENT;

-- The stored credentials go. Dropping rather than nulling: a nullable column
-- that is always null invites a write, and "the column does not exist" is an
-- easier property to guard than "the column is empty".
--
-- SAFE TO DROP NOW, and this is the one moment when it is. plugin.TenantPools
-- was only ever constructed inside an unreachable branch (removed in #1350), so
-- no deployment has ever opened a tenant connection and no live traffic depends
-- on the stored passwords. Any role already provisioned keeps working: its
-- password is re-ALTERed to the derived value the next time a worker calls
-- create_tenant_role for it, which is also the rotation path.
ALTER TABLE admin.tenant_roles DROP COLUMN IF EXISTS password;

-- Re-grant for roles that already exist. A deployment that provisioned tenant
-- roles before this migration has them with the six-table set; this brings them
-- up to date without recreating the role or changing its password.
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN SELECT role_name FROM admin.tenant_roles LOOP
        BEGIN
            PERFORM admin.grant_core_tables_to_tenant_role(r.role_name);
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'could not re-grant core tables to %: % (SQLSTATE %)',
                r.role_name, SQLERRM, SQLSTATE;
        END;
    END LOOP;
END;
$$;
