-- cleat migration 031 (mssql): add the missing SECURITY POLICY for
-- dbo.workflow_promises.
--
-- Finding S1's MSSQL parity gap. PostgreSQL's 001_schema.sql enables RLS on
-- eight tables, workflow_promises among them. migrations/mssql/001_schema.sql's
-- CREATE SECURITY POLICY block (restated in 012_admin_role.sql when
-- fn_tenant_filter picked up the cleat_admin bypass) covers seven of the
-- same eight tables and omits dbo.workflow_promises -- verified 2026-08-09
-- with:
--
--   grep -n '^CREATE SECURITY POLICY' migrations/mssql/001_schema.sql | wc -l   -> 7
--   grep -n '^CREATE SECURITY POLICY' migrations/mssql/012_admin_role.sql | wc -l -> 7
--   grep -n '^CREATE SECURITY POLICY' migrations/mssql/001_schema.sql   -- names
--     TenantFilter_Defs, _Instances, _EventHistory, _Signals, _Schedules,
--     _Tags, _Routing -- workflow_promises is not among them
--
-- dbo.workflow_promises already has a tenant_id column
-- (migrations/mssql/001_schema.sql, NVARCHAR(255) -- the one tenant-scoped
-- table on this dialect that stores it as a string rather than
-- UNIQUEIDENTIFIER; engine/mssql_signals_promises.go's CreatePromise writes
-- s.tenantID, which is always a UUID string, so the implicit conversion
-- fn_tenant_filter's UNIQUEIDENTIFIER parameter requires is well-formed in
-- practice). Postgres already protects the equivalent table; this brings
-- SQL Server to parity.
--
-- This migration does not touch dbo.fn_tenant_filter or the other seven
-- policies -- unlike 012_admin_role.sql, it does not need to, because it
-- adds a new policy rather than altering the function the existing seven
-- depend on (CREATE SECURITY POLICY only requires the function to already
-- exist). The seven are therefore left exactly as 012 last defined them.
--
-- ***************************************************************************
-- VERIFIED against a live SQL Server, 2026-08-09 (Stream J,
-- CLEAT_TEST_MSSQL='sqlserver://sa:...@localhost:1435?database=cleat').
--
-- This migration ran on a fresh database built from migrations/mssql/*.sql
-- (recorded in schema_migrations as version 31) with no manual intervention,
-- confirming the static reasoning this comment used to carry (CREATE
-- SECURITY POLICY shape, the implicit NVARCHAR(255)-to-UNIQUEIDENTIFIER
-- conversion, the FK cascade interaction) was correct.
--
-- engine/testutil/mssql_schema.go no longer curates a file list at all (see
-- engine/testutil/migrations.go's header): applyMigrations runs every file
-- under migrations/mssql/ through the real migration.Runner, so this
-- migration has been part of every MSSQL test's schema since that change
-- landed, with no separate wiring step needed. The "not wired in" gap this
-- comment used to describe closed as a side effect of that change, not
-- because anyone edited a list here.
--
-- engine/mssql_rls_enforcement_test.go's enableMSSQLTenantPolicies now reads
-- this file in addition to 001_schema.sql, so its mssqlPolicyRe scan covers
-- all eight tenant-scoped tables, this one included.
--
-- TestMSSQLTenantIsolation_WorkflowPromises_UnderRealSecurityPolicies is the
-- layer-separation proof this migration's isolation claim needed: it seeds
-- two tenants' promises via CreatePromise (the one write path with no
-- Go-level tenant predicate -- it is a bare INSERT) and reads them back with
-- raw SQL naming no tenant_id column anywhere in the query text, so
-- TenantFilter_Promises is the only thing that can hide a row. Proven to
-- fail for the right reason: with `ALTER SECURITY POLICY
-- dbo.TenantFilter_Promises WITH (STATE = OFF)`, the test fails with "exists
-- but is disabled, so it filters nothing", and a direct check while disabled
-- confirmed the row *is* visible with no session context set at all
-- (COUNT = 1), i.e. the policy -- not some other layer -- is what was hiding
-- it. Restoring STATE = ON returns the suite to green.
-- ***************************************************************************

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Promises')
    DROP SECURITY POLICY dbo.TenantFilter_Promises;
GO

CREATE SECURITY POLICY dbo.TenantFilter_Promises
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_promises
    WITH (STATE = ON);
