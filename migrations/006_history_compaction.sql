-- cleat event history compaction migration
-- Adds columns for tracking automatic compaction of event_history for long-running workflows.
-- Compaction prunes old events into a checkpoint + recent tail, reducing storage and replay time.
--
-- Idempotent: all statements use IF NOT EXISTS / IF EXISTS where applicable.

-- compaction_state: JSONB capturing the minimal replay state needed after compaction
-- (pending defers, open children, query state, call results, event type sequence).
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS compaction_state JSONB;

-- compacted_at: timestamp of the last compaction for this workflow.
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS compacted_at TIMESTAMPTZ;

-- compaction_step: the step number up to which events have been compacted.
-- Events with step < compaction_step have been deleted; events >= compaction_step are in the tail.
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS compaction_step INTEGER;
