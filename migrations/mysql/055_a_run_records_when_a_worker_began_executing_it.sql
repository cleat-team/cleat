-- workflow_instances.started_at: when a worker first began executing this run.
--
-- cleat-team/cleat#1090. See migrations/postgres/055 for the reasoning; it is
-- the same column for the same reason on every dialect.
--
-- DATETIME(6) to match this dialect's other timestamps -- heartbeat_at is
-- stamped with NOW(6) at claim, so a lower-resolution column here would round
-- away the difference between a run that waited and one that did not, on the
-- very comparison the column exists to support.
--
-- No backfill: a run claimed before this migration has no knowable start, and
-- NULL says "not recorded", which is true.
--
-- MySQL applies this migration set TWICE at worker startup -- once against the
-- main database and once against the per-tenant one (cmd/cleat-worker/main.go,
-- `if *driver == "mysql"`). Idempotence is not a nicety here; the second pass
-- runs on every start.
SET @col := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE()
      AND table_name = 'workflow_instances'
      AND column_name = 'started_at'
);
SET @ddl := IF(@col = 0,
    'ALTER TABLE workflow_instances ADD COLUMN started_at DATETIME(6) NULL',
    'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
