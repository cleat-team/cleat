-- ===========================================================================
-- 040: the cross-tenant claim picks up defer phases too
--
-- IMPROVEMENT-PLAN 3.75 step 2, decision D6. A workflow whose terminal outcome
-- has been decided but whose cleanup has not run sits in 'terminating' carrying
-- the outcome in pending_terminal_status (migration 038), and is dispatched
-- like any other workflow so that its defers run on a live instance.
--
-- The tenant-scoped claim is inline SQL in engine/store_lifecycle.go and moved
-- with that change. This one is a function, so it moves here. A deployment that
-- applied 038 and not this file would have a cross-tenant dispatch loop that
-- never claims a defer phase -- every terminate on it would wait out
-- defer_phase_deadline and be finalized by ExpireDeferPhases with its cleanup
-- skipped, silently, on the deployments most likely to have long-running
-- workflows holding locks.
--
-- Two changes, both required for that:
--
--   * the candidate predicate accepts 'terminating' alongside 'ready';
--   * the returned row carries pending_terminal_status, which is what tells
--     the executor the claim it just took is a defer segment rather than
--     ordinary work. The status column cannot say it: the claim sets
--     status = 'running' and RETURNING gives the new value.
--
-- DROP and CREATE rather than CREATE OR REPLACE. Adding a column to RETURNS
-- TABLE changes the function's return type, and PostgreSQL refuses that with
-- "cannot change return type of existing function" -- so a replace would fail
-- the migration rather than silently keeping the old shape. The owner, revoke
-- and grant have to be re-applied because they do not survive the drop; they
-- are copied from 023 deliberately unchanged, and 023 remains the file that
-- explains why the exemption is shaped this way.
--
-- Idempotent: DROP ... IF EXISTS, and every statement below is the same on a
-- second application.
-- ===========================================================================

DROP FUNCTION IF EXISTS admin.claim_workflows(text, text[], integer);

CREATE FUNCTION admin.claim_workflows(
    p_worker_id   text,
    p_task_queues text[],
    p_limit       integer
)
RETURNS TABLE (
    id                      text,
    def_name                text,
    def_version             integer,
    status                  text,
    input                   jsonb,
    assigned_to             text,
    next_wake_at            timestamptz,
    tenant_id               uuid,
    created_at              timestamptz,
    error_code              text,
    error_op                text,
    generation              bigint,
    priority                integer,
    trace_id                text,
    pending_terminal_status text
)
LANGUAGE sql
SECURITY DEFINER
-- Pinned so the body cannot be redirected by a caller's search_path. Standard
-- hardening for SECURITY DEFINER, and not optional when the function holds an
-- RLS exemption.
SET search_path FROM CURRENT
AS $$
    WITH candidates AS (
        SELECT w.id FROM workflow_instances w
        WHERE w.status IN ('ready', 'terminating')
          AND w.next_wake_at <= now()
          AND w.task_queue = ANY(p_task_queues)
        ORDER BY w.priority ASC, w.created_at
        LIMIT p_limit
        FOR UPDATE SKIP LOCKED
    )
    UPDATE workflow_instances w
    SET status = 'running',
        assigned_to = p_worker_id,
        heartbeat_at = now(),
        generation = w.generation + 1
    FROM candidates c
    WHERE w.id = c.id
    RETURNING w.id, w.def_name, w.def_version, w.status, w.input, w.assigned_to,
              w.next_wake_at, w.tenant_id, w.created_at, w.error_code, w.error_op,
              w.generation, COALESCE(w.priority, 0), COALESCE(w.trace_id, ''),
              COALESCE(w.pending_terminal_status, '');
$$;

-- Conditional for the reason 023 gives at length: on managed PostgreSQL the
-- role cannot be created, and this statement aborting the run was how a
-- degraded 023 still took the whole migration set down with it.
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
        EXECUTE 'ALTER FUNCTION admin.claim_workflows(text, text[], integer) OWNER TO cleat_dispatcher';
    EXCEPTION WHEN insufficient_privilege OR undefined_object THEN
        RAISE NOTICE 'cannot give the function to cleat_dispatcher (SQLSTATE %); it keeps the migrating role as its owner and will not see across tenants. Use --claim-strategy=rotate, which needs no exemption.', SQLSTATE;
    END;
