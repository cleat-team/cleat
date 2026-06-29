IF NOT EXISTS (SELECT 1 FROM sys.columns c JOIN sys.tables t ON c.object_id = t.object_id WHERE t.name = 'workflow_tags' AND c.name = 'canary_weight')
ALTER TABLE dbo.workflow_tags ADD canary_weight INT NOT NULL DEFAULT 0;
GO

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = 'ck_wf_tags_canary_weight')
ALTER TABLE dbo.workflow_tags ADD CONSTRAINT ck_wf_tags_canary_weight CHECK (canary_weight >= 0 AND canary_weight <= 100);
GO
