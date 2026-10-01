-- cleat#1981: typed invocation, part 2. See migrations/postgres/007 for the
-- full rationale; this is the same column on the MySQL dialect.
--
-- Guarded through information_schema.columns and PREPARE/EXECUTE, the same
-- shape 004_workflow_defs_exposure_class.sql's `exposure` column and
-- 005_entry_point_schemas.sql use for the identical "bare ALTER is not
-- idempotent on MySQL" hazard (cleat#2117): a crash between this ALTER and
-- the runner's schema_migrations write leaves a worker that re-runs this
-- file on its next boot and gets `ERROR 1060 (42S21): Duplicate column
-- name 'input_validation_disabled'` and never starts.
SET @col := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE()
      AND table_name = 'workflow_defs'
      AND column_name = 'input_validation_disabled'
);
SET @ddl := IF(@col = 0,
    'ALTER TABLE workflow_defs ADD COLUMN input_validation_disabled TINYINT(1) NOT NULL DEFAULT 0',
    'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
