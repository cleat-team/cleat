-- cleat migration 048 (mssql): a continuation records the run it came from
--
-- SQL Server half of migrations/postgres/045_continue_as_new_records_its_predecessor.sql,
-- which carries the rationale for the column and for why it is NOT
-- parent_workflow_id. cleat#826.
--
-- Closest of the three to the PostgreSQL file: SQL Server has filtered indexes,
-- so the forward-walk index is partial here too. Only the guarded-add idiom
-- differs, since there is no ADD COLUMN IF NOT EXISTS.
--
-- workflow_instances carries a FILTER PREDICATE security policy
-- (migrations/mssql/012_admin_role.sql). Adding a column does not interact with
-- it -- the predicate is on tenant_id -- so there is no policy to drop and
-- recreate. Same reasoning as 041_defer_phase_marker.sql, and worth restating
-- because 038-040 DID have to, and the difference is what the column touches.
--
-- continued_from is NVARCHAR(255) to match workflow_instances.id.

IF NOT EXISTS (
    SELECT 1 FROM sys.columns
    WHERE object_id = OBJECT_ID(N'dbo.workflow_instances')
      AND name = N'continued_from'
)
    ALTER TABLE dbo.workflow_instances ADD continued_from NVARCHAR(255) NULL;
GO

IF NOT EXISTS (
    SELECT 1 FROM sys.indexes
    WHERE object_id = OBJECT_ID(N'dbo.workflow_instances')
      AND name = N'idx_workflow_instances_continued_from'
)
    CREATE INDEX idx_workflow_instances_continued_from
        ON dbo.workflow_instances (continued_from)
        WHERE continued_from IS NOT NULL;
GO