END $do$;

-- REVOKE first: PostgreSQL grants EXECUTE to PUBLIC on new functions by
-- default, which would hand the exemption to every role in the database.
REVOKE ALL ON FUNCTION admin.claim_workflows(text, text[], integer) FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_app') THEN
        GRANT EXECUTE ON FUNCTION admin.claim_workflows(text, text[], integer) TO cleat_app;
    END IF;
END
$$;

-- ===========================================================================
-- The claim's indexes, widened to match the claim.
--
-- This is the half of the change that is invisible until it is a production
-- incident. EVERY index PostgreSQL has for finding runnable work is PARTIAL on
-- `status = 'ready'` (001_schema.sql:424, 448, 451, 456). A predicate of
-- `status IN ('ready', 'terminating')` does not imply `status = 'ready'`, so
-- the planner may use none of them -- on the single hottest query in the
-- system, the dispatch loop's claim.
--
-- Widening the PREDICATE rather than adding a second partial index per pair is
-- deliberate, and it was measured rather than reasoned. PostgreSQL 16,
-- 2026-09-04, 20,000 workflow_instances rows of which 6,693 are claimable,
-- EXPLAIN (ANALYZE, BUFFERS) of the claim's candidate SELECT under three index
-- configurations:
--
--                                        A: ready-only    B: widened   C: ready-only +
--                                        (before this)    (this file)  terminating-only
--   with an explicit tenant predicate    tenant_status_   claim_order  tenant_status_
--                                        created (883)    (830)        created (883)
--   without one (see below)              SEQ SCAN (1404)  claim_order  SEQ SCAN (1404)
--                                                         (1222)
--
-- C is the alternative this file rejects, and it buys nothing: the planner does
-- not combine two partial indexes for `status IN (...)` here -- it ignores both
-- and produces exactly A's plan.
--
-- "Without a tenant predicate" is not hypothetical. PostgresStore.ClaimWorkflows'
-- candidate SELECT carries none: its isolation is RLS, not a predicate the
-- planner sees written down. That is the row where the old indexes lose the
-- claim entirely.
--
-- Re-derive: seed with
--   INSERT INTO workflow_instances (id, def_name, def_version, status,
--       next_wake_at, input, task_queue, priority)
--   SELECT 'x' || g, <a def>, 1,
--          CASE WHEN g % 500 = 0 THEN 'terminating'
--               WHEN g % 3 = 0 THEN 'ready' ELSE 'running' END,
--          now() - (g || ' seconds')::interval, '{}', 'default', 0
--   FROM generate_series(1, 20000) g;
--   ANALYZE workflow_instances;
-- then EXPLAIN the claim's candidate SELECT with and without these indexes.
--
-- What the widening costs in size: the rows added are exactly the workflows
-- currently in their defer phase, which is a handful at a time and zero on a
-- deployment that does not use `defer`.
--
-- Created under new names BEFORE the old ones are dropped, so there is no
-- window in which the claim has no index to use. The build is proportional to
-- the runnable backlog rather than to the table, because these are partial
-- indexes over exactly that backlog.
-- ===========================================================================

CREATE INDEX IF NOT EXISTS idx_instances_claimable
    ON workflow_instances(status, next_wake_at)
    WHERE status IN ('ready', 'terminating');

CREATE INDEX IF NOT EXISTS idx_instances_tenant_claimable
    ON workflow_instances(tenant_id, status, next_wake_at)
    WHERE status IN ('ready', 'terminating');

CREATE INDEX IF NOT EXISTS idx_instances_tenant_queue_claimable
    ON workflow_instances(tenant_id, task_queue, status, priority ASC, next_wake_at)
    WHERE status IN ('ready', 'terminating');

CREATE INDEX IF NOT EXISTS idx_instances_claim_order
    ON workflow_instances (tenant_id, task_queue, priority, created_at)
    WHERE status IN ('ready', 'terminating');

DROP INDEX IF EXISTS idx_instances_ready;
DROP INDEX IF EXISTS idx_instances_tenant_ready;
DROP INDEX IF EXISTS idx_instances_tenant_queue_ready;
DROP INDEX IF EXISTS idx_instances_ready_claim;
