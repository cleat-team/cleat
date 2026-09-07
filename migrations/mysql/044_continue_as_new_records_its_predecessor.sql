-- cleat migration 044 (mysql): a continuation records the run it came from
--
-- MySQL half of migrations/postgres/045_continue_as_new_records_its_predecessor.sql,
-- which carries the rationale for the column and for why it is NOT
-- parent_workflow_id. cleat#826.
--
-- Two dialect differences:
--
--   * No ADD COLUMN IF NOT EXISTS, so the guarded-add idiom from
--     037_defer_phase_marker.sql is used here too.
--   * No partial indexes. The PostgreSQL and SQL Server halves exclude
--     non-continuations from the index; MySQL cannot, so this one carries every
--     row with the column NULL. That is the same trade already made by
--     idx_workflow_instances_defer_phase in 037.
--
-- continued_from is VARCHAR(255) to match workflow_instances.id, which is
-- VARCHAR(255) here (001_schema.sql) rather than the TEXT it is on PostgreSQL.

SET @add_continued_from := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_instances ADD COLUMN continued_from VARCHAR(255) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_instances'
      AND COLUMN_NAME = 'continued_from'
);
PREPARE add_continued_from FROM @add_continued_from;
EXECUTE add_continued_from;
DEALLOCATE PREPARE add_continued_from;

SET @add_continued_from_idx := (
    SELECT IF(COUNT(*) = 0,
        'CREATE INDEX idx_workflow_instances_continued_from ON workflow_instances (continued_from)',
        'DO 0')
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_instances'
      AND INDEX_NAME = 'idx_workflow_instances_continued_from'
);
PREPARE add_continued_from_idx FROM @add_continued_from_idx;
EXECUTE add_continued_from_idx;
DEALLOCATE PREPARE add_continued_from_idx;
