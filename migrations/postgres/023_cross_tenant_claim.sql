-- ===========================================================================
-- 023: cross-tenant workflow claim (admin.claim_workflows)
--
-- Why this exists
-- ---------------
-- A worker holds one store, scoped to one tenant, and its dispatch loop claims
-- through it. Every tenant-scoped table has RLS, so the claim only ever returns
-- rows for that tenant -- and a non-default tenant's workflows therefore never
-- execute at all. The schedules that would start them do not fire either, for
-- the same reason.
--
-- The alternative to this function is polling every tenant separately, one
-- query per tenant per tick. That is O(tenants) round trips against the
-- database at the dispatch interval, which is the cost this exists to avoid:
-- one query per tick regardless of tenant count, at the price of one carefully
-- bounded RLS exemption.
--
-- Why a BYPASSRLS role and not just SECURITY DEFINER
-- --------------------------------------------------
-- SECURITY DEFINER alone does NOT work here, and the failure would be silent:
-- the function would return zero rows and read as "there is no work".
--
-- A SECURITY DEFINER function runs as its owner, and 001_schema.sql sets FORCE
-- ROW LEVEL SECURITY on these tables, which subjects the table owner to the
-- policies too. That was deliberate -- 005_app_role.sql explains why -- so the
-- owner has no exemption to lend. In PostgreSQL the only exemptions are
-- superuser and the BYPASSRLS attribute, and superuser is exactly what
-- 005_app_role.sql exists to keep the application out of.
--
-- So the exemption lives in a role that holds nothing else:
--
--   * cleat_dispatcher is NOLOGIN. Nobody can connect as it. It exists to own
--     one function.
--   * That function's body is the claim and nothing else, so what the
--     exemption can do is bounded by what the function does rather than by
--     what its caller asks for.
--   * cleat_app gains EXECUTE on it and no new privilege anywhere else. Its
--     own connections stay subject to every policy, exactly as before.
--
-- What this does not defend against
-- ---------------------------------
-- Worth stating plainly rather than leaving to be discovered: the application
-- already chooses its own tenant context by calling set_config('cleat.tenant_id',
-- ...), so RLS here is a backstop against a missing WHERE clause, not against a
-- compromised application. That was true before this migration.
--
-- What this changes is the blast radius of a bug. A scoping mistake elsewhere
-- leaks one tenant; a cross-tenant read leaks all of them at once. That is the
-- reason the exemption is confined to the claim, and the reason execution runs
-- on a per-tenant store afterwards (see cmd/cleat-worker, storeForTenant) --
-- the rows this returns carry tenant_id precisely so the caller can re-scope
-- before touching anything else.
--
-- Deployment
-- ----------
-- Creating a role with BYPASSRLS requires superuser, so this migration must be
-- applied by one. 005_app_role.sql already requires superuser to CREATE ROLE;
-- this raises that to creating a role with a privilege attribute.
--
-- It fails loudly if it cannot do that, deliberately. A function created
-- without the exemption compiles, runs, returns nothing, and looks exactly
-- like an idle queue.
-- ===========================================================================

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        CREATE ROLE cleat_dispatcher NOLOGIN BYPASSRLS;
    ELSIF NOT (SELECT rolbypassrls FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        -- Pre-existing but without the attribute: the function would be owned
        -- by a role that cannot see across tenants, so the claim would return
        -- nothing and the dispatch loop would look idle forever.
        ALTER ROLE cleat_dispatcher BYPASSRLS;
    END IF;
END
$$;

-- The exemption is not enough on its own: SECURITY DEFINER runs the body as the
-- OWNER, so cleat_dispatcher needs its own privileges on the table. Without
-- these the function raises "permission denied for table workflow_instances"
-- for every caller -- found exactly that way, by calling it as a role RLS
-- actually applies to. Testing it as a superuser proves nothing, because a
-- superuser bypasses RLS and already holds every privilege.
--
-- SELECT and UPDATE only. The body reads candidates and marks them running; it
-- has no reason to insert or delete, and the grant should not imply it can.
GRANT USAGE ON SCHEMA public TO cleat_dispatcher;
GRANT SELECT, UPDATE ON workflow_instances TO cleat_dispatcher;

-- The claim itself. This is the same statement cmd/cleat-worker's dispatch loop
-- has always run -- see engine/store_lifecycle.go ClaimWorkflows -- with no
-- tenant predicate, because it never had one: isolation came entirely from RLS.
-- The column list is the contract with that Go code, and
-- TestClaimWorkflowsAcrossTenants_ColumnsMatchTheGoScan pins the two together,
-- because nothing else would notice them drifting apart.
CREATE OR REPLACE FUNCTION admin.claim_workflows(
    p_worker_id   text,
    p_task_queues text[],
    p_limit       integer
)
RETURNS TABLE (
    id            text,
    def_name      text,
    def_version   integer,
    status        text,
    input         jsonb,
    assigned_to   text,
    next_wake_at  timestamptz,
    tenant_id     uuid,
    created_at    timestamptz,
    error_code    text,
    error_op      text,
    generation    bigint,
    priority      integer,
    trace_id      text
)
LANGUAGE sql
SECURITY DEFINER
-- Pinned so the body cannot be redirected by a caller's search_path. Standard
-- hardening for SECURITY DEFINER, and not optional when the function holds an
-- RLS exemption.
SET search_path = public, pg_temp
AS $$
    WITH candidates AS (
        SELECT w.id FROM workflow_instances w
        WHERE w.status = 'ready'
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
              w.generation, COALESCE(w.priority, 0), COALESCE(w.trace_id, '');
$$;

ALTER FUNCTION admin.claim_workflows(text, text[], integer) OWNER TO cleat_dispatcher;

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
