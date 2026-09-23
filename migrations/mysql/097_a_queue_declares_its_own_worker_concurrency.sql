-- cleat migration 097 (mysql): a queue declares its own worker concurrency
--
-- See migrations/postgres/098_a_queue_declares_its_own_worker_concurrency.sql
-- for the full reasoning: cleat#1917, one nullable column, NULL = no
-- per-worker cap, non-breaking, CHECK backstop for "1 <= worker_concurrency
-- <= concurrency_limit". This header records only what differs here.
--
-- GUARDED, following 095's own precedent: cmd/cleat-worker runs several
-- instances against one database at startup, each applying pending
-- migrations, and a plain ALTER here is not idempotent under that race.

SET @col := (SELECT COUNT(*) FROM information_schema.COLUMNS
             WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'queues'
               AND COLUMN_NAME = 'worker_concurrency');
SET @sql := IF(@col = 0,
    'ALTER TABLE queues
        ADD COLUMN worker_concurrency INT NULL,
        ADD CONSTRAINT ck_queues_worker_concurrency_positive
            CHECK (worker_concurrency IS NULL OR worker_concurrency >= 1),
        ADD CONSTRAINT ck_queues_worker_concurrency_le_concurrency
            CHECK (worker_concurrency IS NULL OR worker_concurrency <= concurrency_limit)',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
