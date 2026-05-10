-- cleat JSONB payload column migration (T-SQL for SQL Server 2017+ / Azure SQL Database)
-- Adds a JSON payload column to event_history for structured event-type-specific data.
-- Dual-write period: old columns + new payload column are both populated.
-- After all workflows have migrated, the old columns can be dropped.

-- ===========================================================================
-- Add payload column to event_history
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'payload')
    ALTER TABLE dbo.event_history ADD payload NVARCHAR(MAX) NULL;
