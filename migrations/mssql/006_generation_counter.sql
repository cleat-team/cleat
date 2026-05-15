-- Migration 006: Add generation counter column to workflow_instances.
-- The generation column is a concurrency guard incremented on every claim.
-- It prevents stale workers from modifying workflows that have been re-assigned.
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('workflow_instances') AND name = 'generation')
    ALTER TABLE workflow_instances ADD generation BIGINT NOT NULL DEFAULT 0;
