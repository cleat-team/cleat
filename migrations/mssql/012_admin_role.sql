-- ===========================================================================
-- 012: administrative access under Row-Level Security (dbo.cleat_admin)
--
-- Why this exists
-- ---------------
-- Every tenant-scoped table carries a FILTER PREDICATE built on
-- dbo.fn_tenant_filter (001_schema.sql), and that predicate admits exactly one
-- thing: a connection whose SESSION_CONTEXT('tenant_id') matches the row's
-- tenant_id. There is no principal that can see across tenants, because SQL
-- Server applies a security policy to every user -- sysadmin, db_owner and dbo
-- included. Measured rather than assumed: connected as sa with no session
-- context, a table holding three rows reads as empty.
--
-- So SQL Server had no administrative access path at all. Not for a support
-- query, not for a cross-tenant backfill, and not for test teardown -- which is
-- where it surfaced. CleanupMSSQLTestData issues DELETE on a plain pool, the
-- predicate hides every row it means to delete, and it silently removes
-- nothing; rows then accumulate across tests until fixtures collide on a
-- primary key (IMPROVEMENT-PLAN 2.71).
--
-- PostgreSQL never had this problem, and not because its policies are weaker.
-- A superuser bypasses RLS unconditionally there, which is why the whole
-- argument in 005_app_role.sql is about keeping the *application* out of that
-- exemption: cleat_app is NOSUPERUSER, NOBYPASSRLS, and owns nothing, so it is
-- subject to the policies unconditionally. That design has two halves, and SQL
-- Server can only inherit one of them. The privileged half has to be written
-- into the predicate here, because the predicate is the only place an exemption
-- can live.
--
-- What this deliberately is not
-- -----------------------------
-- Not a sentinel value in SESSION_CONTEXT. A magic tenant_id would be assumable
-- by anything that can call sp_set_session_context -- which is the application
-- itself, on every connection it opens, so a single bad code path or an
-- injected value would turn tenant isolation off. Role membership is granted
-- once by a DBA and cannot be assumed by a connection at runtime.
--
-- The role ships with no members. Until a deployment adds one the predicate
-- behaves exactly as it did before: IS_ROLEMEMBER returns 0 for every existing
-- principal, and `0 = 1` leaves the session-context test as the only way
-- through. Two properties of SQL Server make it hard to acquire by accident,
-- both verified against 2022:
--
--   * db_owner does not confer it. sa reads IS_ROLEMEMBER('cleat_admin') = 0
--     and stays filtered, so the common "the application connects as sa"
--     deployment does not silently lose tenant isolation.
--   * dbo cannot be added to it. SQL Server refuses
--     `ALTER ROLE cleat_admin ADD MEMBER dbo` outright -- "Cannot use the
--     special principal 'dbo'" -- so membership requires a user someone created
--     on purpose.
--
-- Granting it is a deployment decision, deliberately not made in a committed
-- file, mirroring 005_app_role.sql's refusal to commit a credential:
--
--   CREATE LOGIN cleat_admin_login WITH PASSWORD = '...';
--   CREATE USER  cleat_admin_login FOR LOGIN cleat_admin_login;
--   ALTER ROLE   cleat_admin ADD MEMBER cleat_admin_login;
--
-- What it costs
-- -------------
-- Measured on 2022, 50k rows, 300 queries, two interleaved rounds: point
-- lookups are unchanged (~350 us either way, dominated by the round trip), and
-- a full scan of a filtered table goes from ~3.4 ms to ~4.1 ms -- about 20%.
-- IS_ROLEMEMBER is evidently not folded to a per-query constant, so the cost
-- tracks rows scanned (~14 ns/row). Two rewrites were tried and both were
-- worse: putting the role test first costs 33%, and wrapping it in a scalar
-- subquery costs 166%. The form below is the cheapest of the three, so this
-- does not need re-measuring. See IMPROVEMENT-PLAN 3.37.
--
-- Why the policies are dropped and recreated below
-- ------------------------------------------------
-- fn_tenant_filter is bound by the seven security policies that use it, and
-- SQL Server refuses to ALTER a function while a policy references it
-- ("Cannot ALTER 'dbo.fn_tenant_filter' because it is being referenced by
-- object 'TenantFilter_Defs'"). The runner executes each migration inside one
-- transaction (migration/runner.go), so the drop, the redefinition and the
-- recreation commit or roll back together -- there is no committed state in
-- which these tables exist without their filter.
-- ===========================================================================

-- The role first: fn_tenant_filter below names it, and IS_ROLEMEMBER on a role
-- that does not exist returns NULL rather than 0. `NULL = 1` is UNKNOWN, which
-- filters rather than admits, so the order is not load-bearing for safety --
-- but a predicate referring to a principal that exists is easier to reason
-- about than one relying on three-valued logic.
IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'cleat_admin' AND type = 'R')
    CREATE ROLE cleat_admin;
GO

-- Release the function. Guarded individually: a database may predate any one of
-- these policies.
IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Defs')
    DROP SECURITY POLICY dbo.TenantFilter_Defs;
IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Instances')
    DROP SECURITY POLICY dbo.TenantFilter_Instances;
IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_EventHistory')
    DROP SECURITY POLICY dbo.TenantFilter_EventHistory;
IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Signals')
    DROP SECURITY POLICY dbo.TenantFilter_Signals;
IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Schedules')
    DROP SECURITY POLICY dbo.TenantFilter_Schedules;
IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Tags')
    DROP SECURITY POLICY dbo.TenantFilter_Tags;
IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Routing')
    DROP SECURITY POLICY dbo.TenantFilter_Routing;
GO

-- The predicate. The session-context test is unchanged and still comes first;
-- the role membership is an additional way through, not a replacement.
CREATE OR ALTER FUNCTION dbo.fn_tenant_filter(@tenant_id UNIQUEIDENTIFIER)
RETURNS TABLE
WITH SCHEMABINDING
AS
RETURN SELECT 1 AS access
    WHERE @tenant_id = CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER)
       OR IS_ROLEMEMBER(N'cleat_admin') = 1;
GO

-- Rebind, on the same seven tables and in the same shape as 001_schema.sql.
CREATE SECURITY POLICY dbo.TenantFilter_Defs
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_defs
    WITH (STATE = ON);

GO
CREATE SECURITY POLICY dbo.TenantFilter_Instances
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_instances
    WITH (STATE = ON);

GO
CREATE SECURITY POLICY dbo.TenantFilter_EventHistory
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.event_history
    WITH (STATE = ON);

GO
CREATE SECURITY POLICY dbo.TenantFilter_Signals
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_signals
    WITH (STATE = ON);

GO
CREATE SECURITY POLICY dbo.TenantFilter_Schedules
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_schedules
    WITH (STATE = ON);

GO
CREATE SECURITY POLICY dbo.TenantFilter_Tags
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_tags
    WITH (STATE = ON);

GO
CREATE SECURITY POLICY dbo.TenantFilter_Routing
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_routing
    WITH (STATE = ON);
