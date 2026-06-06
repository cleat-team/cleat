-- 007_event_history_cascade: Add ON DELETE CASCADE to event_history FK
-- Drops fk_event_history_workflow and recreates with CASCADE.
-- Down: ALTER TABLE event_history DROP CONSTRAINT fk_event_history_workflow;
--       ALTER TABLE event_history ADD CONSTRAINT fk_event_history_workflow
--         FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id);

IF EXISTS (
    SELECT 1 FROM sys.foreign_keys
    WHERE name = 'fk_event_history_workflow'
      AND parent_object_id = OBJECT_ID('dbo.event_history')
)
BEGIN
    ALTER TABLE dbo.event_history DROP CONSTRAINT fk_event_history_workflow;
END

ALTER TABLE dbo.event_history
    ADD CONSTRAINT fk_event_history_workflow
    FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE;
