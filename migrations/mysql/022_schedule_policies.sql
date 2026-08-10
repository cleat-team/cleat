-- cleat migration 022 (mysql): misfire and overlap policy
--
-- See migrations/postgres/022_schedule_policies.sql for what these mean and
-- why the defaults are what they are.
--
-- MySQL has no ADD COLUMN IF NOT EXISTS, so this uses the prepared-statement
-- guard established by 020 and 021.

SET @add_misfire := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_schedules ADD COLUMN misfire_policy VARCHAR(16) NOT NULL DEFAULT ''catch_up''',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_schedules' AND COLUMN_NAME = 'misfire_policy'
);
PREPARE add_misfire FROM @add_misfire; EXECUTE add_misfire; DEALLOCATE PREPARE add_misfire;

SET @add_limit := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_schedules ADD COLUMN catch_up_limit INT NOT NULL DEFAULT 60',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_schedules' AND COLUMN_NAME = 'catch_up_limit'
);
PREPARE add_limit FROM @add_limit; EXECUTE add_limit; DEALLOCATE PREPARE add_limit;

SET @add_overlap := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_schedules ADD COLUMN overlap_policy VARCHAR(16) NOT NULL DEFAULT ''allow''',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_schedules' AND COLUMN_NAME = 'overlap_policy'
);
PREPARE add_overlap FROM @add_overlap; EXECUTE add_overlap; DEALLOCATE PREPARE add_overlap;

SET @add_lastrun := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_schedules ADD COLUMN last_run_id VARCHAR(255) NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_schedules' AND COLUMN_NAME = 'last_run_id'
);
PREPARE add_lastrun FROM @add_lastrun; EXECUTE add_lastrun; DEALLOCATE PREPARE add_lastrun;
