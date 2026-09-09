-- cleat migration 050 (mysql): a reclaim is counted on the row it happened to
--
-- cleat#1008. See migrations/postgres/051 for the full reasoning: why
-- `generation` cannot serve (it counts claims, not reclaims, and a healthy
-- workflow reaches 12), and why this column is deliberately NOT a bound -- the
-- causes of repeated reclaim that survive the worker's default limits are all
-- infrastructure, and dead-lettering past a threshold would convert a node
-- redeploy into permanent workflow failure.
--
-- MySQL 8.0 has no ADD COLUMN IF NOT EXISTS, so the guard is the prepared-
-- statement form the rest of this directory uses (see 044).

SET @add_reclaim_count := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_instances ADD COLUMN reclaim_count BIGINT NOT NULL DEFAULT 0',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_instances'
      AND COLUMN_NAME = 'reclaim_count'
);
PREPARE add_reclaim_count FROM @add_reclaim_count;
EXECUTE add_reclaim_count;
DEALLOCATE PREPARE add_reclaim_count;
