-- Migration 012: Add event threading columns (preparation for future use).
-- Columns added with defaults; PK remains (workflow_id, step).
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'thread_id')
    ALTER TABLE dbo.event_history ADD thread_id NVARCHAR(64) NOT NULL DEFAULT 'main';
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'local_step')
    ALTER TABLE dbo.event_history ADD local_step INT NOT NULL DEFAULT 0;
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'global_seq')
    ALTER TABLE dbo.event_history ADD global_seq BIGINT NOT NULL DEFAULT 0;
