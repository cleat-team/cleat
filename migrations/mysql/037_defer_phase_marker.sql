-- cleat migration 037 (mysql): a workflow can owe a defer phase
--
-- MySQL half of migrations/postgres/038_defer_phase_marker.sql, which carries
-- the rationale. D6 in tiers.yaml, IMPROVEMENT-PLAN 3.75 step 1.
--
-- Note for anyone editing this file: no comment here may contain a semicolon.
-- migration/runner.go splits MySQL migrations on the semicolon character with
-- no comment awareness, so one inside a leading-dash comment cuts the file
-- mid-sentence and the remainder is handed to MySQL as SQL. See
-- IMPROVEMENT-PLAN 3.13.
--
-- Two differences from the PostgreSQL file, both forced:
--
--   * The adds are guarded through information_schema and dynamic SQL, because
--     MySQL has no ADD COLUMN IF NOT EXISTS. Without the guard, re-running
--     against a database whose schema was created by engine/testutil -- which
--     builds the table with the columns already present -- fails with error
--     1060 rather than doing nothing.
--
--   * The index is not partial, because MySQL has no such thing. It leads with
--     pending_terminal_status precisely because the overwhelming majority of
--     rows have that column NULL, which a B-tree stores compactly and skips
--     cheaply, and it is only ever consulted by the reaper.

SET @add_pending_terminal_status := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_instances ADD COLUMN pending_terminal_status VARCHAR(32) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_instances'
      AND COLUMN_NAME = 'pending_terminal_status'
);
PREPARE add_pending_terminal_status FROM @add_pending_terminal_status;
EXECUTE add_pending_terminal_status;
DEALLOCATE PREPARE add_pending_terminal_status;

SET @add_defer_phase_deadline := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_instances ADD COLUMN defer_phase_deadline TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_instances'
      AND COLUMN_NAME = 'defer_phase_deadline'
);
PREPARE add_defer_phase_deadline FROM @add_defer_phase_deadline;
EXECUTE add_defer_phase_deadline;
DEALLOCATE PREPARE add_defer_phase_deadline;

SET @add_defer_phase_idx := (
    SELECT IF(COUNT(*) = 0,
        'CREATE INDEX idx_workflow_instances_defer_phase ON workflow_instances (pending_terminal_status, defer_phase_deadline)',
        'DO 0')
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_instances'
      AND INDEX_NAME = 'idx_workflow_instances_defer_phase'
);
PREPARE add_defer_phase_idx FROM @add_defer_phase_idx;
EXECUTE add_defer_phase_idx;
DEALLOCATE PREPARE add_defer_phase_idx;
