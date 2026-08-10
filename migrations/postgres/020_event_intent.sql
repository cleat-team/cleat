-- cleat migration 020 (postgres): write-ahead call intent
--
-- IMPROVEMENT-PLAN 1.4 phase D; design in docs/durable-call-intent-design.md §5.
--
-- A durable call dispatches the external request and *then* persists the
-- outcome. A crash between those two writes loses the outcome, so replay makes
-- the call again -- a duplicated real-world side effect, produced silently.
-- This column is what lets replay know a call was in flight when the process
-- died.
--
-- An event is PENDING iff intent_at IS NOT NULL AND checksum IS NULL.
--
-- Why a dedicated column rather than a sentinel in `error`
-- -------------------------------------------------------
-- The deleted implementation wrote a sentinel string into event_history.error.
-- Every completion path in the engine upserts with
--
--     ON CONFLICT (workflow_id, step) DO UPDATE ... WHERE response = '' AND error IS NULL
--
-- so a row whose error held the sentinel could never be completed: the
-- completion was a silent no-op and the row stayed pending forever, making
-- every subsequent replay of that workflow report [AMBIGUOUS]. Keeping `error`
-- meaning only "the call failed" removes that by construction.
--
-- Why checksum IS NULL is part of the definition
-- ---------------------------------------------
-- A pending row is by definition incomplete, so there is nothing stable to
-- checksum yet -- the response has not been written. Verification skips rows
-- with no checksum already (see VerifyWorkflowEvents), so a pending row is
-- passed over rather than reported as corrupt. The completing UPDATE writes
-- the checksum and clears intent_at in the same statement, so the two can
-- never disagree.
--
-- No backfill. Nothing has ever written the old sentinel in any deployment, so
-- there are no rows to migrate: every existing event is complete and gets
-- intent_at = NULL, which is what the column defaults to.

ALTER TABLE event_history ADD COLUMN IF NOT EXISTS intent_at TIMESTAMPTZ NULL;

-- Finding pending events is a recovery-path query, not a hot one, but it runs
-- against the largest table in the schema. The partial index keeps it
-- proportional to the number of in-flight calls rather than to history size,
-- and costs nothing on the write path for the complete rows that are almost
-- all of the table.
CREATE INDEX IF NOT EXISTS idx_event_history_pending
    ON event_history (workflow_id, step)
    WHERE intent_at IS NOT NULL AND checksum IS NULL;
