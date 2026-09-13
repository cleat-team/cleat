-- cleat migration 062 (mysql): an update name is reusable, so the row needs
-- an identity that is not its name.
--
-- An update name is reusable. cleat#1416.
--
-- See migrations/postgres/068 for why, why the identity is a new column rather
-- than promise_id, and why the backfill copies update_name. The reasoning is
-- identical on all three dialects and is written out once, there.
--
-- MySQL-specific note: workflow_id carries a FOREIGN KEY, and before this
-- migration the only index leading with it was the primary key being replaced.
-- DROP and ADD happen in ONE ALTER so the column is never without one --
-- separated, MySQL refuses the drop with "Cannot drop index 'PRIMARY': needed
-- in a foreign key constraint". idx_update_requests_pending (workflow_id,
-- status) would in fact also satisfy it, but relying on a second index to keep
-- the first droppable is a coupling nothing states.
--
-- No DEFAULT (UUID()) on the column, and this dialect is the reason there is no
-- default on any of them. MySQL refuses it outright under statement-based
-- binlogging -- ERROR 1674, "unsafe because it uses a system function that may
-- return a different value on the replica" -- so the migration itself fails.
-- See migrations/postgres/068 for the full reasoning.
--
-- Guarded on information_schema rather than IF NOT EXISTS, which MySQL 8 does
-- not have for ADD COLUMN, because SetupFullSchema re-applies the whole set in
-- tests.

SET @col := (SELECT COUNT(*) FROM information_schema.COLUMNS
             WHERE TABLE_SCHEMA = DATABASE()
               AND TABLE_NAME = 'workflow_update_requests'
               AND COLUMN_NAME = 'request_id');
SET @sql := IF(@col = 0,
    'ALTER TABLE workflow_update_requests ADD COLUMN request_id VARCHAR(255) NULL',
    'DO 0');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

UPDATE workflow_update_requests SET request_id = update_name WHERE request_id IS NULL;

SET @nullable := (SELECT COUNT(*) FROM information_schema.COLUMNS
                  WHERE TABLE_SCHEMA = DATABASE()
                    AND TABLE_NAME = 'workflow_update_requests'
                    AND COLUMN_NAME = 'request_id'
                    AND IS_NULLABLE = 'YES');
SET @sql := IF(@nullable = 1,
    'ALTER TABLE workflow_update_requests MODIFY request_id VARCHAR(255) NOT NULL',
    'DO 0');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @keyed_on_name := (SELECT COUNT(*) FROM information_schema.KEY_COLUMN_USAGE
                       WHERE TABLE_SCHEMA = DATABASE()
                         AND TABLE_NAME = 'workflow_update_requests'
                         AND CONSTRAINT_NAME = 'PRIMARY'
                         AND COLUMN_NAME = 'update_name');
SET @sql := IF(@keyed_on_name = 1,
    'ALTER TABLE workflow_update_requests
        DROP PRIMARY KEY,
        ADD PRIMARY KEY (workflow_id, request_id)',
    'DO 0');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @named_idx := (SELECT COUNT(*) FROM information_schema.STATISTICS
                   WHERE TABLE_SCHEMA = DATABASE()
                     AND TABLE_NAME = 'workflow_update_requests'
                     AND INDEX_NAME = 'idx_update_requests_pending_name');
SET @sql := IF(@named_idx = 0,
    'CREATE INDEX idx_update_requests_pending_name
        ON workflow_update_requests (workflow_id, update_name, status)',
    'DO 0');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
