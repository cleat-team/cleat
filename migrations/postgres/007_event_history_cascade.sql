-- 007_event_history_cascade: Add ON DELETE CASCADE to event_history FK
-- Replaces the auto-generated FK with a named one that cascades deletes.
-- Down: ALTER TABLE event_history DROP CONSTRAINT fk_event_history_workflow;
--       ALTER TABLE event_history ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id);
DO $$
DECLARE
    constraint_name text;
BEGIN
    SELECT conname INTO constraint_name
    FROM pg_constraint
    WHERE conrelid = 'event_history'::regclass
      AND confrelid = 'workflow_instances'::regclass
      AND contype = 'f';

    IF constraint_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE event_history DROP CONSTRAINT %I', constraint_name);
    END IF;

    ALTER TABLE event_history
        ADD CONSTRAINT fk_event_history_workflow
        FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE;
END $$;
