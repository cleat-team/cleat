-- ===========================================================================
-- 073: a plugin's background sweep can ask which workflows are in flight
--
-- WHAT THIS FIXES: cleat#1528. blobstore's TTL cleanup has never run on a
-- deployment where the worker is subject to row-level security, and nothing
-- reports it as broken -- the loop logs "blobstore: TTL cleanup failed" each
-- tick and keeps ticking. Expired blob_index entries are never deleted,
-- blob_content.ref_count is never decremented, and orphaned content is never
-- collected.
--
-- The cause is its FIRST statement, which is not about its own tables at all:
--
--   DELETE FROM workflow_blob_refs
--   WHERE workflow_id NOT IN (
--       SELECT id FROM workflow_instances WHERE status IN ('ready','running')
--   )
--
-- workflow_instances is a CORE table, and 001_schema.sql's policy on it is the
-- older inline form
--
--   tenant_isolation_instances | (tenant_id = cleat.assert_tenant_set())
--
-- which RAISEs on an unset tenant. A background sweep has no tenant, so the
-- subquery is refused and the whole sweep returns before reaching phase 2.
--
-- MEASURED, and the control is what makes it a finding rather than a guess.
-- Connected as a role with rolsuper=false and rolbypassrls=false, with
-- blob_index carrying NO POLICY AT ALL (relrowsecurity=false,
-- relforcerowsecurity=false, verified in the same session):
--
--   cleanupExpired(context.Background()) -> pq: cleat.tenant_id is not set (P0001)
--
-- So this predates cleat#1512's work on blob_index and is not caused by it.
--
-- WHY NOT plugin.AcrossAllTenants, which is the mechanism cleat#1278 added for
-- exactly this shape. Because cleat.tenant_row_is_visible() -- the predicate
-- that honours a named bypass -- is what migration 063 gave to PLUGIN tables.
-- workflow_instances still carries the 001 predicate, which consults nothing
-- and raises. A sweep that names itself cross-tenant is refused identically.
--
-- WHY NOT MOVE workflow_instances TO tenant_row_is_visible, which would be a
-- two-line migration and make the bypass work uniformly. Because it would let
-- ANY plugin that names itself cross-tenant read the engine's own table. That
-- trades a bounded question for an open capability, on behalf of two callers.
-- The narrow form is the one this repository already chose twice: see 023 and
-- 024, both named "cross_tenant_*", both exposing one query rather than one
-- exemption.
--
-- AND THE OWNER IS NOT EXEMPT, WHICH IS THE PART THAT IS EASY TO GET WRONG.
-- SECURITY DEFINER alone does nothing here: 001_schema.sql sets FORCE ROW LEVEL
-- SECURITY on workflow_instances, so the table owner is subject to its own
-- policies and has no exemption to lend. In PostgreSQL the only exemptions are
-- superuser and BYPASSRLS. 023 measured this directly -- ALTER ROLE
-- cleat_dispatcher NOBYPASSRLS makes admin.claim_workflows raise P0001 rather
-- than return fewer rows -- and says so under the heading "Why a BYPASSRLS role
-- and not just SECURITY DEFINER". This function therefore reuses that role
-- rather than introducing a second one.
--
-- WHAT THE EXEMPTION CAN DO IS BOUNDED BY THE BODY. cleat_dispatcher is NOLOGIN
-- and owns nothing but these functions; this one returns a list of ids and
-- takes no arguments, so there is no predicate a caller can influence. It reads
-- a column that is not tenant data -- an opaque workflow id -- and the only
-- thing a caller learns is whether some workflow, somewhere, is still running.
--
-- MySQL and SQL Server: unaffected and not mirrored. Neither has this policy,
-- so the plugin keeps the direct subquery there; see the Default/MySQL/MSSQL
-- arms of blobstore's staleWorkflowRefs query.
-- ===========================================================================

