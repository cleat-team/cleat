-- cleat#953. See migrations/postgres/046 for the mechanism.
IF NOT EXISTS (
    SELECT 1 FROM sys.columns
    WHERE object_id = OBJECT_ID('dbo.workflow_instances') AND name = 'signal_seq'
)
    ALTER TABLE dbo.workflow_instances ADD signal_seq BIGINT NOT NULL DEFAULT 0;

-- signal_seq_at_claim: see migrations/postgres/046.
IF NOT EXISTS (
    SELECT 1 FROM sys.columns
    WHERE object_id = OBJECT_ID('dbo.workflow_instances') AND name = 'signal_seq_at_claim'
)
    ALTER TABLE dbo.workflow_instances ADD signal_seq_at_claim BIGINT NOT NULL DEFAULT 0;
