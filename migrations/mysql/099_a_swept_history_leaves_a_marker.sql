-- cleat migration 099 (mysql): a swept history leaves a marker
--
-- See migrations/postgres/100_a_swept_history_leaves_a_marker.sql for the
-- full reasoning: cleat#2038, nullable history_swept_at with no backfill, set
-- by DeleteExpiredEvents alongside its existing DELETE so ReReplay's
-- pending-intent guard can tell "never attempted" from "swept, cannot tell"
-- rather than treating both as empty history. Retention semantics (what
-- --retention-days deletes) are unchanged.
--
-- TIMESTAMP(6), matching completed_at/compacted_at (001_schema.sql).
--
-- GUARDED, following 095/097/098's own precedent.

SET @col := (SELECT COUNT(*) FROM information_schema.COLUMNS
             WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_instances'
               AND COLUMN_NAME = 'history_swept_at');
SET @sql := IF(@col = 0,
    'ALTER TABLE workflow_instances ADD COLUMN history_swept_at TIMESTAMP(6) NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
