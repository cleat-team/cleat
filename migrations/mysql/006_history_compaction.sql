-- cleat MySQL migration 006: event history compaction
-- Adds columns for tracking automatic compaction of event_history for
-- long-running workflows. Compaction prunes old events into a checkpoint
-- plus recent tail, reducing storage and replay time.
--
-- MySQL differences from PostgreSQL:
--   - JSONB becomes JSON
--   - TIMESTAMPTZ becomes TIMESTAMP(6)
--   - ADD COLUMN IF NOT EXISTS becomes ADD COLUMN with safety comment
--   - Idempotent: verify column absence before running

-- compaction_state: JSON capturing the minimal replay state needed after
-- compaction (pending defers, open children, query state, call results,
-- event type sequence).
-- ensure column does not exist before running
ALTER TABLE workflow_instances ADD COLUMN compaction_state JSON;

-- compacted_at: timestamp of the last compaction for this workflow.
-- ensure column does not exist before running
ALTER TABLE workflow_instances ADD COLUMN compacted_at TIMESTAMP(6);

-- compaction_step: the step number up to which events have been compacted.
-- Events with step < compaction_step have been deleted; events >= compaction_step
-- are in the tail.
-- ensure column does not exist before running
ALTER TABLE workflow_instances ADD COLUMN compaction_step INTEGER;
