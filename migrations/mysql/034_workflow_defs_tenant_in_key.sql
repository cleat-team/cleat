-- cleat migration 034 (mysql): put the tenant in the workflow-definition key
--
-- D7, decided 2026-09-02: names are per-tenant. See
-- migrations/postgres/035_workflow_defs_tenant_in_key.sql for the reasoning,
-- and IMPROVEMENT-PLAN 3.77 for the four options and why this is option 4.
--
-- MySQL is documented single-tenant-only (D1, tiers.yaml) because it has no
-- row-level security. That does not make this migration optional: the schema
-- has to agree across dialects or the Go store code cannot be written once,
-- and MySQL's per-tenant-database topology still keys definitions by name.
--
-- Idempotent, guarded on information_schema, because the ALTER TABLE forms
-- used here have no IF EXISTS / IF NOT EXISTS in MySQL.

-- ── 0. Bring tenant_id into line with the other dialects ─────────────────────
--
-- A divergence this migration exposed rather than created. 001_schema.sql
-- declares the column three different ways:
--
--     postgres  tenant_id UUID NOT NULL DEFAULT '00000000-...'
--     mssql     tenant_id UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-...'
--     mysql     tenant_id CHAR(36)                  -- nullable, no default
--
-- and 002_defaults.sql backfills the column on postgres and mssql but has no
-- MySQL counterpart. That was latent: nothing required the column to be
-- non-null here, so a NULL tenant just meant "unset". Putting the column in
-- the primary key makes MySQL force it NOT NULL, and without a DEFAULT every
-- INSERT that omits it then fails with
--
--     Error 1364 (HY000): Field 'tenant_id' doesn't have a default value
--
-- Backfill first, then constrain. Doing it the other way round fails on any
-- row that is already NULL.
UPDATE workflow_defs SET tenant_id = '00000000-0000-0000-0000-000000000000'
    WHERE tenant_id IS NULL;

ALTER TABLE workflow_defs
    MODIFY COLUMN tenant_id CHAR(36) NOT NULL
    DEFAULT '00000000-0000-0000-0000-000000000000';

-- ── 1. Drop the foreign keys that point at the old key ───────────────────────
--
-- Named dynamically. 001_schema.sql declares all three inline, so InnoDB
-- generated their names (workflow_instances_ibfk_N and friends) and the numbers
-- depend on declaration order -- not something to hard-code. Each is dropped
-- only if it is actually there.
DROP PROCEDURE IF EXISTS cleat_drop_defs_fks;
DELIMITER //
CREATE PROCEDURE cleat_drop_defs_fks()
BEGIN
    DECLARE done INT DEFAULT 0;
    DECLARE tbl, fk VARCHAR(64);
    DECLARE cur CURSOR FOR
        SELECT TABLE_NAME, CONSTRAINT_NAME
        FROM information_schema.REFERENTIAL_CONSTRAINTS
        WHERE CONSTRAINT_SCHEMA = DATABASE()
          AND REFERENCED_TABLE_NAME = 'workflow_defs';
    DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = 1;
    OPEN cur;
    read_loop: LOOP
        FETCH cur INTO tbl, fk;
        IF done THEN
            LEAVE read_loop;
        END IF;
        SET @s := CONCAT('ALTER TABLE `', tbl, '` DROP FOREIGN KEY `', fk, '`');
        PREPARE st FROM @s; EXECUTE st; DEALLOCATE PREPARE st;
    END LOOP;
    CLOSE cur;
END //
DELIMITER ;
CALL cleat_drop_defs_fks();
DROP PROCEDURE cleat_drop_defs_fks;

-- ── 2. Swap the primary key ──────────────────────────────────────────────────
--
-- STATISTICS has one row per key column, so a PRIMARY of two rows is the old
-- (name, version) shape. Widening, so it cannot fail on existing data.
SET @pk_columns := (
    SELECT COUNT(*) FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_defs'
      AND INDEX_NAME = 'PRIMARY'
);

SET @stmt := IF(@pk_columns = 2,
    'ALTER TABLE workflow_defs DROP PRIMARY KEY, ADD PRIMARY KEY (tenant_id, name, version)',
    'DO 0');
PREPARE swap_pk FROM @stmt; EXECUTE swap_pk; DEALLOCATE PREPARE swap_pk;

-- ── 3. Put the foreign keys back, with the tenant in them ────────────────────
--
-- Named explicitly this time, so the next migration that has to touch them can
-- say which one it means.
--
-- No ON DELETE clause, matching what these carried before: NO ACTION, so
-- deleting a definition a running workflow still references is refused.
SET @have := (
    SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS
    WHERE CONSTRAINT_SCHEMA = DATABASE()
      AND CONSTRAINT_NAME = 'fk_instances_def'
);
SET @stmt := IF(@have = 0,
    'ALTER TABLE workflow_instances ADD CONSTRAINT fk_instances_def
       FOREIGN KEY (tenant_id, def_name, def_version)
       REFERENCES workflow_defs(tenant_id, name, version)',
    'DO 0');
PREPARE add_fk FROM @stmt; EXECUTE add_fk; DEALLOCATE PREPARE add_fk;

SET @have := (
    SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS
    WHERE CONSTRAINT_SCHEMA = DATABASE()
      AND CONSTRAINT_NAME = 'fk_workflow_tags_def'
);
SET @stmt := IF(@have = 0,
    'ALTER TABLE workflow_tags ADD CONSTRAINT fk_workflow_tags_def
       FOREIGN KEY (tenant_id, workflow_name, version)
       REFERENCES workflow_defs(tenant_id, name, version)',
    'DO 0');
PREPARE add_fk FROM @stmt; EXECUTE add_fk; DEALLOCATE PREPARE add_fk;

SET @have := (
    SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS
    WHERE CONSTRAINT_SCHEMA = DATABASE()
      AND CONSTRAINT_NAME = 'fk_workflow_routing_def'
);
SET @stmt := IF(@have = 0,
    'ALTER TABLE workflow_routing ADD CONSTRAINT fk_workflow_routing_def
       FOREIGN KEY (tenant_id, workflow_name, target_version)
       REFERENCES workflow_defs(tenant_id, name, version)',
    'DO 0');
PREPARE add_fk FROM @stmt; EXECUTE add_fk; DEALLOCATE PREPARE add_fk;
