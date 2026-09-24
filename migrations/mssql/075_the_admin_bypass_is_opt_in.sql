-- ===========================================================================
-- 075: the cross-tenant admin bypass becomes opt-in, and the predicate says so
--
-- Why
-- ---
-- 012 added `OR IS_ROLEMEMBER(N'cleat_admin') = 1` to dbo.fn_tenant_filter so
-- that SQL Server had an administrative access path at all. It works, and it
-- costs every deployment the seek on any query that does not carry its own
-- tenant predicate. Measured on SQL Server 2022, 200k rows over 200 tenants:
--
--   predicate      query carries tenant_id = ?   plan          logical reads
--   001 plain      yes                           Index Seek    33
--   001 plain      NO                            Index Seek    33
--   012 OR form    yes                           Index Seek    33
--   012 OR form    NO                            Index Scan    5760
--
-- The disjunction is only expensive for a query that relies on RLS to supply
-- the tenant. cleat#1491 is the defect; cleat#1541 established that the switch
-- cannot live in the predicate, because the predicate is evaluated per row and
-- the thing being switched is a property of the DEPLOYMENT.
--
-- So the default returns to 001's plain, seekable predicate, and a deployment
-- that wants `--claim-across-tenants` applies
-- migrations/mssql/optional/cross_tenant_claim.sql on purpose. A deployment
-- that does nothing gets the faster predicate and no bypass, rather than the
-- slower predicate and a bypass nobody is a member of.
--
-- WHAT THIS IS NOT
-- ----------------
-- Not a revert of 012. The cleat_admin ROLE stays: it is what the optional
-- migration admits, and dropping it would break a deployment that has already
-- granted membership. Only the predicate changes.
--
-- THE MARKER, AND WHY IT IS A TABLE RATHER THAN A QUERY
-- ----------------------------------------------------
-- engine.MSSQLStore.CheckCrossTenantCapability answers "can this worker see
-- across tenants?" from IS_ROLEMEMBER alone, which was sufficient while every
-- deployment had the OR form -- its own comment says so: "altering the
-- predicate is a schema change". This migration is what makes it variable, so
-- membership stops being sufficient: a member under the plain predicate reads
-- IS_ROLEMEMBER = 1 and sees ZERO rows. Measured.
--
-- The obvious alternative was to ask the predicate's own definition:
--
--   SELECT 1 FROM sys.sql_modules m JOIN sys.objects o ON o.object_id = m.object_id
--    WHERE o.name = 'fn_tenant_filter' AND m.definition LIKE '%IS_ROLEMEMBER%'
--
-- It discriminates correctly as sa and is USELESS from an unprivileged
-- connection: SQL Server's metadata visibility shows sys.objects rows only for
-- objects the principal has permission on, so a worker with no rights on the
-- function gets 0 under BOTH predicates. Measured -- can_see_the_object_at_all
-- was 0. A check that cannot disagree with itself would have made this change
-- look verified.
--
-- Hence an ordinary table. It is read with a plain SELECT by whatever principal
-- the worker connects as, and it is written by the migration that installs each
-- predicate, so the two can never disagree.
-- ===========================================================================

IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'admin.rls_predicate_form') AND type = N'U')
CREATE TABLE admin.rls_predicate_form (
    only_row   BIT            NOT NULL DEFAULT 1,
    form       NVARCHAR(16)   NOT NULL,
    applied_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT pk_admin_rls_predicate_form PRIMARY KEY (only_row),
    -- One row, enforced rather than assumed: two rows disagreeing about the
    -- predicate is a state no reader could resolve.
    CONSTRAINT ck_admin_rls_predicate_form_single CHECK (only_row = 1),
    CONSTRAINT ck_admin_rls_predicate_form_value CHECK (form IN (N'plain', N'admin'))
);

GO

-- Policies first, before the function they depend on is altered: a SECURITY
-- POLICY's FILTER PREDICATE holds a hard dependency on the function, and
-- CREATE OR ALTER fails with "Cannot ALTER ... because it is being referenced
-- by object ..." while ANY of them exists.
--
-- DERIVED, NOT LISTED. The first draft of this migration hard-coded eight
-- policy names from reading 012. There are SIXTEEN, spread across migrations
-- that post-date it -- Domains, Secrets, Settings, Tags, Routing, MemoryStats,
-- MemorySamples, EventHistory among them -- so the hand-written list would
-- have left half of them bound to the old function and the CREATE OR ALTER
-- would have failed at apply time. A list written by hand is also the list that
-- goes stale the next time a migration adds a policy.
--
-- So the set comes from sys.security_predicates: every policy whose predicate
-- is dbo.fn_tenant_filter, with the table it guards, captured before the drop
-- and replayed after. A policy added by a later migration is carried along
-- without this file being touched.
--
-- The window in which the predicates are absent is bounded by the transaction:
-- migration.Runner.applyMigration opens one per file and commits at the end, so
-- a concurrent session sees either the old policies or the new ones.
DECLARE @fn INT = OBJECT_ID(N'dbo.fn_tenant_filter');

IF @fn IS NULL
    THROW 50075, N'075: dbo.fn_tenant_filter does not exist; 001_schema.sql has not been applied', 1;

IF OBJECT_ID(N'tempdb..#cleat_bound_policies') IS NOT NULL DROP TABLE #cleat_bound_policies;
CREATE TABLE #cleat_bound_policies (policy_name SYSNAME, target_schema SYSNAME, target_name SYSNAME);

