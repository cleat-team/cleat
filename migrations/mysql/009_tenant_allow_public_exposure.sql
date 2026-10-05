-- cleat#1986: see migrations/postgres/011 for the full rationale; this is the
-- same column on the MySQL dialect, on the unqualified `tenants` table (MySQL
-- has no admin schema -- see auth/tenant_store.go's setTenantSuspendedStmt for
-- the same split).
--
-- Guarded through information_schema.columns and PREPARE/EXECUTE, the same
-- shape 004_workflow_defs_exposure_class.sql's `exposure` column and
-- 006_input_validation_disabled.sql use for the identical "bare ALTER is not
-- idempotent on MySQL" hazard (cleat#2117): a crash between this ALTER and the
-- runner's schema_migrations write leaves a worker that re-runs this file on
-- its next boot and gets `ERROR 1060 (42S21): Duplicate column name
-- 'allow_public_exposure'` and never starts.
SET @col := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE()
      AND table_name = 'tenants'
      AND column_name = 'allow_public_exposure'
);
SET @ddl := IF(@col = 0,
    'ALTER TABLE tenants ADD COLUMN allow_public_exposure TINYINT(1) NOT NULL DEFAULT 0',
    'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
