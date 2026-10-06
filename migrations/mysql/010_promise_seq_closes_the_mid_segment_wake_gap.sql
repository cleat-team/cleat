-- cleat#3171: see migrations/postgres/012 for the rationale -- this is
-- signal_seq/signal_seq_at_claim's sibling (cleat#953) for the promise/update
-- wake paths. Two columns, each guarded separately through
-- information_schema.columns and PREPARE/EXECUTE, the same shape
-- 009_tenant_allow_public_exposure.sql uses for the identical "bare ALTER is
-- not idempotent on MySQL" hazard (cleat#2117): a crash between one ALTER and
-- the runner's schema_migrations write leaves a worker that re-runs this file
-- on its next boot and gets `ERROR 1060 (42S21): Duplicate column name`.
SET @col := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE()
      AND table_name = 'workflow_instances'
      AND column_name = 'promise_seq'
);
SET @ddl := IF(@col = 0,
    'ALTER TABLE workflow_instances ADD COLUMN promise_seq BIGINT NOT NULL DEFAULT 0',
    'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @col2 := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE()
      AND table_name = 'workflow_instances'
      AND column_name = 'promise_seq_at_claim'
);
SET @ddl2 := IF(@col2 = 0,
    'ALTER TABLE workflow_instances ADD COLUMN promise_seq_at_claim BIGINT NOT NULL DEFAULT 0',
    'DO 0');
PREPARE stmt2 FROM @ddl2;
EXECUTE stmt2;
DEALLOCATE PREPARE stmt2;