-- FILTER predicates only. Before cleat#2205 (migration 103) every table bound
-- to fn_tenant_filter carried exactly one predicate row, so this query and
-- the CREATE loop below (which has no DISTINCT of its own) returned one row
-- per table by construction. 103 adds three BLOCK predicates per table on
-- the same function, so the unfiltered query now returns four rows per
-- table -- and the CREATE loop, run once per row, does
-- `CREATE SECURITY POLICY dbo.TenantFilter_X` a second time and fails with
-- "There is already an object named ...". Restricting the capture to FILTER
-- restores "one row per table" regardless of how many BLOCK predicates that
-- table also carries. Reproduced against a database with 103 applied before
-- this line existed; see the BLOCK predicate re-creation below for the other
-- half of the fix -- capturing only FILTER here would otherwise silently
-- drop 103's protection the moment this file runs.
INSERT INTO #cleat_bound_policies (policy_name, target_schema, target_name)
SELECT sp.name, SCHEMA_NAME(o.schema_id), o.name
  FROM sys.security_predicates AS pred
  JOIN sys.security_policies   AS sp ON sp.object_id = pred.object_id
  JOIN sys.objects             AS o  ON o.object_id  = pred.target_object_id
 WHERE pred.predicate_definition LIKE N'%fn_tenant_filter%'
   AND pred.predicate_type_desc = N'FILTER';

-- A zero here would silently produce a database with NO row-level security and
-- a migration that reported success. 001 binds eight on a fresh install, so
-- anything less than one means the capture failed rather than that the schema
-- is small.
IF (SELECT COUNT(*) FROM #cleat_bound_policies) = 0
    THROW 50075, N'075: no security policy references dbo.fn_tenant_filter; refusing to continue rather than leave the tables unguarded', 1;

DECLARE @sql NVARCHAR(MAX) = N'';

SELECT @sql = @sql + N'DROP SECURITY POLICY dbo.' + QUOTENAME(policy_name) + N';' + CHAR(10)
  FROM (SELECT DISTINCT policy_name FROM #cleat_bound_policies) AS d;
EXEC sp_executesql @sql;

-- 001's predicate, restored verbatim. The absence of the disjunction is the
-- change; anything else differing from 001 would be an unrelated change hiding
-- in this one.
GO

-- Defined as a plain CREATE OR ALTER in its own batch rather than inside
-- EXEC(N'...'), and that is not style. engine's routine-definition drift guard
-- reads this file TEXTUALLY to learn what each routine should be; wrapped in
-- dynamic SQL it read the whole migration -- MERGE, QUOTENAME, sp_executesql --
-- as part of the function body and reported drift against a database that was
-- correct. A definition a reader cannot extract is one a guard cannot check.
--
-- The captured policy set survives the batch boundary because it is a #temp
-- table: those live for the session, and migration.Runner.applyMigration runs
-- every batch of a file on one connection inside one transaction.
CREATE OR ALTER FUNCTION dbo.fn_tenant_filter(@tenant_id UNIQUEIDENTIFIER)
RETURNS TABLE
WITH SCHEMABINDING
AS
RETURN SELECT 1 AS access
    WHERE @tenant_id = CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER);

GO

DECLARE @sql NVARCHAR(MAX) = N'';
SELECT @sql = @sql
     + N'CREATE SECURITY POLICY dbo.' + QUOTENAME(policy_name)
     + N' ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON '
     + QUOTENAME(target_schema) + N'.' + QUOTENAME(target_name)
     + N' WITH (STATE = ON);' + CHAR(10)
  FROM #cleat_bound_policies;
EXEC sp_executesql @sql;

-- cleat#2205 (migration 103): every policy this file just recreated may have
-- carried BLOCK predicates before the DROP above, and CREATE SECURITY POLICY
-- starts a policy with none. Re-add the same three, on the same function,
-- unconditionally -- a fresh install applies 075 before 103 exists, where
-- this is simply a no-op-sized statement over zero tables' worth of nothing
-- yet to preserve; an operator re-running 075 after 103 to reverse
-- cross_tenant_claim.sql is exactly the case this exists for, and it must not
-- silently trade cleat#2205's write protection away as the price of
-- returning to the plain predicate.
SET @sql = N'';
SELECT @sql = @sql
     + N'ALTER SECURITY POLICY dbo.' + QUOTENAME(policy_name)
     + N' ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON '
     + QUOTENAME(target_schema) + N'.' + QUOTENAME(target_name) + N' AFTER INSERT,'
     + N' ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON '
     + QUOTENAME(target_schema) + N'.' + QUOTENAME(target_name) + N' AFTER UPDATE,'
     + N' ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON '
     + QUOTENAME(target_schema) + N'.' + QUOTENAME(target_name) + N' BEFORE UPDATE;' + CHAR(10)
  FROM #cleat_bound_policies;
IF LEN(@sql) > 0
    EXEC sp_executesql @sql;

MERGE admin.rls_predicate_form AS t
USING (SELECT 1 AS only_row, N'plain' AS form) AS s
   ON t.only_row = s.only_row
 WHEN MATCHED THEN UPDATE SET form = s.form, applied_at = SYSUTCDATETIME()
 WHEN NOT MATCHED THEN INSERT (only_row, form) VALUES (s.only_row, s.form);

GO
