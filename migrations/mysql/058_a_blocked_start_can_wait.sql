-- cleat migration 058 (mysql): record the concurrency key a run wants
--
-- See migrations/postgres/058 for the reasoning. cleat#1186.
--
-- MySQL has no partial index and no IF NOT EXISTS on ADD COLUMN before 8.0.29
-- in the form used elsewhere here, so this follows the pattern the other
-- migrations in this directory use for an idempotent column add.
--
-- The index is not partial: MySQL cannot express `WHERE concurrency_key IS NOT
-- NULL` on an index, so it covers every row. That is a real difference from the
-- PostgreSQL arm and is stated rather than left to be discovered -- on a
-- deployment where few runs carry a key this index is larger than its
-- PostgreSQL counterpart for the same benefit.
SET @col := (SELECT COUNT(*) FROM information_schema.COLUMNS
             WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_instances'
               AND COLUMN_NAME = 'concurrency_key');
SET @sql := IF(@col = 0,
    'ALTER TABLE workflow_instances ADD COLUMN concurrency_key VARCHAR(255) NULL, ADD COLUMN concurrency_key_hash VARBINARY(32) NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @idx := (SELECT COUNT(*) FROM information_schema.STATISTICS
             WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_instances'
               AND INDEX_NAME = 'idx_instances_concurrency_key');
SET @sql := IF(@idx = 0,
    'CREATE INDEX idx_instances_concurrency_key ON workflow_instances (tenant_id, concurrency_key_hash)',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

