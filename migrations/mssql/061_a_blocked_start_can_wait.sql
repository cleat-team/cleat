-- cleat migration 061 (mssql): record the concurrency key a run wants
--
-- See migrations/postgres/058 for the reasoning. cleat#1186.
--
-- SQL Server DOES have filtered indexes, so this arm keeps the partial form the
-- PostgreSQL one uses. The MySQL arm cannot and says so.
IF NOT EXISTS (SELECT 1 FROM sys.columns
               WHERE object_id = OBJECT_ID('dbo.workflow_instances')
                 AND name = 'concurrency_key')
BEGIN
    ALTER TABLE dbo.workflow_instances ADD concurrency_key NVARCHAR(255) NULL, concurrency_key_hash VARBINARY(32) NULL;
END
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes
               WHERE name = 'idx_instances_concurrency_key'
                 AND object_id = OBJECT_ID('dbo.workflow_instances'))
BEGIN
    EXEC('CREATE INDEX idx_instances_concurrency_key
              ON dbo.workflow_instances (tenant_id, concurrency_key_hash)
              WHERE concurrency_key_hash IS NOT NULL');
END
GO

