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

SET search_path = public;

CREATE OR REPLACE FUNCTION admin.grant_core_tables_to_tenant_role(p_role_name TEXT)
RETURNS void AS $$
BEGIN
    -- The six from 001, restated so this function is the single place the core
    -- grant set is expressed. create_tenant_role calls it below, and a
    -- re-grant is idempotent.
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_defs TO %I', p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_instances TO %I', p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.event_history TO %I', p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_signals TO %I', p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_schedules TO %I', p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_promises TO %I', p_role_name);

    -- The four this migration adds. Every one is on the per-workflow engine
    -- path, so a tenant connection cannot run a workflow without them.
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.idempotency_keys TO %I', p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.concurrency_keys TO %I', p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_update_requests TO %I', p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_tags TO %I', p_role_name);

    -- Read-only: a run reads its own limits and must not raise them.
    EXECUTE format('GRANT SELECT ON public.tenant_settings TO %I', p_role_name);

    -- Sequence usage for the tables above that have one. workflow_signals.id is
    -- a bigint identity; an INSERT without USAGE on its sequence fails with
    -- "permission denied for sequence", which is a different error from the
    -- table grant and easy to mistake for it.
    EXECUTE format('GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO %I', p_role_name);
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;

-- Re-grant for roles that already exist. A deployment that provisioned tenant
-- roles before this migration has them with the six-table grant set; this
-- brings them up to date without recreating the role or changing its password.
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

-- admin.create_tenant_role is redefined so NEW roles get the full set from one
-- place. 001 is still its only other definition (the audit in
-- IMPROVEMENT-PLAN-CLOSED.md records "later definition: none" for this
-- function), so this file is now authoritative -- per CLAUDE.md, find the
-- highest-numbered definition before concluding what it does.
--
-- Only the six inline table grants change: they become one PERFORM of the
-- helper above. Everything else -- the role name convention, the generated
-- password, the tenant_<uuid> schema, the search_path and cleat.tenant_id role
-- defaults, the admin.tenant_roles upsert -- is carried over verbatim so this
-- is a change to the grant set and to nothing else.
-- THE PASSWORD IS SUPPLIED, NOT GENERATED, AND NOT STORED. cleat#1307.
--
-- This function used to do `v_password := encode(gen_random_bytes(32), 'hex')`
-- and then INSERT it into admin.tenant_roles.password -- plaintext, in a table
-- with no row-level security. A database backup therefore contained every
-- tenant's login credential, which is to say the credential protecting tenant
-- isolation lived inside the database it was protecting.
--
-- The caller now derives it: HMAC-SHA256(worker key, tenant_id), in
-- plugin.TenantRolePassword. Deterministic, so any worker can open a tenant
-- pool without reading a stored secret, and there is no per-tenant secret at
-- rest to leak or to forget to delete.
--
-- The two-argument form REPLACES the one-argument form rather than overloading
-- it. An overload would leave the old signature working, and the old signature
-- cannot work: the column it wrote no longer exists. A caller that has not been
-- updated gets "function admin.create_tenant_role(uuid) does not exist", which
-- names the problem, instead of a role provisioned with a password nobody can
-- reproduce.
DROP FUNCTION IF EXISTS admin.create_tenant_role(UUID);

CREATE OR REPLACE FUNCTION admin.create_tenant_role(p_tenant_id UUID, p_password TEXT)
RETURNS TEXT AS $$
DECLARE
    v_role_name TEXT;
BEGIN
    IF p_password IS NULL OR length(p_password) < 32 THEN
        RAISE EXCEPTION 'create_tenant_role: password must be at least 32 characters '
            '(got %); derive it with plugin.TenantRolePassword', coalesce(length(p_password), 0);
    END IF;

    v_role_name := 'cleat_tenant_' || replace(p_tenant_id::text, '-', '_');

    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role_name) THEN
        BEGIN
            EXECUTE format(
                'CREATE ROLE %I WITH LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT CONNECTION LIMIT 10',
                v_role_name, p_password
            );
        EXCEPTION WHEN insufficient_privilege OR OTHERS THEN
            RAISE WARNING 'create_tenant_role: cannot create role % (SQLSTATE: %) -- skipping (single-tenant mode)', v_role_name, SQLSTATE;
            RETURN NULL;
        END;
    ELSE
        -- Idempotent, and this is the rotation path: a worker booting with a new
        -- key re-derives and re-ALTERs, so the role converges on the current
        -- key rather than needing to be dropped and recreated.
        BEGIN
            EXECUTE format('ALTER ROLE %I WITH PASSWORD %L', v_role_name, p_password);
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'create_tenant_role: cannot alter password for role % (SQLSTATE: %)', v_role_name, SQLSTATE;
        END;
    END IF;

    EXECUTE format('CREATE SCHEMA IF NOT EXISTS %I AUTHORIZATION %I',
        'tenant_' || replace(p_tenant_id::text, '-', '_'), v_role_name);

    EXECUTE format('ALTER ROLE %I SET search_path = %L, public', v_role_name,
        'tenant_' || replace(p_tenant_id::text, '-', '_'));
    EXECUTE format('ALTER ROLE %I SET cleat.tenant_id = %L', v_role_name, p_tenant_id);

    EXECUTE format('GRANT USAGE ON SCHEMA public TO %I', v_role_name);
    EXECUTE format('GRANT USAGE ON SCHEMA admin TO %I', v_role_name);

    PERFORM admin.grant_core_tables_to_tenant_role(v_role_name);

    -- role_name only. The password column is dropped below.
    INSERT INTO admin.tenant_roles (tenant_id, role_name)
    VALUES (p_tenant_id, v_role_name)
    ON CONFLICT (tenant_id) DO UPDATE SET role_name = EXCLUDED.role_name;

    RETURN v_role_name;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;

-- The stored credentials go. Dropping rather than nulling: a nullable column
-- that is always null is an invitation to write to it again, and the guard for
-- "nobody stores a tenant password" is easier to state as "the column does not
-- exist" than as "the column is empty".
--
-- SAFE TO DROP NOW, and this is the one moment when it is. plugin.TenantPools
-- was only ever constructed inside an unreachable branch (removed in #1350), so
-- no deployment has ever opened a tenant connection and no live traffic depends
-- on the stored passwords. Any role already provisioned keeps working: its
-- password is re-ALTERed to the derived value the next time a worker calls
-- create_tenant_role for it.
ALTER TABLE admin.tenant_roles DROP COLUMN IF EXISTS password;
