-- cleat migration 077 (mysql): schedules retire through disabled_at
--
-- cleat#1702, the LAST of the four conversions. See
-- migrations/postgres/089_schedules_retire_through_disabled_at.sql for the
-- reasoning: the polarity inversion this one conversion carries, why both
-- backfill directions are written, and why the backfill is an upper bound
-- rather than the exact value 087 could claim.
--
-- WHAT IS DIFFERENT HERE, and it is the index.
--
-- PostgreSQL and SQL Server both get a PARTIAL index -- `(tenant_id,
-- next_run_at) WHERE disabled_at IS NULL` -- because the due-schedule scan
-- never wants a retired row and a partial index does not hold one. MySQL has no
-- partial or filtered indexes, so the column stays IN THE KEY:
-- `(tenant_id, disabled_at, next_run_at)`. That is the same shape the old
-- `(tenant_id, enabled, next_run_at)` had, with the column swapped, so the plan
-- does not change here. 087 made the same split for idx_api_keys_hash and left
-- MySQL's equivalent unfiltered for the same reason.
--
-- There is no function to rebuild: admin.get_due_schedules() is a PostgreSQL
-- SECURITY DEFINER function serving the cross-tenant read, and MySQL's
-- cross-tenant path is a plain unscoped SELECT in engine/mysql_ops.go.
--
-- IDEMPOTENT. Every statement naming `enabled` is generated only when `enabled`
-- still exists, because on a fresh database 001_schema.sql never created it.
-- MySQL has no ALTER TABLE ... DROP COLUMN IF EXISTS and no DO block, so the
-- guard is the prepared-statement idiom migrations 076 and 075 already use.

-- ── Backfill, in both directions, while `enabled` is still the authority ─────
--
-- Retired under the authority, unrecorded here. AN UPPER BOUND.

SET @backfill_disabled := (
    SELECT IF(COUNT(*) = 1,
        'UPDATE workflow_schedules SET disabled_at = NOW(6) WHERE NOT enabled AND disabled_at IS NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_schedules'
      AND COLUMN_NAME = 'enabled'
);
PREPARE backfill_disabled FROM @backfill_disabled; EXECUTE backfill_disabled; DEALLOCATE PREPARE backfill_disabled;

-- Live under the authority, so `disabled_at` must not say otherwise. Matches
-- nothing in a shipped deployment; postgres/089's header says why it is written
-- anyway.

SET @backfill_live := (
    SELECT IF(COUNT(*) = 1,
        'UPDATE workflow_schedules SET disabled_at = NULL WHERE enabled AND disabled_at IS NOT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_schedules'
      AND COLUMN_NAME = 'enabled'
);
PREPARE backfill_live FROM @backfill_live; EXECUTE backfill_live; DEALLOCATE PREPARE backfill_live;

-- ── The due-schedule index moves to the new column ──────────────────────────

SET @drop_old_idx := (
    SELECT IF(COUNT(*) > 0,
        'DROP INDEX idx_schedules_tenant_enabled ON workflow_schedules',
        'DO 0')
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_schedules'
      AND INDEX_NAME = 'idx_schedules_tenant_enabled'
);
PREPARE drop_old_idx FROM @drop_old_idx; EXECUTE drop_old_idx; DEALLOCATE PREPARE drop_old_idx;

SET @add_new_idx := (
    SELECT IF(COUNT(*) = 0,
        'CREATE INDEX idx_schedules_tenant_due ON workflow_schedules(tenant_id, disabled_at, next_run_at)',
        'DO 0')
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_schedules'
      AND INDEX_NAME = 'idx_schedules_tenant_due'
);
PREPARE add_new_idx FROM @add_new_idx; EXECUTE add_new_idx; DEALLOCATE PREPARE add_new_idx;

-- ── And the column goes ──────────────────────────────────────────────────────

SET @drop_enabled := (
    SELECT IF(COUNT(*) = 1,
        'ALTER TABLE workflow_schedules DROP COLUMN enabled',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_schedules'
      AND COLUMN_NAME = 'enabled'
);
PREPARE drop_enabled FROM @drop_enabled; EXECUTE drop_enabled; DEALLOCATE PREPARE drop_enabled;
