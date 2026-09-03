-- cleat migration 035 (mysql): put the tenant in the schedule key
--
-- D7, decided 2026-09-02: names are per-tenant. IMPROVEMENT-PLAN 3.77 step 3.
-- See migrations/postgres/036_workflow_schedules_tenant_in_key.sql for the
-- reasoning and for why there are no foreign keys to drop here.
--
-- MySQL is documented single-tenant-only (D1, tiers.yaml) for want of row-level
-- security. The schema still has to agree across dialects or the Go store code
-- cannot be written once.
--
-- Idempotent, guarded on information_schema: the ALTER TABLE forms used here
-- have no IF EXISTS / IF NOT EXISTS in MySQL.

-- ── 0. Bring tenant_id into line with the other dialects ─────────────────────
--
-- The same divergence 034 hit on workflow_defs, in the same shape and for the
-- same reason -- 001_schema.sql declares the column three ways and
-- 002_defaults.sql backfills it on postgres and mssql with no MySQL
-- counterpart:
--
--     postgres  tenant_id UUID NOT NULL DEFAULT '00000000-...'
--     mssql     tenant_id UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-...'
--     mysql     tenant_id CHAR(36)                  -- nullable, no default
--
-- Latent while the column was outside the key. Putting it in the primary key
-- forces NOT NULL, and without a DEFAULT every INSERT omitting the column then
-- fails with
--
--     Error 1364 (HY000): Field 'tenant_id' doesn't have a default value
--
-- Backfill first, then constrain. The other order fails on any row already
-- holding NULL.
--
-- That this is the second table needing the identical stanza is the signal
-- that the divergence belongs in 001_schema.sql rather than in a third
-- migration; workflow_tags is the remaining one and 3.77 records it.
UPDATE workflow_schedules SET tenant_id = '00000000-0000-0000-0000-000000000000'
    WHERE tenant_id IS NULL;

ALTER TABLE workflow_schedules
    MODIFY COLUMN tenant_id CHAR(36) NOT NULL
    DEFAULT '00000000-0000-0000-0000-000000000000';

-- ── 1. Swap the primary key ──────────────────────────────────────────────────
--
-- STATISTICS has one row per key column, so a PRIMARY of one row is the old
-- (name) shape. Widening, so it cannot fail on existing data.
SET @pk_columns := (
    SELECT COUNT(*) FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_schedules'
      AND INDEX_NAME = 'PRIMARY'
);

SET @stmt := IF(@pk_columns = 1,
    'ALTER TABLE workflow_schedules DROP PRIMARY KEY, ADD PRIMARY KEY (tenant_id, name)',
    'DO 0');
PREPARE swap_pk FROM @stmt; EXECUTE swap_pk; DEALLOCATE PREPARE swap_pk;
