-- cleat migration 030 (mysql): event_history.service/operation/request become nullable
--
-- Dialect parity fix. 001_schema.sql declared these three columns NOT NULL.
-- PostgreSQL declares them plain TEXT and SQL Server declares them NULL, so
-- MySQL was the only dialect asserting a constraint the engine does not honour.
--
-- The engine writes all three through nullStr in engine/store_events.go, which
-- maps the empty string to SQL NULL. Several event types legitimately leave
-- them empty -- EventTypeDefer engine/durablecalls.go, EventTypeChildWorkflow
-- engine/children.go, EventTypeAwaitSignals and EventTypeUpdateHandler -- so on
-- MySQL the engine has always been handing NULL to a NOT NULL column.
--
-- Why nobody noticed, and it is worth reading because the answer is not
-- "it never happened":
--
--   * The main write path is INSERT IGNORE, engine/mysql_events.go and
--     engine/mysql_store.go. MySQL downgrades error 1048 under IGNORE to a
--     warning and substitutes the implicit default, the empty string. That is
--     the same value the Go side started from, so it round-trips and the
--     checksum chain still matches. No corruption, no failure, no signal.
--
--   * WriteCallIntent in engine/store_intent.go is a plain INSERT with no
--     IGNORE. Service and operation are non-empty for any durable call because
--     validServiceName rejects otherwise, but the request body is validated
--     nowhere, so a write-ahead-intent operation carrying an empty request
--     fails on MySQL with error 1048 and succeeds on the other two dialects.
--     WriteAheadIntent is opt-in through WithWriteAheadIntentOps, which is why
--     this has not been hit.
--
--   * No test could see either case. engine/testutil/mysql_schema.go builds
--     these columns nullable, so every MySQL test in the repo runs against a
--     schema production never uses. Fixing that divergence is a separate
--     change and is the one that stops this recurring.
--
-- Aligning the schema to the other two dialects is preferred over teaching
-- nullStr to special-case three columns: the invariant worth holding is that
-- the three dialects accept the same writes, and a per-column exception list in
-- Go is a thing the next author has to remember.
--
-- Note for anyone editing this file: no comment here may contain a semicolon.
-- migration/runner.go splits MySQL migrations on the semicolon with no comment
-- awareness, so one inside a leading-dash comment cuts the file mid-sentence and
-- hands the remainder to MySQL as SQL. See 020_event_intent.sql.
--
-- MySQL has no ALTER COLUMN IF, and re-running MODIFY is harmless but rewrites
-- the table, so each change is guarded through information_schema in the same
-- style as 020. IS_NULLABLE is the guard rather than column existence: a
-- database built by engine/testutil already has these columns nullable and must
-- be left alone.

SET @relax_service := (
    SELECT IF(COUNT(*) = 1,
        'ALTER TABLE event_history MODIFY COLUMN service VARCHAR(255) NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'event_history'
      AND COLUMN_NAME = 'service'
      AND IS_NULLABLE = 'NO'
);
PREPARE relax_service FROM @relax_service;
EXECUTE relax_service;
DEALLOCATE PREPARE relax_service;

SET @relax_operation := (
    SELECT IF(COUNT(*) = 1,
        'ALTER TABLE event_history MODIFY COLUMN operation VARCHAR(255) NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'event_history'
      AND COLUMN_NAME = 'operation'
      AND IS_NULLABLE = 'NO'
);
PREPARE relax_operation FROM @relax_operation;
EXECUTE relax_operation;
DEALLOCATE PREPARE relax_operation;

SET @relax_request := (
    SELECT IF(COUNT(*) = 1,
        'ALTER TABLE event_history MODIFY COLUMN request LONGTEXT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'event_history'
      AND COLUMN_NAME = 'request'
      AND IS_NULLABLE = 'NO'
);
PREPARE relax_request FROM @relax_request;
EXECUTE relax_request;
DEALLOCATE PREPARE relax_request;
