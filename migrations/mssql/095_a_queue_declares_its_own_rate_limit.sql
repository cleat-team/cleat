-- cleat migration 095 (mssql): a queue declares its own rate limit
--
-- See migrations/postgres/096_a_queue_declares_its_own_rate_limit.sql for the
-- full reasoning: cleat#1918, two nullable columns rather than a new config
-- table, NULL/NULL = unlimited, non-breaking. This header records only what
-- differs here.

ALTER TABLE dbo.queues
    ADD rate_limit          INT NULL,
        rate_period_seconds INT NULL;
GO

ALTER TABLE dbo.queues
    ADD CONSTRAINT ck_queues_rate_limit_paired
        CHECK ((rate_limit IS NULL AND rate_period_seconds IS NULL)
            OR (rate_limit IS NOT NULL AND rate_period_seconds IS NOT NULL)),
        CONSTRAINT ck_queues_rate_limit_positive
        CHECK (rate_limit IS NULL OR rate_limit >= 1),
        CONSTRAINT ck_queues_rate_period_positive
        CHECK (rate_period_seconds IS NULL OR rate_period_seconds >= 1);
GO
