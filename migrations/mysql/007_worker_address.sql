-- cleat#2196. The reaper-to-worker veto channel needs a way to reach a
-- specific worker, and the owner's decision was DNS via a headless Service
-- (k8s/service.yaml, charts/cleat/templates/service.yaml), not a captured
-- pod IP. workers.hostname is diagnostic only (os.Hostname(), not
-- necessarily routable); this is the address another worker can actually
-- dial, published only when --worker-service-name names a headless Service
-- that selects this pod (see cmd/cleat-worker/connection_share.go's
-- podAddress). Empty for every worker outside that configuration, which is
-- every worker today -- nothing yet reads this column (cleat#2196 step 4).
--
-- MySQL has no bare conditional DDL statement (confirmed on mysql:8.4.11,
-- the pin in .github/workflows/multi-db-ci.yml -- see 004_workflow_defs_
-- exposure_class.sql for the measured error), and a bare ALTER is not
-- idempotent on MySQL the way Postgres's ADD COLUMN IF NOT EXISTS is: MySQL
-- DDL is not transactional, so a crash between this ALTER and the runner's
-- schema_migrations write leaves a worker that re-runs this file on its next
-- boot and gets `ERROR 1060 (42S21): Duplicate column name 'address'` and
-- never starts. Guarded through information_schema.columns, the same shape
-- that file's workflow_defs.exposure guard uses.
SET @col := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE()
      AND table_name = 'workers'
      AND column_name = 'address'
);
SET @ddl := IF(@col = 0,
    'ALTER TABLE workers ADD COLUMN address VARCHAR(255) NOT NULL DEFAULT ''''',
    'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
