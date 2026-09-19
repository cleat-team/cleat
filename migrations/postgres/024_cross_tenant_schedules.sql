-- ===========================================================================
-- 024: cross-tenant due-schedule read (admin.get_due_schedules)
--
-- Why this exists
-- ---------------
-- 023 gave the dispatch loop a way to claim runnable work for every tenant.
-- That fixed half of the problem it describes. This is the other half.
--
-- A non-default tenant's workflows now execute -- but only once something has
-- enqueued them, and for a cron schedule that something is the worker's own
-- schedule loop. That loop calls GetDueSchedules through the same single
-- tenant-scoped store, so it never sees a non-default tenant's schedule, and
-- nothing ever enqueues the run for 023 to claim. The schedule is stored,
-- listed in the dashboard, shows as enabled with a next_run_at, and never
-- fires. Recorded in engine.TestScheduleLoop_OnlySeesItsOwnTenantsSchedules.
--
-- So this is the same shape as 023 and exists for the same reason: one query
-- per tick regardless of tenant count, instead of one query per tenant.
--
-- Why this one only READS
-- -----------------------
-- 023's function claims: it reads and writes in a single statement, because a
-- claim that is not atomic hands the same workflow to two workers.
--
-- This one does not, and the difference is deliberate. Firing a schedule is
-- already a two-step operation in the worker -- see the loop in
-- cmd/cleat-worker: it reads the due set, starts a run, and only then calls
-- ClaimDueSchedule, a compare-and-swap on (next_run_at) that is what actually
-- makes delivery at-least-once. Moving the advance in here would duplicate
-- that CAS in a second place and give it a second answer.
--
-- So the exemption granted here is strictly narrower than 023's: SELECT on one
-- table. The advance still happens through the caller's own tenant-scoped
-- store, under RLS, exactly as it does today. The worker re-scopes to
-- Schedule.TenantID before ClaimDueSchedule and before StartNewRun.
--
-- No FOR UPDATE SKIP LOCKED, and that is a deliberate divergence
-- ------------------------------------------------------------
-- The tenant-scoped GetDueSchedules takes FOR UPDATE SKIP LOCKED. This does
-- not, because in PostgreSQL FOR UPDATE requires UPDATE privilege on the
-- table, and granting that to the role holding the RLS exemption would widen
-- it from "may read every tenant's schedules" to "may write them" for the sake
-- of an optimisation.
--
-- It is only an optimisation. Those row locks are released when the reading
-- transaction commits, which is before the worker does anything with the rows
-- -- what actually prevents a double firing is ClaimDueSchedule's
-- compare-and-swap on next_run_at, and that is unchanged and still tenant
-- scoped. Without the lock, two workers reaching the same instant both try and
-- exactly one wins the CAS; the loser logs and moves on. Same outcome, one
-- wasted round trip, and a grant that stays SELECT.
--
-- Everything in 023's header about WHY a BYPASSRLS role rather than plain
-- SECURITY DEFINER applies here unchanged, and is not repeated. The role it
-- created, cleat_dispatcher, is reused rather than joined by a second one: two
-- roles holding the same exemption is two things to audit instead of one.
-- ===========================================================================

