-- cleat migration 041 (mssql): a workflow can owe a defer phase
--
-- SQL Server half of migrations/postgres/038_defer_phase_marker.sql, which
-- carries the rationale. D6 in tiers.yaml, IMPROVEMENT-PLAN 3.75 step 1.
--
-- Closest of the three to the PostgreSQL file: SQL Server has filtered indexes,
-- so the reaper's index is partial here too and excludes the overwhelming
-- majority of rows, which owe no defer phase. Only the guarded-add idiom
-- differs, since there is no ADD COLUMN IF NOT EXISTS.
--
-- workflow_instances carries a FILTER PREDICATE security policy
-- (migrations/mssql/012_admin_role.sql). Adding a column does not interact with
-- it -- the predicate is on tenant_id -- so there is no policy to drop and
-- recreate, unlike the primary-key changes in 038-040.

IF NOT EXISTS (
    SELECT 1 FROM sys.columns
    WHERE object_id = OBJECT_ID(N'dbo.workflow_instances')
      AND name = N'pending_terminal_status'
)
    ALTER TABLE dbo.workflow_instances ADD pending_terminal_status NVARCHAR(32) NULL;
GO

IF NOT EXISTS (
    SELECT 1 FROM sys.columns
    WHERE object_id = OBJECT_ID(N'dbo.workflow_instances')
      AND name = N'defer_phase_deadline'
)
    ALTER TABLE dbo.workflow_instances ADD defer_phase_deadline DATETIME2 NULL;
GO

IF NOT EXISTS (
    SELECT 1 FROM sys.indexes
    WHERE object_id = OBJECT_ID(N'dbo.workflow_instances')
      AND name = N'idx_workflow_instances_defer_phase_deadline'
)
    CREATE INDEX idx_workflow_instances_defer_phase_deadline
        ON dbo.workflow_instances (defer_phase_deadline)
        WHERE pending_terminal_status IS NOT NULL;
GO
