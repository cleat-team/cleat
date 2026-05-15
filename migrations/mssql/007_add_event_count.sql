-- Migration 007: Ensure event_count column exists on workflow_instances.
-- This column was added to the 001 migration after initial deployments,
-- so existing databases may not have it. This migration is idempotent.
IF NOT EXISTS (SELECT * FROM sys.columns WHERE object_id = OBJECT_ID(N'workflow_instances') AND name = 'event_count')
BEGIN
    ALTER TABLE workflow_instances ADD event_count BIGINT NOT NULL DEFAULT 0;
END
