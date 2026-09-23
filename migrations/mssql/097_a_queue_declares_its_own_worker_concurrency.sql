-- cleat migration 097 (mssql): a queue declares its own worker concurrency
--
-- See migrations/postgres/098_a_queue_declares_its_own_worker_concurrency.sql
-- for the full reasoning: cleat#1917, one nullable column, NULL = no
-- per-worker cap, non-breaking, CHECK backstop for "1 <= worker_concurrency
-- <= concurrency_limit". This header records only what differs here.
--
-- GUARDED, following 095's own precedent (COL_LENGTH before ADD,
-- sys.check_constraints before ADD CONSTRAINT).

IF COL_LENGTH(N'dbo.queues', N'worker_concurrency') IS NULL
    ALTER TABLE dbo.queues
        ADD worker_concurrency INT NULL;
GO

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_queues_worker_concurrency_positive')
    ALTER TABLE dbo.queues
        ADD CONSTRAINT ck_queues_worker_concurrency_positive
            CHECK (worker_concurrency IS NULL OR worker_concurrency >= 1);
GO

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_queues_worker_concurrency_le_concurrency')
    ALTER TABLE dbo.queues
        ADD CONSTRAINT ck_queues_worker_concurrency_le_concurrency
            CHECK (worker_concurrency IS NULL OR worker_concurrency <= concurrency_limit);
GO
