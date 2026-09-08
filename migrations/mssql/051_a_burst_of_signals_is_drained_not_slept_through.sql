-- cleat#953, second half. See migrations/postgres/048.
IF NOT EXISTS (SELECT 1 FROM sys.columns
               WHERE object_id = OBJECT_ID('dbo.workflow_instances') AND name = 'signal_consumed_seq')
    ALTER TABLE dbo.workflow_instances ADD signal_consumed_seq BIGINT NOT NULL DEFAULT 0;
IF NOT EXISTS (SELECT 1 FROM sys.columns
               WHERE object_id = OBJECT_ID('dbo.workflow_instances') AND name = 'signal_consumed_at_claim')
    ALTER TABLE dbo.workflow_instances ADD signal_consumed_at_claim BIGINT NOT NULL DEFAULT 0;
