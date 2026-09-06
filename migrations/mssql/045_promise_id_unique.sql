-- cleat migration 045 (mssql): a promise is addressed by its own id
--
-- SQL Server half of migrations/postgres/042_promise_id_unique.sql, which
-- carries the rationale. IMPROVEMENT-PLAN 3.235, cleat#813.

IF NOT EXISTS (
    SELECT 1 FROM sys.indexes
    WHERE name = 'idx_promises_id_unique'
      AND object_id = OBJECT_ID('dbo.workflow_promises')
)
    CREATE UNIQUE INDEX idx_promises_id_unique
        ON dbo.workflow_promises(tenant_id, promise_id);