-- 023 creates cleat_dispatcher and grants it USAGE on the schema plus SELECT
-- and UPDATE on workflow_instances. SELECT is all this function needs, so
-- nothing new is granted on the table.
--
-- Guarded anyway: a database that has not applied 023 has no such role, and a
-- bare ALTER FUNCTION ... OWNER TO would fail the whole migration rather than
-- this one statement. That cannot happen through the ordered runner -- 023
-- sorts first -- but these files are also applied by hand.
-- THE DISCRIMINATOR IS 023'S FUNCTION, NOT ITS ROLE.
--
-- "No cleat_dispatcher" used to mean one thing -- 023 was skipped -- and now
-- means two. Since 023 degrades rather than aborting when BYPASSRLS cannot be
-- granted (managed PostgreSQL has no superuser to grant it), a database can
-- have had 023 applied in full and still have no such role.
--
-- admin.claim_workflows separates the cases: 023 creates it unconditionally,
-- so its absence still means 023 never ran, which is the out-of-order hand
-- application this guard was written for. Its presence with no role means 023
-- ran and degraded, and this file should degrade the same way rather than
-- refuse to apply.
DO $$
BEGIN
    IF to_regprocedure('admin.claim_workflows(text, text[], integer)') IS NULL THEN
        RAISE EXCEPTION
            'cleat_dispatcher does not exist: apply migrations/postgres/023_cross_tenant_claim.sql first';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        RAISE NOTICE 'cleat_dispatcher does not exist because 023 could not create it on this platform; admin.in_flight_workflow_ids is created without the exemption and will see only the caller''s own tenant.';
    END IF;
END
$$;

-- Dropped before created, for the reason 003_procedures.sql documents: a later
-- migration that changes the return type cannot use CREATE OR REPLACE
-- (42P13), and re-applying the whole set to an existing database is exactly
-- what an operator upgrading does. TestShippedSchema_IsIdempotent enforces it.
DROP FUNCTION IF EXISTS admin.in_flight_workflow_ids();

-- STABLE, not VOLATILE. It reads and writes nothing, so the planner may hoist
-- it out of a scan and use it in an index condition. cleat#1488 is what a
-- VOLATILE predicate costs: every RLS policy in the schema rested on one, and
-- the planner was denied every tenant_id-leading index in the database.
CREATE FUNCTION admin.in_flight_workflow_ids()
RETURNS TABLE (id TEXT)
LANGUAGE sql
STABLE
SECURITY DEFINER
-- Pinned so the body cannot be redirected by a caller's search_path. Standard
-- hardening for SECURITY DEFINER and not optional when the function holds an
-- RLS exemption. FROM CURRENT rather than a literal, because --schema puts
-- workflow_instances somewhere other than public and the migration runs with
-- that schema already in scope -- the same form 023 uses.
SET search_path FROM CURRENT
AS $$
    SELECT w.id FROM workflow_instances w WHERE w.status IN ('ready', 'running');
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
        EXECUTE 'ALTER FUNCTION admin.in_flight_workflow_ids() OWNER TO cleat_dispatcher';
    EXCEPTION WHEN insufficient_privilege OR undefined_object THEN
        RAISE NOTICE 'cannot give the function to cleat_dispatcher (SQLSTATE %); it keeps the migrating role as its owner and will not see across tenants. Use --claim-strategy=rotate, which needs no exemption.', SQLSTATE;
    END;
END $do$;

-- REVOKE first: PostgreSQL grants EXECUTE to PUBLIC on a new function by
-- default, which would hand the exemption to every role in the database.
-- Migration 065 also revokes it schema-wide and sets a default privilege, but
-- that covers functions created by the role that ran 065; this is explicit for
-- the same reason 023 is.
REVOKE ALL ON FUNCTION admin.in_flight_workflow_ids() FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_app') THEN
        GRANT EXECUTE ON FUNCTION admin.in_flight_workflow_ids() TO cleat_app;
    END IF;
END
$$;
