-- ===========================================================================
-- OPTIONAL: admit dbo.cleat_admin across tenants (the --claim-across-tenants
-- prerequisite on SQL Server)
--
-- NOT APPLIED AUTOMATICALLY, and the mechanism for that is the directory rather
-- than a flag: migration.Runner.readMigrations walks migrations/<dialect>/ and
-- skips directory entries, so nothing under optional/ is ever picked up. It
-- also means this file has no version number and is not recorded in
-- schema_migrations -- applying it is an operator action, and
-- admin.rls_predicate_form below is what records that it happened.
--
-- WHEN YOU NEED THIS
-- ------------------
-- Only if you run a worker with --claim-across-tenants. Without it the worker
-- claims work for its own tenant, which is the default and is what most
-- deployments want. cleat#1541.
--
-- WHAT IT COSTS, MEASURED
-- -----------------------
-- SQL Server 2022, 200k rows over 200 tenants, tenant_id indexed:
--
--   predicate    query carries tenant_id = ?   plan          logical reads
--   plain        yes                           Index Seek    33
--   plain        NO                            Index Seek    33
--   this one     yes                           Index Seek    33
--   this one     NO                            Index Scan    5760
--
-- The disjunction is free for a query that supplies its own tenant and costs
-- the seek for one that relies on RLS to supply it. 40 of cleat's 58
-- tenant-scoped statements carry their own predicate; the ones that cannot are
-- ClaimWorkflowsAcrossTenants and BatchHeartbeat, which scan by design.
--
-- APPLY IT WITH
-- -------------
--   sqlcmd -S <server> -d <database> -i migrations/mssql/optional/cross_tenant_claim.sql
--
-- then grant membership, which this file deliberately does NOT do -- adding a
-- principal to an administrative role is a decision for whoever owns the
-- database, not a side effect of running a script:
--
--   CREATE LOGIN cleat_admin_login WITH PASSWORD = '...';
--   CREATE USER  cleat_admin_login FOR LOGIN cleat_admin_login;
--   ALTER ROLE   cleat_admin ADD MEMBER cleat_admin_login;
--
-- TO REVERSE IT
-- -------------
-- Re-apply migrations/mssql/075_the_admin_bypass_is_opt_in.sql. It is
-- idempotent and restores the plain predicate and the 'plain' marker.
-- ===========================================================================

SET XACT_ABORT ON;
BEGIN TRANSACTION;

DECLARE @fn INT = OBJECT_ID(N'dbo.fn_tenant_filter');
IF @fn IS NULL
    THROW 50075, N'dbo.fn_tenant_filter does not exist; apply the numbered migrations first', 1;

IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'cleat_admin' AND type = N'R')
    THROW 50075, N'the dbo.cleat_admin role does not exist; migration 012_admin_role.sql has not been applied', 1;

IF OBJECT_ID(N'admin.rls_predicate_form') IS NULL
    THROW 50075, N'admin.rls_predicate_form does not exist; migration 075 has not been applied, and without it nothing records which predicate is installed', 1;

-- Derived, not listed. See 075 for why: the hand-written list is wrong the
-- moment a later migration adds a policy, and there are more of them than the
-- file that introduced them suggests.
--
-- FILTER predicates only, and that restriction is load-bearing, not tidiness:
-- see 075's identical comment on its own capture query. cleat#2205 (migration
-- 103) puts three BLOCK predicates on the same function beside every FILTER
-- one, so an unfiltered capture returns four rows per table and the CREATE
-- loop below -- one CREATE SECURITY POLICY per row, no DISTINCT -- fails on
-- the second row for the same table with "There is already an object named
-- ...". Reproduced against a database with 103 applied.
DECLARE @bound TABLE (policy_name SYSNAME, target_schema SYSNAME, target_name SYSNAME);

INSERT INTO @bound (policy_name, target_schema, target_name)
SELECT sp.name, SCHEMA_NAME(o.schema_id), o.name
  FROM sys.security_predicates AS pred
  JOIN sys.security_policies   AS sp ON sp.object_id = pred.object_id
  JOIN sys.objects             AS o  ON o.object_id  = pred.target_object_id
 WHERE pred.predicate_definition LIKE N'%fn_tenant_filter%'
   AND pred.predicate_type_desc = N'FILTER';

IF (SELECT COUNT(*) FROM @bound) = 0
    THROW 50075, N'no security policy references dbo.fn_tenant_filter; refusing to continue rather than leave the tables unguarded', 1;

DECLARE @sql NVARCHAR(MAX) = N'';
SELECT @sql = @sql + N'DROP SECURITY POLICY dbo.' + QUOTENAME(policy_name) + N';' + CHAR(10)
  FROM (SELECT DISTINCT policy_name FROM @bound) AS d;
EXEC sp_executesql @sql;

EXEC(N'
CREATE OR ALTER FUNCTION dbo.fn_tenant_filter(@tenant_id UNIQUEIDENTIFIER)
RETURNS TABLE
WITH SCHEMABINDING
AS
RETURN SELECT 1 AS access
    WHERE @tenant_id = CAST(SESSION_CONTEXT(N''tenant_id'') AS UNIQUEIDENTIFIER)
       OR IS_ROLEMEMBER(N''cleat_admin'') = 1;
');

SET @sql = N'';
SELECT @sql = @sql
     + N'CREATE SECURITY POLICY dbo.' + QUOTENAME(policy_name)
     + N' ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON '
     + QUOTENAME(target_schema) + N'.' + QUOTENAME(target_name)
     + N' WITH (STATE = ON);' + CHAR(10)
  FROM @bound;
EXEC sp_executesql @sql;

-- cleat#2205 (migration 103): the DROP above took every BLOCK predicate a
-- recreated policy carried down with it. Put them back, on the same
-- function, before this transaction commits -- opting into
-- --claim-across-tenants must not be how a deployment silently loses its
-- cross-tenant write protection.
SET @sql = N'';
SELECT @sql = @sql
     + N'ALTER SECURITY POLICY dbo.' + QUOTENAME(policy_name)
     + N' ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON '
     + QUOTENAME(target_schema) + N'.' + QUOTENAME(target_name) + N' AFTER INSERT,'
     + N' ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON '
     + QUOTENAME(target_schema) + N'.' + QUOTENAME(target_name) + N' AFTER UPDATE,'
     + N' ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON '
     + QUOTENAME(target_schema) + N'.' + QUOTENAME(target_name) + N' BEFORE UPDATE;' + CHAR(10)
  FROM @bound;
IF LEN(@sql) > 0
    EXEC sp_executesql @sql;

-- The marker is what CheckCrossTenantCapability reads. Role membership alone
-- cannot answer the question once the predicate is variable: a member under the
-- plain predicate reads IS_ROLEMEMBER = 1 and sees zero rows.
UPDATE admin.rls_predicate_form SET form = N'admin', applied_at = SYSUTCDATETIME();

COMMIT TRANSACTION;
