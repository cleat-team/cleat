-- Migration 007: Ensure event_count column exists on workflow_instances.
-- This column was added to the 001 migration after initial deployments,
-- so existing databases may not have it. This migration is idempotent.
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS event_count BIGINT NOT NULL DEFAULT 0;
