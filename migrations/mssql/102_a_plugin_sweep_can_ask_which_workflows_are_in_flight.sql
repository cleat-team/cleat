-- ===========================================================================
-- 102: a plugin's background sweep can ask which workflows are in flight
-- (SQL Server)
--
-- WHAT THIS FIXES: cleat#2125, this dialect's half of cleat#1528/073. Neither
-- blobstore's staleWorkflowRefs nor jobqueue's abandonedJobsQuery ever worked
-- against workflow_instances on SQL Server: dbo.fn_tenant_filter (001, then
-- 075) applies to every principal with no exemption, so a tenant-less
-- background sweep's direct
--
--   SELECT id FROM workflow_instances WHERE status IN ('ready', 'running')
--
-- saw zero rows on every tick, on a default (075's plain-form) deployment.
-- staleWorkflowRefs then deleted the in-flight workflow's own blob refs, and
-- abandonedJobsQuery marked every dispatched job's run abandoned, whether or
-- not it was still running. Both measured on a real SQL Server 2022 container
-- before this migration was written.
--
-- WHY NOT PostgreSQL 073's SHAPE VERBATIM: OWNER TO / SECURITY DEFINER. SQL
-- Server has no SECURITY DEFINER, and ownership chaining does not apply
-- across a security-policy filter predicate the way it does across an
-- ordinary permission check -- the predicate governs row VISIBILITY to the
-- CALLER, not to a called routine's owner. The SQL-Server-native mechanism
-- for "this routine's body runs as a different principal than its caller" is
-- EXECUTE AS, and the principal it runs as needs the SAME kind of admission
-- 073 grants cleat_dispatcher: fn_tenant_filter must name it explicitly.
--
-- THREE THINGS VERIFIED ON A REAL SQL SERVER 2022 CONTAINER BEFORE WRITING
-- THIS, because none of them is documented behavior anyone here had used:
--
--   * An INLINE table-valued function (RETURNS TABLE ... RETURN SELECT ...)
--     REJECTS WITH EXECUTE AS: "Msg 487: An invalid option was specified for
--     the statement". A MULTI-STATEMENT one (RETURNS @t TABLE (...) AS BEGIN
--     ... RETURN; END) ACCEPTS it. That is why the function below is the
--     multi-statement form and not the shorter inline one every other
--     read-only helper in this file uses.
--   * USER_NAME() inside a function body executing under EXECUTE AS reports
--     the IMPERSONATED principal, not the caller -- confirmed with a throwaway
--     helper function before trusting it in a security predicate.
--   * The exemption is bounded to code actually executing as that principal:
--     a plain caller with no session context sees 0 rows directly, and a
--     caller whose SESSION_CONTEXT names a DIFFERENT tenant still sees 0 rows
--     for a workflow it does not own, directly. Only the impersonating
--     function sees across tenants. Both are negative controls, not
--     hypothesized -- run against the modified predicate before this file
--     existed.
--
-- WHY A NOLOGIN USER RATHER THAN A ROLE. EXECUTE AS names a principal that can
-- be impersonated: a database user, not a role -- SQL Server has no
-- "impersonate this role" form of EXECUTE AS. cleat_dispatcher is WITHOUT
-- LOGIN for the same reason PostgreSQL's is NOLOGIN: nothing should ever
-- authenticate as it, and USER_NAME() is exactly as trustworthy as that
-- guarantee.
--
-- WHAT THE EXEMPTION CAN DO IS BOUNDED BY THE BODY, same as 073: the function
-- takes no arguments, returns only ids, and cleat_dispatcher owns nothing
-- else. An id is not tenant data; the only thing a caller of this function
-- learns is that some workflow, somewhere, is ready or running.
--
-- NO cleat_app PRINCIPAL EXISTS ON THIS DIALECT to grant to -- confirmed by
-- grep; it is a PostgreSQL role name, referenced on MSSQL only in an
-- explanatory comment in 012_admin_role.sql. Granted to PUBLIC instead, which
-- is safe for the reason the paragraph above gives: the function cannot be
-- asked for anything but the full in-flight id list, the same thing 073 hands
-- cleat_app. SELECT, not EXECUTE: a table-valued function is queried, and SQL
-- Server refuses "GRANT EXECUTE" on one with error 4606 -- confirmed by
-- trying it before this file shipped.
--
-- WHY fn_tenant_filter IS TOUCHED HERE AT ALL, AND UNCONDITIONALLY RESET TO
-- THE PLAIN FORM. This migration is not the first to redefine it -- 075 did,
-- and migrations/mssql/optional/cross_tenant_claim.sql can, at any time, as
-- an operator action outside the numbered sequence. Editing THOSE FILES (done
-- in this same change, so a fresh install or a fresh opt-in already carries
-- the disjunct below) cannot help a database that already applied one of them
-- before this migration shipped: migration.Runner records applied versions
-- and never re-reads an old file's content. Only a NEW, numbered migration
-- that every upgrading database runs exactly once can retrofit the exemption
-- onto an existing deployment, so this migration resets fn_tenant_filter
-- itself, the same way 075 already resets it from 012's form to its own --
-- see 075's header for that precedent.
--
-- A deployment that had applied cross_tenant_claim.sql (the 'admin' form)
-- before upgrading past this migration loses that opt-in when this migration
-- runs, exactly as it would if an operator re-applied 075 by hand. Re-apply
-- migrations/mssql/optional/cross_tenant_claim.sql afterward to restore it;
-- the updated copy carries this migration's disjunct too, so doing so does
-- not undo this fix.
--
-- Defined as a plain CREATE OR ALTER in its own batch, never inside
-- EXEC(N'...') -- engine/routine_definition_drift_test.go reads this file
-- TEXTUALLY to learn fn_tenant_filter's shipped definition, and 075's own
-- comment already records what dynamic SQL does to that parse: it reads the
-- whole migration as the function's body and reports drift against a
-- database that is correct. See that test's mssqlOptedIntoTheCrossTenantPredicate
-- for the other half -- it exempts fn_tenant_filter from this comparison
-- entirely on a database that opted into the admin form, which is what makes
-- re-applying cross_tenant_claim.sql after this migration safe as far as that
-- guard is concerned.
-- ===========================================================================

IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'cleat_dispatcher' AND type = N'S')
    CREATE USER cleat_dispatcher WITHOUT LOGIN;

GO

-- Policies first, before the function they depend on is altered -- same
-- reason and same derivation as 075: a SECURITY POLICY's FILTER PREDICATE
-- holds a hard dependency on the function, CREATE OR ALTER fails while any of
-- them exists, and the set is read from sys.security_predicates rather than
-- hand-listed because a hand-written list goes stale the moment a later
-- migration adds a policy.
DECLARE @fn INT = OBJECT_ID(N'dbo.fn_tenant_filter');

IF @fn IS NULL
    THROW 50102, N'102: dbo.fn_tenant_filter does not exist; 001_schema.sql has not been applied', 1;

IF OBJECT_ID(N'tempdb..#cleat_2125_bound_policies') IS NOT NULL DROP TABLE #cleat_2125_bound_policies;
CREATE TABLE #cleat_2125_bound_policies (policy_name SYSNAME, target_schema SYSNAME, target_name SYSNAME);

INSERT INTO #cleat_2125_bound_policies (policy_name, target_schema, target_name)
SELECT sp.name, SCHEMA_NAME(o.schema_id), o.name
  FROM sys.security_predicates AS pred
  JOIN sys.security_policies   AS sp ON sp.object_id = pred.object_id
  JOIN sys.objects             AS o  ON o.object_id  = pred.target_object_id
 WHERE pred.predicate_definition LIKE N'%fn_tenant_filter%';

-- A zero here would silently produce a database with NO row-level security
-- and a migration that reported success -- same guard, same reasoning as 075.
IF (SELECT COUNT(*) FROM #cleat_2125_bound_policies) = 0
    THROW 50102, N'102: no security policy references dbo.fn_tenant_filter; refusing to continue rather than leave the tables unguarded', 1;

DECLARE @sql NVARCHAR(MAX) = N'';

SELECT @sql = @sql + N'DROP SECURITY POLICY dbo.' + QUOTENAME(policy_name) + N';' + CHAR(10)
  FROM (SELECT DISTINCT policy_name FROM #cleat_2125_bound_policies) AS d;
EXEC sp_executesql @sql;

GO

-- The plain form (001/075) plus the cleat_dispatcher exemption. Resets any
-- 'admin' opt-in to 'plain' -- see the header for why that is unavoidable and
-- how to restore it.
CREATE OR ALTER FUNCTION dbo.fn_tenant_filter(@tenant_id UNIQUEIDENTIFIER)
RETURNS TABLE
WITH SCHEMABINDING
AS
RETURN SELECT 1 AS access
    WHERE @tenant_id = CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER)
       OR USER_NAME() = N'cleat_dispatcher';

GO

DECLARE @sql NVARCHAR(MAX) = N'';
SELECT @sql = @sql
     + N'CREATE SECURITY POLICY dbo.' + QUOTENAME(policy_name)
     + N' ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON '
     + QUOTENAME(target_schema) + N'.' + QUOTENAME(target_name)
     + N' WITH (STATE = ON);' + CHAR(10)
  FROM #cleat_2125_bound_policies;
EXEC sp_executesql @sql;

MERGE admin.rls_predicate_form AS t
USING (SELECT 1 AS only_row, N'plain' AS form) AS s
   ON t.only_row = s.only_row
 WHEN MATCHED THEN UPDATE SET form = s.form, applied_at = SYSUTCDATETIME()
 WHEN NOT MATCHED THEN INSERT (only_row, form) VALUES (s.only_row, s.form);

GO

-- admin.fn_in_flight_workflow_ids(): the read a background sweep uses instead
-- of querying workflow_instances directly. Multi-statement, not inline --
-- see the header for why EXECUTE AS requires that shape here.
CREATE OR ALTER FUNCTION admin.fn_in_flight_workflow_ids()
RETURNS @result TABLE (id NVARCHAR(255))
WITH EXECUTE AS 'cleat_dispatcher'
AS
BEGIN
    INSERT INTO @result (id)
    SELECT id FROM dbo.workflow_instances WHERE status IN (N'ready', N'running');
    RETURN;
END;

GO

GRANT SELECT ON admin.fn_in_flight_workflow_ids TO PUBLIC;

GO
