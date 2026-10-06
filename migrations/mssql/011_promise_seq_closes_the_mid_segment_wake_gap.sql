-- cleat#3171. See migrations/postgres/013 for the mechanism -- this is
-- signal_seq/signal_seq_at_claim's sibling (cleat#953) for the
-- promise/update wake paths rather than the signal one.
IF NOT EXISTS (
    SELECT 1 FROM sys.columns
    WHERE object_id = OBJECT_ID('dbo.workflow_instances') AND name = 'promise_seq'
)
    ALTER TABLE dbo.workflow_instances ADD promise_seq BIGINT NOT NULL DEFAULT 0;

IF NOT EXISTS (
    SELECT 1 FROM sys.columns
    WHERE object_id = OBJECT_ID('dbo.workflow_instances') AND name = 'promise_seq_at_claim'
)
    ALTER TABLE dbo.workflow_instances ADD promise_seq_at_claim BIGINT NOT NULL DEFAULT 0;
GO
