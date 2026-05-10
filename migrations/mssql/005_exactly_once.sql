-- cleat exactly-once semantics migration (T-SQL for SQL Server 2017+ / Azure SQL Database)
-- Adds idempotency key support for exactly-once workflow start semantics.
-- When a workflow is started with an Idempotency-Key header, the key is stored
-- in this table so that retried API calls return the existing workflow ID instead
-- of creating a duplicate.
--
-- Keys automatically expire after 7 days and are periodically cleaned up.
--
-- Idempotent: all statements use IF NOT EXISTS / IF EXISTS where applicable.

-- ===========================================================================
-- Table: idempotency_keys
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.idempotency_keys') AND type = N'U')
CREATE TABLE dbo.idempotency_keys (
    key_hash    VARBINARY(32)    NOT NULL,
    workflow_id NVARCHAR(MAX)    NOT NULL,
    result      NVARCHAR(MAX)    NULL,
    error_msg   NVARCHAR(MAX)    NULL,
    created_at  DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
    expires_at  DATETIMEOFFSET   NOT NULL DEFAULT DATEADD(DAY, 7, SYSUTCDATETIME()),
    CONSTRAINT pk_idempotency_keys PRIMARY KEY (key_hash),
    CONSTRAINT ck_idempotency_keys_result CHECK (result IS NULL OR ISJSON(result) = 1)
);

-- ===========================================================================
-- Index for looking up by workflow_id
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_idempotency_workflow_id' AND object_id = OBJECT_ID(N'dbo.idempotency_keys'))
    CREATE INDEX idx_idempotency_workflow_id ON dbo.idempotency_keys(workflow_id);

-- ===========================================================================
-- Index for expiration cleanup queries
-- Note: T-SQL filtered indexes cannot use non-deterministic functions like
-- SYSUTCDATETIME() in the predicate, so we use a regular index on expires_at.
-- Cleanup queries filter WHERE expires_at < SYSUTCDATETIME() at query time.
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_idempotency_expires' AND object_id = OBJECT_ID(N'dbo.idempotency_keys'))
    CREATE INDEX idx_idempotency_expires ON dbo.idempotency_keys(expires_at);
