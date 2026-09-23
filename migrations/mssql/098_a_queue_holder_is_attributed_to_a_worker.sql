-- cleat migration 098 (mssql): a queue holder is attributed to a worker
--
-- See migrations/postgres/099_a_queue_holder_is_attributed_to_a_worker.sql for
-- the full reasoning: cleat#1917, second piece, nullable worker_id with no
-- backfill, moved (not duplicated) when a parked run wakes onto a different
-- worker. This header records only what differs here.
--
-- NVARCHAR(255), matching workflow_instances.assigned_to (001_schema.sql).
--
-- GUARDED, following 095/097's own precedent.

IF COL_LENGTH(N'dbo.queue_holders', N'worker_id') IS NULL
    ALTER TABLE dbo.queue_holders
        ADD worker_id NVARCHAR(255) NULL;
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_queue_holders_worker' AND object_id = OBJECT_ID(N'dbo.queue_holders'))
    CREATE INDEX idx_queue_holders_worker ON dbo.queue_holders(tenant_id, queue_name, worker_id, expires_at);
GO
