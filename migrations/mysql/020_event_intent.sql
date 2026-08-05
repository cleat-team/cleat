-- cleat migration 020 (mysql): write-ahead call intent
--
-- MySQL half of migrations/postgres/020_event_intent.sql, which carries the
-- rationale. An event is PENDING iff intent_at IS NOT NULL AND checksum IS NULL.
--
-- Note for anyone editing this file: no comment here may contain a semicolon.
-- migration/runner.go splits MySQL migrations on the semicolon character with
-- no comment awareness, so one inside a leading-dash comment cuts the file
-- mid-sentence and the remainder of the sentence is handed to MySQL as SQL.
-- That is what makes migrations/mysql/001_schema.sql unappliable today, and it
-- is recorded as IMPROVEMENT-PLAN 3.13.
--
-- MySQL 8.0 has no ADD COLUMN IF NOT EXISTS and no partial index, so this file
-- differs from the PostgreSQL one in two ways worth reading rather than
-- skimming:
--
--   * The add is guarded through information_schema and dynamic SQL. The
--     runner applies every statement of a migration inside one transaction on
--     one connection, so the session variable and the prepared statement below
--     see each other. Without the guard, re-running against a database whose
--     schema was created by engine/testutil -- which builds the table with the
--     column already present -- fails with error 1060 rather than doing
--     nothing.
--
--   * The index is not partial, because MySQL has no such thing. It leads with
--     intent_at precisely because the overwhelming majority of rows have that
--     column NULL, which a B-tree stores compactly and skips cheaply, and it
--     is only ever consulted on the recovery path.

SET @add_intent_at := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE event_history ADD COLUMN intent_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'event_history'
      AND COLUMN_NAME = 'intent_at'
);
PREPARE add_intent_at FROM @add_intent_at;
EXECUTE add_intent_at;
DEALLOCATE PREPARE add_intent_at;

SET @add_pending_idx := (
    SELECT IF(COUNT(*) = 0,
        'CREATE INDEX idx_event_history_pending ON event_history (intent_at, workflow_id, step)',
        'DO 0')
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'event_history'
      AND INDEX_NAME = 'idx_event_history_pending'
);
PREPARE add_pending_idx FROM @add_pending_idx;
EXECUTE add_pending_idx;
DEALLOCATE PREPARE add_pending_idx;
