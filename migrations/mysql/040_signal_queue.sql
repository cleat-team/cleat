-- cleat migration 040 (mysql): a signal is a delivery, not a row
--
-- MySQL half of migrations/postgres/041_signal_queue.sql, which carries the
-- rationale. IMPROVEMENT-PLAN 3.215.
--
-- Note for anyone editing this file: no comment here may contain a semicolon.
-- migration/runner.go splits MySQL migrations on the semicolon character with
-- no comment awareness, so one inside a leading-dash comment cuts the file
-- mid-sentence and the remainder is handed to MySQL as SQL. See
-- IMPROVEMENT-PLAN 3.13.
--
-- Two differences from the PostgreSQL file, one forced and one ordering.
--
--   * The adds are guarded through information_schema and dynamic SQL, because
--     MySQL has no ADD COLUMN IF NOT EXISTS and no CREATE INDEX IF NOT EXISTS.
--
--   * The index MUST be created BEFORE the primary key is dropped, and this is
--     the only dialect where the order matters. workflow_signals has a foreign
--     key on workflow_id, InnoDB requires an index whose leftmost column is the
--     referencing column, and until now the primary key was providing it.
--     Dropping the primary key with nothing else covering workflow_id fails
--     with error 1553, "Cannot drop index ... needed in a foreign key
--     constraint". That is also why the index is two columns rather than
--     three: the id column does not exist yet at the moment the index has to.

SET @add_queue_idx := (
    SELECT IF(COUNT(*) = 0,
        'CREATE INDEX idx_workflow_signals_queue ON workflow_signals (workflow_id, signal_name)',
        'DO 0')
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_signals'
      AND INDEX_NAME = 'idx_workflow_signals_queue'
);
PREPARE add_queue_idx FROM @add_queue_idx;
EXECUTE add_queue_idx;
DEALLOCATE PREPARE add_queue_idx;

-- One ALTER for both actions. Split into two statements the table would sit
-- with no primary key in between, and a second AUTO_INCREMENT column cannot be
-- added to a table that already has one, so the guard has to cover both halves
-- as a unit anyway.
SET @swap_signal_key := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_signals DROP PRIMARY KEY, ADD COLUMN id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_signals'
      AND COLUMN_NAME = 'id'
);
PREPARE swap_signal_key FROM @swap_signal_key;
EXECUTE swap_signal_key;
DEALLOCATE PREPARE swap_signal_key;
