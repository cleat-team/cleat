-- cleat migration 066 (mssql): an update name is reusable, so the row needs
-- an identity that is not its name.
--
-- An update name is reusable. cleat#1416.
--
-- See migrations/postgres/068 for why, why the identity is a new column rather
-- than promise_id, and why the backfill copies update_name. The reasoning is
-- identical on all three dialects and is written out once, there.
--
-- Guarded on sys.* rather than IF NOT EXISTS, which SQL Server does not have on
-- ALTER TABLE, because SetupFullSchema re-applies the whole set in tests.

IF NOT EXISTS (SELECT 1 FROM sys.columns
               WHERE object_id = OBJECT_ID(N'dbo.workflow_update_requests')
                 AND name = N'request_id')
    ALTER TABLE dbo.workflow_update_requests ADD request_id NVARCHAR(255) NULL;
GO

UPDATE dbo.workflow_update_requests SET request_id = update_name WHERE request_id IS NULL;
GO

IF EXISTS (SELECT 1 FROM sys.columns
           WHERE object_id = OBJECT_ID(N'dbo.workflow_update_requests')
             AND name = N'request_id' AND is_nullable = 1)
    ALTER TABLE dbo.workflow_update_requests ALTER COLUMN request_id NVARCHAR(255) NOT NULL;
GO

-- The primary key is replaced only if it is still the one keyed on update_name.
-- Re-running must not drop the new key and rebuild it: that is a table rebuild
-- on every SetupFullSchema.
IF EXISTS (SELECT 1
           FROM sys.index_columns ic
           JOIN sys.indexes i ON i.object_id = ic.object_id AND i.index_id = ic.index_id
           JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
           WHERE i.object_id = OBJECT_ID(N'dbo.workflow_update_requests')
             AND i.is_primary_key = 1
             AND c.name = N'update_name')
BEGIN
    ALTER TABLE dbo.workflow_update_requests DROP CONSTRAINT pk_workflow_update_requests;
    ALTER TABLE dbo.workflow_update_requests
        ADD CONSTRAINT pk_workflow_update_requests PRIMARY KEY (workflow_id, request_id);
END
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes
               WHERE name = N'idx_update_requests_pending_name'
                 AND object_id = OBJECT_ID(N'dbo.workflow_update_requests'))
    CREATE INDEX idx_update_requests_pending_name
        ON dbo.workflow_update_requests (workflow_id, update_name, status);
GO
