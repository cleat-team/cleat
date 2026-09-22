-- cleat migration 095 (mysql): a queue declares its own rate limit
--
-- See migrations/postgres/096_a_queue_declares_its_own_rate_limit.sql for the
-- full reasoning: cleat#1918, two nullable columns rather than a new
-- config table, NULL/NULL = unlimited, non-breaking. This header records only
-- what differs here.
--
-- MySQL has no ADD CONSTRAINT CHECK before 8.0.16 in one statement the way
-- postgres does, but this repo's floor already requires CHECK-capable MySQL
-- (092/093 use it for concurrency_limit and the name charset), so the three
-- constraints are written the same way, inline on the ALTER.
--
-- GUARDED like 060: cmd/cleat-worker runs several instances against one
-- database at startup, each applying pending migrations, and a plain ALTER
-- here is not idempotent under that race -- measured on the postgres sibling
-- of this exact migration in the Cluster Integration Tests job (cleat#1918 PR
-- #1961), where concurrent workers raced this ALTER and every loser failed
-- permanently on "column already exists" instead of seeing the winner's work
-- and moving on.

SET @col := (SELECT COUNT(*) FROM information_schema.COLUMNS
             WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'queues'
               AND COLUMN_NAME = 'rate_limit');
SET @sql := IF(@col = 0,
    'ALTER TABLE queues
        ADD COLUMN rate_limit          INT NULL,
        ADD COLUMN rate_period_seconds INT NULL,
        ADD CONSTRAINT ck_queues_rate_limit_paired
            CHECK ((rate_limit IS NULL) = (rate_period_seconds IS NULL)),
        ADD CONSTRAINT ck_queues_rate_limit_positive
            CHECK (rate_limit IS NULL OR rate_limit >= 1),
        ADD CONSTRAINT ck_queues_rate_period_positive
            CHECK (rate_period_seconds IS NULL OR rate_period_seconds >= 1)',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
