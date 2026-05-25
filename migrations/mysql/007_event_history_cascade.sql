-- 007_event_history_cascade: Add ON DELETE CASCADE to event_history FK
-- Discovers the current FK name and replaces it with a CASCADE version.
-- Down: ALTER TABLE event_history DROP FOREIGN KEY fk_event_history_workflow;
--       ALTER TABLE event_history ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id);

SET @constraint_name = (
    SELECT CONSTRAINT_NAME
    FROM information_schema.KEY_COLUMN_USAGE
    WHERE TABLE_NAME = 'event_history'
      AND COLUMN_NAME = 'workflow_id'
      AND REFERENCED_TABLE_NAME = 'workflow_instances'
    LIMIT 1
);

SET @sql = IF(@constraint_name IS NOT NULL,
    CONCAT('ALTER TABLE event_history DROP FOREIGN KEY ', @constraint_name),
    'SELECT 1'
);
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

ALTER TABLE event_history
    ADD CONSTRAINT fk_event_history_workflow
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE;
