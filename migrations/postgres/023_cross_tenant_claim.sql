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
-- SECURITY DEFINER alone does NOT work here. It fails loudly rather than
-- silently, and it is worth knowing which, because an earlier version of this
-- comment asserted the opposite.
--
-- MEASURED 2026-08-09 by removing the attribute (ALTER ROLE cleat_dispatcher
-- NOBYPASSRLS) and calling both functions: each raises
--
--   pq: cleat.tenant_id is not set -- tenant context required for RLS-scoped
--   query (P0001)
--
-- rather than returning fewer rows. That is 001_schema.sql's doing: its
-- policies are fail-closed through cleat.assert_tenant_set(), which RAISES on
-- an unset GUC instead of COALESCE-ing to a default, and these functions are
-- deliberately called outside beginTxWithRLS so no tenant GUC is ever set on
-- that connection (set_config here is transaction-local).
--
-- So the loud failure is a property of the fail-closed policy choice, not luck.
-- If a future policy is ever written with COALESCE, this becomes the silent
-- failure the old comment described, and the startup check in
-- PostgresStore.CheckCrossTenantCapability becomes the only thing that can see
-- it. It reports the missing attribute by name either way, which is the useful
-- part: P0001 says the tenant context is unset and says nothing about
-- BYPASSRLS.
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

-- DEGRADES ON MANAGED POSTGRESQL RATHER THAN ABORTING THE MIGRATION RUN.
--
-- BYPASSRLS can only be granted by a true superuser, and RDS, Cloud SQL and
-- Azure do not have one. Measured on PostgreSQL 16 against a role of exactly
-- the shape AWS documents for the RDS master user:
--
--   ERROR:  permission denied to create role
--   DETAIL:  Only roles with the BYPASSRLS attribute may create roles with
--            the BYPASSRLS attribute.
--
-- Unhandled, that error stopped the whole migration run at file 23 of 72 and
-- took 040 and 073 down with it -- so cleat could not be INSTALLED on managed
-- PostgreSQL at all, not merely run single-tenant there.
--
-- Caught, the file continues and the function below is created owned by the
-- migrating role instead. That is deliberate rather than a consolation: an
-- owner without BYPASSRLS is the exact state
-- PostgresStore.CheckCrossTenantCapability already probes for and already
-- words correctly -- "exists and is executable, but its owner does not have
-- BYPASSRLS" -- which is a truer thing to tell an RDS operator than "does not
-- exist; apply 023", a file they cannot apply.
--
-- The degradation is the repo's existing idiom for this, not a new one:
-- 001_schema.sql's create_tenant_role catches its own CREATE ROLE failure and
-- warns "skipping (single-tenant mode)".
--
-- Cross-tenant dispatch is NOT lost on those platforms. --claim-strategy=rotate
-- claims each tenant's work under that tenant's own RLS context and needs no
-- exemption at all; this function is what the older `global` strategy uses.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        BEGIN
            CREATE ROLE cleat_dispatcher NOLOGIN BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'cleat_dispatcher needs BYPASSRLS, which only a superuser can grant, and this connection is not one. The cross-tenant claim function is still created but will not see across tenants; use --claim-strategy=rotate, which needs no grant. (SQLSTATE %)', SQLSTATE;
        END;
    ELSIF NOT (SELECT rolbypassrls FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        -- Pre-existing but without the attribute: the function would be owned
        -- by a role that cannot see across tenants, so the claim would return
        -- nothing and the dispatch loop would look idle forever.
        BEGIN
            ALTER ROLE cleat_dispatcher BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'cleat_dispatcher exists without BYPASSRLS and this connection cannot grant it. (SQLSTATE %)', SQLSTATE;
        END;
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
-- Conditional on the role, for the reason the block above gives: on managed
-- PostgreSQL it does not exist and these would abort the run.
DO $do$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        EXECUTE format('GRANT USAGE ON SCHEMA %I TO cleat_dispatcher', current_schema());
        EXECUTE 'GRANT SELECT, UPDATE ON workflow_instances TO cleat_dispatcher';
    END IF;
END $do$;

-- Dropped before it is created, for the reason 003_procedures.sql documents at
-- length: 040_claim_terminating_workflows.sql replaces this function with one
-- that returns an extra column, and PostgreSQL rejects a return-type change
-- through CREATE OR REPLACE with
--   ERROR: cannot change return type of existing function (42P13)
-- Re-applying the migration set to a database that already has 040's version
-- would otherwise fail here, and re-applying is exactly what an operator
-- upgrading an existing deployment does. TestShippedSchema_IsIdempotent
-- enforces it, and also asserts the END state is 040's shape -- which holds
-- because 040 sorts after 023 and is re-applied in the same pass.
DROP FUNCTION IF EXISTS admin.claim_workflows(text, text[], integer);

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
SET search_path FROM CURRENT
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
