-- cleat migration 021 (mysql): schedule timezone
--
-- See migrations/postgres/021_schedule_timezone.sql for why this column
-- exists and why the default is 'UTC' rather than the server's zone. The
-- short version: a cron expression is a statement about a wall clock, and
-- until this column existed the worker evaluated it in the worker PROCESS's
-- local zone, so one row meant different instants on different machines.
--
-- MySQL has no ADD COLUMN IF NOT EXISTS, so this uses the prepared-statement
-- guard that 020_event_intent.sql established: look the column up in
-- information_schema and prepare either the real ALTER or a no-op.
--
-- VARCHAR(64), not TEXT: the longest IANA zone name in tzdata is well under
-- that ('America/Argentina/ComodRivadavia' is 32), and a bounded column can
-- carry an index if one is ever wanted, which a TEXT column on MySQL cannot
-- without a prefix length.

SET @add_timezone := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_schedules ADD COLUMN timezone VARCHAR(64) NOT NULL DEFAULT ''UTC''',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_schedules'
      AND COLUMN_NAME = 'timezone'
);
PREPARE add_timezone FROM @add_timezone;
EXECUTE add_timezone;
DEALLOCATE PREPARE add_timezone;