-- 023 creates cleat_dispatcher. Guarded anyway: a database that somehow has
-- 024 without 023 should get a role, not a cryptic "role does not exist".
-- Degrades exactly as 023 does, and for the same reason; see the long note
-- there. On managed PostgreSQL the role cannot be created, the due-schedule
-- function is still created owned by the migrating role, and
-- CheckCrossTenantCapability reports it as unavailable with the owner named.
--
-- UNLIKE THE CLAIM, THERE IS NO GRANT-FREE ALTERNATIVE FOR THIS ONE YET.
-- --claim-strategy=rotate replaces 023's function; nothing yet replaces this
-- one, so a non-default tenant's cron does not fire on managed PostgreSQL.
-- The worker says so at startup rather than leaving it to be noticed.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        BEGIN
            CREATE ROLE cleat_dispatcher NOLOGIN BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'cleat_dispatcher needs BYPASSRLS, which only a superuser can grant, and this connection is not one. A non-default tenant''s cron will not fire on this deployment. (SQLSTATE %)', SQLSTATE;
        END;
    ELSIF NOT (SELECT rolbypassrls FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        BEGIN
            ALTER ROLE cleat_dispatcher BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'cleat_dispatcher exists without BYPASSRLS and this connection cannot grant it. (SQLSTATE %)', SQLSTATE;
        END;
    END IF;
END
$$;

DO $do$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        EXECUTE format('GRANT USAGE ON SCHEMA %I TO cleat_dispatcher', current_schema());
    END IF;
END $do$;

-- SELECT only. The owner of a SECURITY DEFINER function needs its own table
-- privileges regardless of the RLS exemption -- those are two different checks,
-- and 023 shipped once without this GRANT and failed with "permission denied
-- for table" against a non-superuser role.
DO $do$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        EXECUTE 'GRANT SELECT ON workflow_schedules TO cleat_dispatcher';
    END IF;
END $do$;

-- The column list is the contract with engine's scanDueSchedules. It matches
-- the tenant-scoped GetDueSchedules on every dialect, in order, so both feed
-- the same Go scan and cannot drift apart unnoticed.
--
-- CONDITIONAL SINCE cleat#1702, AND THE DEFINITION BELOW IS UNCHANGED. Migration
-- 089 converts workflow_schedules.enabled to disabled_at, drops the column, and
-- replaces this function with one whose RETURNS TABLE says `disabled_at
-- timestamptz` instead of `enabled boolean`.
--
-- Re-applying THIS file to a database that has been through 089 then fails two
-- ways over, and the docs promise every migration is re-appliable
-- (TestShippedSchema_IsIdempotent enforces it):
--
--   pq: cannot change return type of existing function (42P13)
--       -- CREATE OR REPLACE cannot change a signature, in either direction
--   and, past that, the body names s.enabled, a column 089 dropped
--
-- So the creation is guarded on the column its body needs. On a fresh database
-- 001_schema.sql declares `enabled`, this runs, and 089 replaces it four
-- migrations later; on a converted database this is a no-op and 089's
-- definition stands. The GRANT, the OWNER and the REVOKE below are left
-- unconditional -- they are correct for whichever definition is in place.
--
-- The definition is quoted rather than rewritten on purpose. Editing its body
-- to name disabled_at was tried first and made it byte-identical to 089's,
-- which engine/routine_definition_drift_test.go rejects: two identical
-- definitions in the tree leave it no way to tell a current database from one
-- stuck here.
DO $guard$
BEGIN
    -- current_schema(), not an unqualified lookup. information_schema.columns
    -- lists EVERY schema, and cleat supports schema-per-pool: a database can
    -- hold a converted pool_a alongside a fresh pool_b, so an unqualified
    -- `table_name = 'workflow_schedules'` answers about whichever pool happens
    -- to match and this block then edits a different one. Measured --
    -- migration/an_extension_is_per_database_not_per_schema_test.go migrates two
    -- schemas in one database and is the test that caught it.
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'workflow_schedules'
          AND column_name = 'enabled'
    ) THEN
        -- DROP before CREATE, because admin.get_due_schedules() is shared
        -- across schemas while workflow_schedules is per-schema. In a
        -- multi-schema database the function may already be the post-089 one
        -- installed while migrating ANOTHER pool, and CREATE OR REPLACE cannot
        -- change a return type in either direction (42P13). Dropping restores
        -- the pre-existing last-writer-wins behaviour -- the function's body is
        -- pinned to the migrating schema by SET search_path FROM CURRENT, so it
        -- was always the last pool migrated that the shared function served --
        -- rather than turning that into a hard failure at 024.
        DROP FUNCTION IF EXISTS admin.get_due_schedules();
        EXECUTE $fn$
CREATE OR REPLACE FUNCTION admin.get_due_schedules()
RETURNS TABLE (
    name            text,
    def_name        text,
    entry_point     text,
    cron_expression text,
    input           jsonb,
    enabled         boolean,
    next_run_at     timestamptz,
    last_run_at     timestamptz,
    timezone        text,
    tenant_id       uuid,
    misfire_policy  text,
    catch_up_limit  integer,
    overlap_policy  text,
    last_run_id     text
)
LANGUAGE sql
SECURITY DEFINER
-- Pinned so the body cannot be redirected by a caller's search_path. Standard
-- hardening for SECURITY DEFINER, and not optional when the function holds an
-- RLS exemption.
SET search_path FROM CURRENT
AS $$
    SELECT s.name, s.def_name, s.entry_point, s.cron_expression, s.input,
           s.enabled, s.next_run_at, s.last_run_at, s.timezone, s.tenant_id,
           s.misfire_policy, s.catch_up_limit, s.overlap_policy,
           COALESCE(s.last_run_id, '')
    FROM workflow_schedules s
    WHERE s.enabled = true
      AND s.next_run_at <= now()
    ORDER BY s.next_run_at;
$$;
$fn$;
    END IF;
END
$guard$;

DO $do$ BEGIN
    -- ATTEMPTED, NOT GUARDED ON THE ROLE'S EXISTENCE. "Does cleat_dispatcher
    -- exist" is the wrong question: ALTER ... OWNER TO also requires the
    -- current role to be a MEMBER of the target, so a role that exists but
    -- was created by somebody else still fails --
    --
    --   ERROR:  must be able to SET ROLE "cleat_dispatcher"   (SQLSTATE 42501)
    --
    -- and when the role does not exist at all the refusal is a DIFFERENT
    -- SQLSTATE -- undefined_object, 42704, not 42501 -- so both are caught.
    -- Catching only the first passed every run against a cluster where some
    -- earlier superuser run had left the role behind, and failed the first
    -- genuinely clean one.
    --
    -- which is what re-applying this file as a non-superuser hit, against a
    -- cluster where a superuser had created the role earlier. Asking the
    -- database to do it and catching the refusal answers both questions at
    -- once, and needs no version-specific reasoning about pg_has_role and
    -- PostgreSQL 16's WITH SET.
    BEGIN
        EXECUTE 'ALTER FUNCTION admin.get_due_schedules() OWNER TO cleat_dispatcher';
    EXCEPTION WHEN insufficient_privilege OR undefined_object THEN
        RAISE NOTICE 'cannot give the function to cleat_dispatcher (SQLSTATE %); it keeps the migrating role as its owner and will not see across tenants. Use --claim-strategy=rotate, which needs no exemption.', SQLSTATE;
    END;
END $do$;

-- REVOKE first: PostgreSQL grants EXECUTE to PUBLIC on new functions by
-- default, which would hand the exemption to every role in the database.
REVOKE ALL ON FUNCTION admin.get_due_schedules() FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_app') THEN
        GRANT EXECUTE ON FUNCTION admin.get_due_schedules() TO cleat_app;
    END IF;
END
$$;
