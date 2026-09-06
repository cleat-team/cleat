-- cleat migration 041 (mysql): a promise is addressed by its own id
--
-- MySQL half of migrations/postgres/042_promise_id_unique.sql, which carries
-- the rationale. IMPROVEMENT-PLAN 3.235, cleat#813.
--
-- Note for anyone editing this file: no comment here may contain a semicolon.
--
-- MySQL has no CREATE UNIQUE INDEX IF NOT EXISTS, so this is guarded by
-- checking information_schema first, which is the pattern the other MySQL
-- migrations in this directory use.

SET @idx_exists = (
    SELECT COUNT(*) FROM information_schema.statistics
    WHERE table_schema = DATABASE()
      AND table_name = 'workflow_promises'
      AND index_name = 'idx_promises_id_unique'
);

SET @stmt = IF(@idx_exists = 0,
    'CREATE UNIQUE INDEX idx_promises_id_unique ON workflow_promises(tenant_id, promise_id)',
    'SELECT 1');

PREPARE s FROM @stmt;
EXECUTE s;
DEALLOCATE PREPARE s;
