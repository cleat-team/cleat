-- cleat migration 098 (mysql): a queue holder is attributed to a worker
--
-- See migrations/postgres/099_a_queue_holder_is_attributed_to_a_worker.sql for
-- the full reasoning: cleat#1917, second piece, nullable worker_id with no
-- backfill, moved (not duplicated) when a parked run wakes onto a different
-- worker. This header records only what differs here.
--
-- VARCHAR(255), matching workflow_instances.assigned_to (001_schema.sql) --
-- the same worker-id value, carried on the row a holder is claimed for.
--
-- GUARDED, following 095/097's own precedent.

SET @col := (SELECT COUNT(*) FROM information_schema.COLUMNS
             WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'queue_holders'
               AND COLUMN_NAME = 'worker_id');
SET @sql := IF(@col = 0,
    'ALTER TABLE queue_holders ADD COLUMN worker_id VARCHAR(255) NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @idx := (SELECT COUNT(*) FROM information_schema.STATISTICS
             WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'queue_holders'
               AND INDEX_NAME = 'idx_queue_holders_worker');
SET @sql2 := IF(@idx = 0,
    'CREATE INDEX idx_queue_holders_worker ON queue_holders(tenant_id, queue_name, worker_id, expires_at)',
    'SELECT 1');
PREPARE stmt2 FROM @sql2; EXECUTE stmt2; DEALLOCATE PREPARE stmt2;
