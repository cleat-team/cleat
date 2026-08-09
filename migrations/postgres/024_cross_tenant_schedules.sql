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
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        CREATE ROLE cleat_dispatcher NOLOGIN BYPASSRLS;
    ELSIF NOT (SELECT rolbypassrls FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        ALTER ROLE cleat_dispatcher BYPASSRLS;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO cleat_dispatcher;

-- SELECT only. The owner of a SECURITY DEFINER function needs its own table
-- privileges regardless of the RLS exemption -- those are two different checks,
-- and 023 shipped once without this GRANT and failed with "permission denied
-- for table" against a non-superuser role.
GRANT SELECT ON workflow_schedules TO cleat_dispatcher;

-- The column list is the contract with engine's scanDueSchedules. It matches
-- the tenant-scoped GetDueSchedules on every dialect, in order, so both feed
-- the same Go scan and cannot drift apart unnoticed.
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
SET search_path = public, pg_temp
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

ALTER FUNCTION admin.get_due_schedules() OWNER TO cleat_dispatcher;

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
