-- cleat#953, second half. See migrations/postgres/048 for the mechanism, and
-- for why this is a new file rather than an edit to the applied 045/046.
SET @c3 := (SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE()
            AND TABLE_NAME = 'workflow_instances' AND COLUMN_NAME = 'signal_consumed_seq');
SET @d3 := IF(@c3 = 0,
    'ALTER TABLE workflow_instances ADD COLUMN signal_consumed_seq BIGINT NOT NULL DEFAULT 0',
    'SELECT 1');
PREPARE s3 FROM @d3;
EXECUTE s3;
DEALLOCATE PREPARE s3;

SET @c4 := (SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE()
            AND TABLE_NAME = 'workflow_instances' AND COLUMN_NAME = 'signal_consumed_at_claim');
SET @d4 := IF(@c4 = 0,
    'ALTER TABLE workflow_instances ADD COLUMN signal_consumed_at_claim BIGINT NOT NULL DEFAULT 0',
    'SELECT 1');
PREPARE s4 FROM @d4;
EXECUTE s4;
DEALLOCATE PREPARE s4;
