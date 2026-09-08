-- cleat#953. See migrations/postgres/046 for the mechanism.
--
-- MySQL has no ADD COLUMN IF NOT EXISTS, so this is guarded by a lookup in
-- information_schema and run through a prepared statement -- the same shape
-- the other MySQL migrations in this directory use for the same reason.
SET @col := (SELECT COUNT(*) FROM information_schema.COLUMNS
             WHERE TABLE_SCHEMA = DATABASE()
               AND TABLE_NAME = 'workflow_instances'
               AND COLUMN_NAME = 'signal_seq');
SET @ddl := IF(@col = 0,
    'ALTER TABLE workflow_instances ADD COLUMN signal_seq BIGINT NOT NULL DEFAULT 0',
    'SELECT 1');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- signal_seq_at_claim: see migrations/postgres/046 for why it is captured on
-- the row rather than passed through the finalize signature.
SET @col2 := (SELECT COUNT(*) FROM information_schema.COLUMNS
              WHERE TABLE_SCHEMA = DATABASE()
                AND TABLE_NAME = 'workflow_instances'
                AND COLUMN_NAME = 'signal_seq_at_claim');
SET @ddl2 := IF(@col2 = 0,
    'ALTER TABLE workflow_instances ADD COLUMN signal_seq_at_claim BIGINT NOT NULL DEFAULT 0',
    'SELECT 1');
PREPARE stmt2 FROM @ddl2;
EXECUTE stmt2;
DEALLOCATE PREPARE stmt2;
