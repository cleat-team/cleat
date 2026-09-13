-- cleat migration 063 (mysql): a schedule carries the idempotency key that
-- created it, so a retry can be told from a name collision.
--
-- POST /api/schedules honours Idempotency-Key. cleat#1495.
--
-- See migrations/postgres/072 for why the key lives on the schedule row rather
-- than in idempotency_keys, why it is nullable and not backfilled, and why it is
-- scoped by tenant. The reasoning is identical on all three dialects and is
-- written out once, there.
--
-- MySQL-specific note: the index is a plain UNIQUE KEY with no filter, and it is
-- correct here for a reason that does NOT hold on SQL Server. MySQL's unique
-- indexes permit any number of NULLs, so every keyless schedule coexists under
-- one index without a WHERE clause -- which MySQL does not have for indexes
-- anyway. PostgreSQL behaves the same way and still writes the filter, for the
-- reader; SQL Server does not, and needs a filtered index or it would refuse the
-- second keyless schedule outright. See migrations/mssql/067.
--
-- VARCHAR(255) rather than TEXT because the column is indexed, and InnoDB cannot
-- index a TEXT without a prefix length -- a prefix would make the uniqueness
-- constraint hold over a truncation of the key rather than the key, so two
-- distinct keys sharing 255 characters would collide.
--
-- Guarded on information_schema rather than IF NOT EXISTS, which MySQL 8 does
-- not have for ADD COLUMN, because SetupFullSchema re-applies the whole set in
-- tests.

SET @col := (SELECT COUNT(*) FROM information_schema.COLUMNS
             WHERE TABLE_SCHEMA = DATABASE()
               AND TABLE_NAME = 'workflow_schedules'
               AND COLUMN_NAME = 'idempotency_key');
SET @sql := IF(@col = 0,
    'ALTER TABLE workflow_schedules ADD COLUMN idempotency_key VARCHAR(255) NULL',
    'DO 0');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @col := (SELECT COUNT(*) FROM information_schema.COLUMNS
             WHERE TABLE_SCHEMA = DATABASE()
               AND TABLE_NAME = 'workflow_schedules'
               AND COLUMN_NAME = 'request_digest');
SET @sql := IF(@col = 0,
    'ALTER TABLE workflow_schedules ADD COLUMN request_digest VARCHAR(64) NULL',
    'DO 0');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @idx := (SELECT COUNT(*) FROM information_schema.STATISTICS
             WHERE TABLE_SCHEMA = DATABASE()
               AND TABLE_NAME = 'workflow_schedules'
               AND INDEX_NAME = 'uq_workflow_schedules_idempotency_key');
SET @sql := IF(@idx = 0,
    'ALTER TABLE workflow_schedules
        ADD UNIQUE KEY uq_workflow_schedules_idempotency_key (tenant_id, idempotency_key)',
    'DO 0');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
