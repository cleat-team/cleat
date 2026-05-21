-- event_history
IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_event_history_workflow')
    ALTER TABLE dbo.event_history DROP CONSTRAINT fk_event_history_workflow;
ALTER TABLE dbo.event_history ADD CONSTRAINT fk_event_history_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE;

-- workflow_signals
IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_signals_workflow')
    ALTER TABLE dbo.workflow_signals DROP CONSTRAINT fk_signals_workflow;
ALTER TABLE dbo.workflow_signals ADD CONSTRAINT fk_signals_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE;

-- workflow_promises
IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_promises_workflow')
    ALTER TABLE dbo.workflow_promises DROP CONSTRAINT fk_promises_workflow;
ALTER TABLE dbo.workflow_promises ADD CONSTRAINT fk_promises_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE;

-- concurrency_keys
IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_concurrency_keys_workflow')
    ALTER TABLE dbo.concurrency_keys DROP CONSTRAINT fk_concurrency_keys_workflow;
ALTER TABLE dbo.concurrency_keys ADD CONSTRAINT fk_concurrency_keys_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE;

-- workflow_update_requests
IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_update_requests_workflow')
    ALTER TABLE dbo.workflow_update_requests DROP CONSTRAINT fk_update_requests_workflow;
ALTER TABLE dbo.workflow_update_requests ADD CONSTRAINT fk_update_requests_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE;
