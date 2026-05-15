-- Migration 006: Add generation counter column to workflow_instances.
-- The generation column is a concurrency guard incremented on every claim.
-- It prevents stale workers from modifying workflows that have been re-assigned.
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS generation BIGINT NOT NULL DEFAULT 0;
