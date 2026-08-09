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
-- UNVERIFIED AGAINST A LIVE SQL SERVER.
--
-- No CLEAT_TEST_MSSQL instance was available to this stream when this
-- migration was written (the shared one was in use by another stream; see
-- PARALLEL-WORKSTREAMS.md). This has been reasoned through statically only:
--
--   - The CREATE SECURITY POLICY / ADD FILTER PREDICATE / WITH (STATE = ON)
--     shape, the guarded DROP-then-CREATE idempotency pattern, and the GO
--     batch separators all match the seven existing policies in
--     migrations/mssql/001_schema.sql exactly.
--   - engine/mssql_rls_enforcement_test.go's mssqlPolicyRe regex
--     (`CREATE SECURITY POLICY (dbo\.\w+)\s+ADD FILTER PREDICATE
--     dbo\.fn_tenant_filter\(tenant_id\) ON dbo\.(\w+)\s+WITH \(STATE = ON\);`)
--     is written to match this exact literal form -- no CAST, no extra
--     whitespace variation -- which is why this file does not add an
--     explicit CAST(tenant_id AS UNIQUEIDENTIFIER) despite the column being
--     NVARCHAR(255): SQL Server's implicit conversion from a
--     GUID-formatted string to UNIQUEIDENTIFIER handles it, and matching the
--     existing regex means that test (once wired to include this file --
--     see below) can discover the new policy the same way it discovers the
--     other seven, rather than needing a second pattern.
--   - dbo.workflow_promises has an FK (fk_promises_workflow, workflow_id ->
--     dbo.workflow_instances(id)) ON DELETE CASCADE, so a cascading delete
--     from workflow_instances now also has to satisfy this policy in
--     whatever session performs it, the same requirement workflow_instances'
--     own existing policy already imposes. This migration does not change
--     that requirement's shape, only extends it to one more table already
--     reachable by the same cascade.
--
-- NOT wired into engine/testutil/mssql_schema.go's mssqlSchemaFiles() list.
-- That file is an explicit list (same shape as postgresSchemaFiles()), owned
-- by another stream this round per PARALLEL-WORKSTREAMS.md ("engine/testutil/
-- is WS-1's this round ... WS-2 and WS-3 should ask before adding test-schema
-- columns"). Until it is added there, engine/mssql_rls_enforcement_test.go
-- will not see this policy and TestMSSQLTenantIsolation_UnderRealSecurityPolicies
-- will not exercise dbo.workflow_promises. Do not treat this migration as
-- proven until both (a) it has run against a real SQL Server and (b) that
-- wiring is done.
-- ***************************************************************************

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Promises')
    DROP SECURITY POLICY dbo.TenantFilter_Promises;
GO

CREATE SECURITY POLICY dbo.TenantFilter_Promises
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_promises
    WITH (STATE = ON);
