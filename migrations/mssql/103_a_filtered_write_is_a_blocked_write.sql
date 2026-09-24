-- cleat migration 103 (mssql): BLOCK predicates alongside every FILTER predicate
--
-- FOR THE AUTHOR OF THE NEXT MIGRATION: from here on, ANY migration or
-- backfill that INSERTs or UPDATEs a tenant_id-bearing row on a table this
-- file covers (see its live derivation below -- 14 as of this writing, and
-- growing) must either run as a login in the cleat_admin role with
-- migrations/mssql/optional/cross_tenant_claim.sql's admin-bypass predicate
-- form installed, or call
-- `EXEC sp_set_session_context @key=N'tenant_id', @value=<tenant>` on its own
-- connection, per tenant, before writing. With neither, SESSION_CONTEXT
-- (N'tenant_id') is NULL, `@tenant_id = NULL` is never true, and the BLOCK
-- predicate below refuses the write outright -- SQL Server does not exempt
-- sa or sysadmin from RLS the way PostgreSQL exempts a superuser or table
-- owner. See docs/contributor/plugins/plugin-security.md, "The same BLOCK
-- predicates now cover the core tables too", for the full explanation. As of
-- 2026-09-24 no migration after 002_defaults.sql writes such a row, so
-- nothing existing needed either accommodation; the next one that backfills
-- a core table will.
--
-- cleat#2205. SQL Server's row-level security on this schema has FILTER
-- predicates only. A FILTER predicate silently restricts what SELECT, UPDATE
-- and DELETE can SEE -- it is applied to the statement's WHERE clause the way
-- an implicit `AND fn_tenant_filter(tenant_id)` would be. It says nothing
-- about what a statement WRITES. Measured on a real SQL Server, connected as
-- an ordinary login with tenant A's session context set:
--
--   INSERT INTO dbo.tenant_domains (..., tenant_id, ...) VALUES (..., B, ...)   succeeds, stamped B
--   UPDATE dbo.tenant_domains SET tenant_id = B WHERE hostname = <A's own row>  succeeds, row moves to B
--
-- Postgres does not have this gap: its policies carry a WITH CHECK clause
-- (see e.g. migrations/postgres/*_security_policy.sql), enforced on the
-- post-image of every INSERT and UPDATE. This migration is the SQL Server
-- equivalent of WITH CHECK -- a BLOCK PREDICATE, MSSQL RLS's own mechanism
-- for constraining what a write may leave behind, rather than merely what a
-- read may see.
--
-- REUSES fn_tenant_filter, RATHER THAN A SECOND FUNCTION. MSSQL RLS predicate
-- functions are pure "does this row's tenant_id match the caller's session"
-- predicates, independent of which action (FILTER, or BLOCK's four actions)
-- they are bound to; Microsoft's own reference pattern binds one function to
-- every predicate on a policy. Using the same function means the BLOCK
-- predicates see exactly what the FILTER predicate sees: if a deployment has
-- applied the optional migrations/mssql/optional/cross_tenant_claim.sql (the
-- IS_ROLEMEMBER('cleat_admin') disjunction), cleat_admin members keep the
-- same latitude on writes that they already have on reads, rather than a
-- second, independently-drifting definition of "who is exempt".
--
-- THREE ACTIONS, NOT ONE. AFTER INSERT blocks a write that stamps a row with
-- a tenant_id the caller's session does not hold -- the first case above.
-- AFTER UPDATE blocks a write that leaves a row's tenant_id not matching the
-- caller's session -- the second case, "moves the row". BEFORE UPDATE blocks
-- an UPDATE whose EXISTING (pre-image) row does not match the caller's
-- session; FILTER already keeps such a row out of the WHERE clause for an
-- ordinary UPDATE, but BEFORE UPDATE is what the owner's triage on cleat#2205
-- asked for as the explicit invariant ("a row's tenant must not change"),
-- and it costs nothing extra to state as a predicate rather than lean solely
-- on FILTER's WHERE-clause rewrite to keep meaning that.
--
-- DERIVED, NOT LISTED -- following 075's own precedent and its own stated
-- reason: a hand-written table list is the list that goes stale the next
-- time a migration (core or plugin) adds a FILTER predicate. The set comes
-- from sys.security_predicates: every policy whose predicate is
-- dbo.fn_tenant_filter, captured live. Measured against this tree,
-- 2026-09-24: 14 tables (event_history, queues, tenant_domains,
-- tenant_secrets, tenant_settings, workflow_defs, workflow_instances,
-- workflow_memory_samples, workflow_memory_stats, workflow_promises,
-- workflow_routing, workflow_schedules, workflow_signals, workflow_tags).
-- Any table a PLUGIN registers with its own TenantFilter_* policy against
-- this same function is picked up the same way, with no edit to this file.
--
-- IDEMPOTENT PER (TABLE, OPERATION), NOT PER FILE. sys.security_predicates
-- distinguishes BLOCK predicates by operation_desc ('AFTER INSERT',
-- 'AFTER UPDATE', 'BEFORE UPDATE'); the generated ALTER SECURITY POLICY
-- statement for a table only proposes the operations that table does not
-- already carry, so a second application of this file (or a database that
-- already has some tables covered, from a partial prior run) is a no-op
-- rather than an error. Unlike 075's DROP-then-CREATE, there is no window in
-- which any predicate is absent: ADD BLOCK PREDICATE on an existing, online
-- policy takes effect immediately and atomically, and this migration never
-- touches the FILTER predicates at all.
--
-- A ZERO HERE IS A CAPTURE FAILURE, NOT A SMALL SCHEMA. Same reasoning as
-- 075: 001_schema.sql alone binds eight tables on a fresh install, so finding
-- none means the query failed to find fn_tenant_filter's policies, not that
-- there is nothing to guard.

DECLARE @fn INT = OBJECT_ID(N'dbo.fn_tenant_filter');
IF @fn IS NULL
    THROW 50103, N'103: dbo.fn_tenant_filter does not exist; 001_schema.sql has not been applied', 1;

IF OBJECT_ID(N'tempdb..#cleat_filtered_tables') IS NOT NULL DROP TABLE #cleat_filtered_tables;
CREATE TABLE #cleat_filtered_tables (policy_name SYSNAME, target_schema SYSNAME, target_name SYSNAME, target_object_id INT);

INSERT INTO #cleat_filtered_tables (policy_name, target_schema, target_name, target_object_id)
SELECT DISTINCT sp.name, SCHEMA_NAME(o.schema_id), o.name, o.object_id
  FROM sys.security_predicates AS pred
  JOIN sys.security_policies   AS sp ON sp.object_id = pred.object_id
  JOIN sys.objects             AS o  ON o.object_id  = pred.target_object_id
 WHERE pred.predicate_definition LIKE N'%fn_tenant_filter%'
   AND pred.predicate_type_desc = N'FILTER';

IF (SELECT COUNT(*) FROM #cleat_filtered_tables) = 0
    THROW 50103, N'103: no FILTER predicate references dbo.fn_tenant_filter; refusing to continue', 1;

IF OBJECT_ID(N'tempdb..#cleat_existing_blocks') IS NOT NULL DROP TABLE #cleat_existing_blocks;
CREATE TABLE #cleat_existing_blocks (target_object_id INT, operation_desc NVARCHAR(60));

INSERT INTO #cleat_existing_blocks (target_object_id, operation_desc)
SELECT DISTINCT pred.target_object_id, pred.operation_desc
  FROM sys.security_predicates AS pred
 WHERE pred.predicate_definition LIKE N'%fn_tenant_filter%'
   AND pred.predicate_type_desc = N'BLOCK';

DECLARE @sql NVARCHAR(MAX) = N'';

;WITH wanted AS (
    SELECT target_object_id, op FROM #cleat_filtered_tables
    CROSS JOIN (VALUES (N'AFTER INSERT'), (N'AFTER UPDATE'), (N'BEFORE UPDATE')) AS ops(op)
), missing AS (
    SELECT w.target_object_id, w.op
      FROM wanted AS w
     WHERE NOT EXISTS (
         SELECT 1 FROM #cleat_existing_blocks AS b
          WHERE b.target_object_id = w.target_object_id AND b.operation_desc = w.op
     )
)
SELECT @sql = @sql
     + N'ALTER SECURITY POLICY dbo.' + QUOTENAME(t.policy_name)
     + N' ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON '
     + QUOTENAME(t.target_schema) + N'.' + QUOTENAME(t.target_name)
     + N' ' + m.op + N';' + CHAR(10)
  FROM missing AS m
  JOIN #cleat_filtered_tables AS t ON t.target_object_id = m.target_object_id;

IF LEN(@sql) > 0
    EXEC sp_executesql @sql;

GO
