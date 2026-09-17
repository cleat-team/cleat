-- cleat migration 081 (mssql): schedules retire through disabled_at
--
-- cleat#1702, the LAST of the four conversions. See
-- migrations/postgres/089_schedules_retire_through_disabled_at.sql for the
-- reasoning: the polarity inversion this one conversion carries, why both
-- backfill directions are written, and why the backfill is an upper bound
-- rather than the exact value 087 could claim.
--
-- WHAT IS DIFFERENT HERE, AND IT IS THE ONE THING THAT CANNOT BE SKIPPED.
--
-- `dbo.workflow_schedules` carries a FILTER PREDICATE, from
-- `CREATE SECURITY POLICY dbo.TenantFilter_Schedules` (001_schema.sql:466,
-- restated in 012_admin_role.sql:141). A filter predicate EXEMPTS NOBODY --
-- not the table owner, not sysadmin -- so a connection with no
-- SESSION_CONTEXT(N'tenant_id') sees zero rows in this table. It does not
-- error. An UPDATE simply matches nothing and reports success.
--
-- That is how migration 080 was nearly shipped wrong, and the mistake is
-- recorded here because the check that missed it looked thorough: I asserted
-- `workflow_defs` had no policy, "verified against every ALTER SECURITY
-- POLICY" -- which cannot find one, because a policy is created by CREATE. The
-- discriminator is `sys.security_policies`, not the migration text:
--
--     SELECT p.name, t.name FROM sys.security_predicates sp
--       JOIN sys.security_policies p ON p.object_id = sp.object_id
--       JOIN sys.tables t ON t.object_id = sp.target_object_id;
--
-- So the policy is toggled OFF around the DML and back ON in a CATCH, exactly
-- as 080 does.
--
-- MEASURED, ON THE REAL UPGRADE PATH, WITH AND WITHOUT THE TOGGLE. A database
-- was bootstrapped at develop's schema (where `enabled` still exists), seeded
-- with one retired and one live schedule -- both proven visible first -- and
-- then converted:
--
--     with the toggle        probe-retired  disabled_at = 2026-09-17T13:17:37Z
--                            probe-live     disabled_at = NULL
--
--     toggle line removed    probe-retired  disabled_at = NULL      <-- LIVE
--                            probe-live     disabled_at = NULL
--
-- Both runs reported success and both dropped the column. Without the toggle
-- the retired schedule comes back from the upgrade as LIVE -- a schedule an
-- operator disabled starts firing, and nothing anywhere reports a problem. It
-- is not merely a no-op: the migration destroys the only record of the state it
-- failed to copy, so there is no second chance to notice and no way back.
--
-- That is why the toggle is the one line in this file that cannot be dropped as
-- boilerplate, and why the failure is worse than 080's would have been: 080
-- would have left a version uncollectable, which is inert. This changes what
-- the scheduler does.
--
-- WHY THERE IS NO FUNCTION HERE. admin.get_due_schedules() is a PostgreSQL
-- SECURITY DEFINER function; SQL Server's cross-tenant due read is a plain
-- SELECT in engine/mssql_schedules.go.
--
-- THE DEFAULT CONSTRAINT IS SYSTEM-NAMED. 001 declares `enabled BIT NOT NULL
-- DEFAULT 1` with an inline unnamed default, so SQL Server invented a name like
-- DF__workflow___enabl__<hash> and DROP COLUMN fails while it stands. It is
-- looked up in sys.default_constraints and dropped through sp_executesql on a
-- VARIABLE -- EXEC() will not parse a concatenation (measured in 080).
--
-- IDEMPOTENT. Everything naming `enabled` is guarded on COL_LENGTH, because on
-- a fresh database 001_schema.sql never created it.

-- ── Backfill, in both directions, while `enabled` is still the authority ─────

ALTER SECURITY POLICY dbo.TenantFilter_Schedules WITH (STATE = OFF);
BEGIN TRY
    -- Retired under the authority, unrecorded here. AN UPPER BOUND.
    IF COL_LENGTH(N'dbo.workflow_schedules', N'enabled') IS NOT NULL
        EXEC(N'UPDATE dbo.workflow_schedules
                  SET disabled_at = SYSUTCDATETIME()
                WHERE enabled = 0
                  AND disabled_at IS NULL');

    -- Live under the authority, so `disabled_at` must not say otherwise.
    -- Matches nothing in a shipped deployment; postgres/089 says why it is
    -- written anyway.
    IF COL_LENGTH(N'dbo.workflow_schedules', N'enabled') IS NOT NULL
        EXEC(N'UPDATE dbo.workflow_schedules
                  SET disabled_at = NULL
                WHERE enabled = 1
                  AND disabled_at IS NOT NULL');
END TRY
BEGIN CATCH
    ALTER SECURITY POLICY dbo.TenantFilter_Schedules WITH (STATE = ON);
    THROW;
END CATCH;
ALTER SECURITY POLICY dbo.TenantFilter_Schedules WITH (STATE = ON);
GO

-- ── The due-schedule index moves to the new column ──────────────────────────
--
-- Filtered, matching PostgreSQL: the scan never wants a retired row. MySQL has
-- no filtered indexes and keeps the column in the key instead.

IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_schedules_tenant_enabled' AND object_id = OBJECT_ID(N'dbo.workflow_schedules'))
    DROP INDEX idx_schedules_tenant_enabled ON dbo.workflow_schedules;
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_schedules_tenant_due' AND object_id = OBJECT_ID(N'dbo.workflow_schedules'))
    CREATE INDEX idx_schedules_tenant_due ON dbo.workflow_schedules(tenant_id, next_run_at) WHERE disabled_at IS NULL;
GO

-- ── The system-named default has to go before the column can ────────────────

IF COL_LENGTH(N'dbo.workflow_schedules', N'enabled') IS NOT NULL
BEGIN
    DECLARE @df sysname, @sql nvarchar(max);
    SELECT @df = dc.name
      FROM sys.default_constraints dc
      JOIN sys.columns c ON c.object_id = dc.parent_object_id
                        AND c.column_id = dc.parent_column_id
     WHERE dc.parent_object_id = OBJECT_ID(N'dbo.workflow_schedules')
       AND c.name = N'enabled';
    IF @df IS NOT NULL
    BEGIN
        -- sp_executesql on a VARIABLE, not EXEC on an expression: EXEC() takes
        -- a string literal or a variable and will not parse a concatenation,
        -- which fails with "Incorrect syntax near 'QUOTENAME'" (measured in 080).
        SET @sql = N'ALTER TABLE dbo.workflow_schedules DROP CONSTRAINT ' + QUOTENAME(@df);
        EXEC sp_executesql @sql;
    END
END
GO

-- ── And the column goes ──────────────────────────────────────────────────────

IF COL_LENGTH(N'dbo.workflow_schedules', N'enabled') IS NOT NULL
    ALTER TABLE dbo.workflow_schedules DROP COLUMN enabled;
GO
