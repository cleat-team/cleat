-- cleat event history compaction migration (T-SQL for SQL Server 2017+ / Azure SQL Database)
-- Adds columns for tracking automatic compaction of event_history for long-running workflows.
-- Compaction prunes old events into a checkpoint + recent tail, reducing storage and replay time.
--
-- Idempotent: all statements use IF NOT EXISTS / IF EXISTS where applicable.

-- ===========================================================================
-- compaction_state: JSONB capturing the minimal replay state needed after compaction
-- (pending defers, open children, query state, call results, event type sequence).
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'compaction_state')
    ALTER TABLE dbo.workflow_instances ADD compaction_state NVARCHAR(MAX) NULL;

-- ===========================================================================
-- compacted_at: timestamp of the last compaction for this workflow.
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'compacted_at')
    ALTER TABLE dbo.workflow_instances ADD compacted_at DATETIMEOFFSET NULL;

-- ===========================================================================
-- compaction_step: the step number up to which events have been compacted.
-- Events with step < compaction_step have been deleted; events >= compaction_step
-- are in the tail.
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'compaction_step')
    ALTER TABLE dbo.workflow_instances ADD compaction_step INT NULL;
