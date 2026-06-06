IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE name = N'priority' AND object_id = OBJECT_ID(N'dbo.workflow_promises'))
    ALTER TABLE workflow_promises ADD priority INTEGER NOT NULL DEFAULT 0;
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE name = N'priority' AND object_id = OBJECT_ID(N'dbo.workflow_update_requests'))
    ALTER TABLE workflow_update_requests ADD priority INTEGER NOT NULL DEFAULT 0;
