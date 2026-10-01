-- cleat#1980: typed invocation, part 1. See migrations/postgres/006 for the
-- full rationale; this is the same column on the MySQL dialect.
--
-- A bare ALTER here is not idempotent on MySQL -- confirmed by
-- TestMigrationIsADeployStepOnEveryDialect's step 6 (cleat#2117), which
-- deletes the newest schema_migrations row and re-runs --migrate-only to
-- confirm the runner repairs a "one behind" schema: this file is replayed by
-- migration.NewRunner's schema_migrations dedupe only, not by MySQL itself
-- refusing a duplicate, so the second application hits
-- `ERROR 1060 (42S21): Duplicate column name 'entry_point_schemas'` and the
-- worker never starts. Same hazard and same guard shape as
-- migrations/mysql/004_workflow_defs_exposure_class.sql's `exposure` column
-- (cleat#1986) and plugins/webhookingest/migrations.go's v8.
SET @col := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE()
      AND table_name = 'workflow_defs'
      AND column_name = 'entry_point_schemas'
);
SET @ddl := IF(@col = 0,
    'ALTER TABLE workflow_defs ADD COLUMN entry_point_schemas json DEFAULT NULL',
    'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
