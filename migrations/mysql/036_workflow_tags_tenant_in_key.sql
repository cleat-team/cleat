-- cleat migration 036 (mysql): put the tenant in the deployment-tag key
--
-- D7, decided 2026-09-02. IMPROVEMENT-PLAN 3.77 step 4, the last of the three
-- tables. See migrations/postgres/037_workflow_tags_tenant_in_key.sql for the
-- reasoning and for why there are no foreign keys to drop.
--
-- MySQL is documented single-tenant-only (D1, tiers.yaml) for want of row-level
-- security, which BOUNDS the defect below without making it less real. The
-- schema still has to agree across dialects or the Go store code cannot be
-- written once.
--
-- What MySQL did before this migration, measured through the store API with
-- tenant A holding v1 and v2 and tenant B holding only v1:
--
--   A tags "stable" -> v2.  B tags "stable" -> v1.
--   B's write returns NO ERROR.
--   A now resolves "stable" -> v1.   <- A's production pointer, moved
--   B now resolves "stable" -> NOTHING.
--
-- ON DUPLICATE KEY UPDATE matched on (workflow_name, tag), so B's insert became
-- an UPDATE of A's row. B cannot then read what it wrote, because the row still
-- carries A's tenant_id and B's SELECT is scoped -- so the tenant that caused
-- the damage is the one least able to see it. This is 3.12's defect on a
-- different table, and putting the tenant in the key is what fixes it: the
-- statement is unchanged, but the key it matches on is not.
--
-- Idempotent, guarded on information_schema.

-- ── 0. Bring tenant_id into line with the other dialects ─────────────────────
--
-- Third and last table needing this stanza; 034 and 035 needed it identically.
-- 001_schema.sql declares the column three ways and 002_defaults.sql backfills
-- it on postgres and mssql with no MySQL counterpart:
--
--     postgres  tenant_id UUID NOT NULL DEFAULT '00000000-...'
--     mssql     tenant_id UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-...'
--     mysql     tenant_id CHAR(36)                  -- nullable, no default
--
-- Putting the column in the primary key forces NOT NULL, and without a DEFAULT
-- every INSERT omitting it then fails with
--
--     Error 1364 (HY000): Field 'tenant_id' doesn't have a default value
--
-- ONE DIFFERENCE FROM 034 AND 035, and it is the reason this stanza is not
-- simply copied: on this table tenant_id is part of a FOREIGN KEY
-- (fk_workflow_tags_def -> workflow_defs(tenant_id, name, version), widened by
-- 034). MySQL treats a NULL foreign-key column as satisfying the constraint, so
-- a row whose tenant_id is NULL today is unchecked; the UPDATE below makes it
-- checked. If the default tenant has no matching workflow_defs row, the UPDATE
-- fails with
--
--     Error 1452: Cannot add or update a child row: a foreign key constraint fails
--
-- That is the correct outcome -- it is a real orphan the NULL was hiding -- and
-- it fails the migration loudly rather than silently repointing a tag. D7 was
-- taken on the owner's statement that there are no existing workflows to
-- preserve, so this is expected to move zero rows.
UPDATE workflow_tags SET tenant_id = '00000000-0000-0000-0000-000000000000'
    WHERE tenant_id IS NULL;

ALTER TABLE workflow_tags
    MODIFY COLUMN tenant_id CHAR(36) NOT NULL
    DEFAULT '00000000-0000-0000-0000-000000000000';

-- ── 1. Swap the primary key ──────────────────────────────────────────────────
--
-- STATISTICS has one row per key column, so a PRIMARY of two rows is the old
-- (workflow_name, tag) shape.
SET @pk_columns := (
    SELECT COUNT(*) FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_tags'
      AND INDEX_NAME = 'PRIMARY'
);

SET @stmt := IF(@pk_columns = 2,
    'ALTER TABLE workflow_tags DROP PRIMARY KEY, ADD PRIMARY KEY (tenant_id, workflow_name, tag)',
    'DO 0');
PREPARE swap_pk FROM @stmt; EXECUTE swap_pk; DEALLOCATE PREPARE swap_pk;
