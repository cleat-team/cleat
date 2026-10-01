-- cleat#1986. Every workflow definition gets an exposure class, one of a
-- closed set of three: 'auth' (the default -- reachable from the HTTP API,
-- authentication required, today's behaviour), 'internal' (not reachable from
-- the external HTTP surface at all), or 'public' (reachable without
-- authentication, gated separately on an operator opt-in).
--
-- MySQL has no bare conditional DDL statement -- confirmed directly against
-- mysql:8.4.11 (the pin in .github/workflows/multi-db-ci.yml):
--     ALTER TABLE t ADD COLUMN IF NOT EXISTS x INT;
--     ERROR 1064 (42000): You have an error in your SQL syntax ...
-- A bare ALTER here is also not idempotent on MySQL the way the Postgres arm
-- is: MySQL DDL is not transactional, so a crash between this ALTER and the
-- runner's schema_migrations write (migration/runner.go) leaves a worker that
-- re-runs this file on its next boot and gets
-- `ERROR 1060 (42S21): Duplicate column name 'exposure'` and never starts.
-- Guarded through information_schema.columns and PREPARE/EXECUTE, the same
-- shape plugins/webhookingest/migrations.go's v8 uses for the identical
-- hazard on webhook_sources.deleted_at. Verified idempotent against a scratch
-- mysql:8.4.11 container: run twice, the second run is a no-op.
SET @col := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE()
      AND table_name = 'workflow_defs'
      AND column_name = 'exposure'
);
SET @ddl := IF(@col = 0,
    'ALTER TABLE workflow_defs ADD COLUMN exposure VARCHAR(16) NOT NULL DEFAULT ''auth''',
    'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- Same hazard, same guard, for the CHECK constraint. MySQL 8 does enforce
-- CHECK constraints (confirmed: a deliberately out-of-set INSERT against the
-- scratch container above failed with ERROR 3819, "Check constraint ...
-- is violated"), so this is not a no-op addition.
SET @chk := (
    SELECT COUNT(*) FROM information_schema.table_constraints
    WHERE table_schema = DATABASE()
      AND table_name = 'workflow_defs'
      AND constraint_name = 'ck_workflow_defs_exposure'
);
SET @ddl2 := IF(@chk = 0,
    'ALTER TABLE workflow_defs ADD CONSTRAINT ck_workflow_defs_exposure CHECK (exposure IN (''public'',''auth'',''internal''))',
    'DO 0');
PREPARE stmt2 FROM @ddl2;
EXECUTE stmt2;
DEALLOCATE PREPARE stmt2;
