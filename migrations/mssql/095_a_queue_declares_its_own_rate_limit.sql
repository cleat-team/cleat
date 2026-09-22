-- cleat migration 095 (mssql): a queue declares its own rate limit
--
-- See migrations/postgres/096_a_queue_declares_its_own_rate_limit.sql for the
-- full reasoning: cleat#1918, two nullable columns rather than a new config
-- table, NULL/NULL = unlimited, non-breaking. This header records only what
-- differs here.
--
-- GUARDED, following 082's own precedent for admin.tenants.org_id
-- (COL_LENGTH before ADD, sys.check_constraints before ADD CONSTRAINT):
-- cmd/cleat-worker runs several instances against one database at startup,
-- each applying pending migrations, and a plain ALTER here is not idempotent
-- under that race -- measured on the postgres sibling of this exact migration
-- in the Cluster Integration Tests job (cleat#1918 PR #1961), where concurrent
-- workers raced the ALTER and every loser failed permanently on "column
-- already exists" instead of seeing the winner's work and moving on.

IF COL_LENGTH(N'dbo.queues', N'rate_limit') IS NULL
    ALTER TABLE dbo.queues
        ADD rate_limit          INT NULL,
            rate_period_seconds INT NULL;
GO

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_queues_rate_limit_paired')
    ALTER TABLE dbo.queues
        ADD CONSTRAINT ck_queues_rate_limit_paired
            CHECK ((rate_limit IS NULL AND rate_period_seconds IS NULL)
                OR (rate_limit IS NOT NULL AND rate_period_seconds IS NOT NULL));
GO

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_queues_rate_limit_positive')
    ALTER TABLE dbo.queues
        ADD CONSTRAINT ck_queues_rate_limit_positive
            CHECK (rate_limit IS NULL OR rate_limit >= 1);
GO

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_queues_rate_period_positive')
    ALTER TABLE dbo.queues
        ADD CONSTRAINT ck_queues_rate_period_positive
            CHECK (rate_period_seconds IS NULL OR rate_period_seconds >= 1);
GO
