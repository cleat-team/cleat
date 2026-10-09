-- cleat#2059 (tail): realign event_history's primary key to
-- (tenant_id, workflow_id, step), matching PostgreSQL's already-realigned,
-- hash-partitioned shape (migrations/postgres/001_schema.sql). Drops
-- idx_event_history_tenant_wf as redundant: it led with the exact same
-- three columns in the exact same order the new primary key does. See
-- engine/mysql_ops.go's preemptivelySettle defer-check for the companion
-- query-predicate fix this realignment required to keep its seek path
-- under the new key, and migrations/mssql/014_event_history_tenant_pk.sql
-- for the SQL Server sibling.
--
-- Guarded like 009_tenant_allow_public_exposure.sql and
-- 010_promise_seq_closes_the_mid_segment_wake_gap.sql (cleat#2117): a bare
-- ALTER is not idempotent on MySQL, so a crash between one ALTER and the
-- runner's schema_migrations write must not fail a re-run of this file.

-- 1. tenant_id must be NOT NULL before it can join a PRIMARY KEY -- it was
-- the only event_history column with no default, and the one about to
-- join the primary key below. Every other tenant_id column in this file
-- (bar one unrelated table) already uses this exact default.
SET @nullable := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'event_history'
      AND column_name = 'tenant_id' AND is_nullable = 'YES'
);

SET @ddl := IF(@nullable > 0,
    'UPDATE event_history SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL',
    'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @ddl2 := IF(@nullable > 0,
    'ALTER TABLE event_history MODIFY tenant_id CHAR(36) NOT NULL DEFAULT ''00000000-0000-0000-0000-000000000000''',
    'DO 0');
PREPARE stmt2 FROM @ddl2;
EXECUTE stmt2;
DEALLOCATE PREPARE stmt2;

-- 2. idx_event_history_workflow_fk -- cleat#2059. Added BEFORE the primary
-- key changes below, not after: event_history_ibfk_1's FOREIGN KEY
-- (workflow_id) REFERENCES workflow_instances (id) ON DELETE CASCADE
-- currently relies on the primary key (workflow_id, step) as its covering
-- index, and InnoDB refuses to drop that primary key while nothing else
-- covers the FK's referencing column -- measured directly: running step 3
-- below without this step first fails the whole migration with
-- `Error 1553 (HY000): Cannot drop index 'PRIMARY': needed in a foreign
-- key constraint`, on EVERY test that applies these migrations from
-- scratch, not merely in principle. The new primary key below leads with
-- tenant_id, not workflow_id, so it cannot serve as the FK's covering
-- index either -- this index is permanent, not a transitional scaffold.
SET @fk_idx_exists := (
    SELECT COUNT(*) FROM information_schema.statistics
    WHERE table_schema = DATABASE() AND table_name = 'event_history'
      AND index_name = 'idx_event_history_workflow_fk'
);

SET @ddl3 := IF(@fk_idx_exists = 0,
    'ALTER TABLE event_history ADD INDEX idx_event_history_workflow_fk (workflow_id, step)',
    'DO 0');
PREPARE stmt3 FROM @ddl3;
EXECUTE stmt3;
DEALLOCATE PREPARE stmt3;

-- 3. Realign the primary key. Checked by whether the PRIMARY index's
-- column set already matches, not merely whether tenant_id is IN it at
-- any position -- MySQL has no "change the key order" statement, only
-- drop-and-recreate, and this must not re-run that against a key already
-- in the target shape.
SET @pk_matches := (
    SELECT COUNT(*) FROM (
        SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index) AS cols
        FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 'event_history'
          AND index_name = 'PRIMARY'
        GROUP BY index_name
    ) pk
    WHERE pk.cols = 'tenant_id,workflow_id,step'
);

SET @ddl4 := IF(@pk_matches = 0,
    'ALTER TABLE event_history DROP PRIMARY KEY, ADD PRIMARY KEY (tenant_id, workflow_id, step)',
    'DO 0');
PREPARE stmt4 FROM @ddl4;
EXECUTE stmt4;
DEALLOCATE PREPARE stmt4;

-- 4. idx_event_history_tenant_wf is now redundant with the primary key
-- above (same three columns, same order).
SET @idx_exists := (
    SELECT COUNT(*) FROM information_schema.statistics
    WHERE table_schema = DATABASE() AND table_name = 'event_history'
      AND index_name = 'idx_event_history_tenant_wf'
);

SET @ddl5 := IF(@idx_exists > 0,
    'ALTER TABLE event_history DROP INDEX idx_event_history_tenant_wf',
    'DO 0');
PREPARE stmt5 FROM @ddl5;
EXECUTE stmt5;
DEALLOCATE PREPARE stmt5;
