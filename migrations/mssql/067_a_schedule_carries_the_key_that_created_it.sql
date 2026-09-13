-- cleat migration 067 (mssql): a schedule carries the idempotency key that
-- created it, so a retry can be told from a name collision.
--
-- POST /api/schedules honours Idempotency-Key. cleat#1495.
--
-- See migrations/postgres/072 for why the key lives on the schedule row rather
-- than in idempotency_keys, why it is nullable and not backfilled, and why it is
-- scoped by tenant. The reasoning is identical on all three dialects and is
-- written out once, there.
--
-- SQL SERVER-SPECIFIC, AND THIS IS THE DIALECT THE FILTER EXISTS FOR. A unique
-- index here treats NULLs as EQUAL to one another -- unlike PostgreSQL and
-- MySQL, where they never collide. An unfiltered
-- UNIQUE (tenant_id, idempotency_key) would therefore admit exactly ONE keyless
-- schedule per tenant and refuse every one after it with a duplicate-key error,
-- breaking the case that has nothing to do with idempotency at all. The
-- WHERE clause is what makes the constraint mean "no two schedules share a key"
-- rather than "at most one schedule has no key".
--
-- Guarded on sys.* rather than IF NOT EXISTS, which SQL Server does not have on
-- ALTER TABLE, because SetupFullSchema re-applies the whole set in tests.

IF NOT EXISTS (SELECT 1 FROM sys.columns
               WHERE object_id = OBJECT_ID(N'dbo.workflow_schedules')
                 AND name = N'idempotency_key')
    ALTER TABLE dbo.workflow_schedules ADD idempotency_key NVARCHAR(255) NULL;
GO

IF NOT EXISTS (SELECT 1 FROM sys.columns
               WHERE object_id = OBJECT_ID(N'dbo.workflow_schedules')
                 AND name = N'request_digest')
    ALTER TABLE dbo.workflow_schedules ADD request_digest NVARCHAR(64) NULL;
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes
               WHERE object_id = OBJECT_ID(N'dbo.workflow_schedules')
                 AND name = N'uq_workflow_schedules_idempotency_key')
    CREATE UNIQUE INDEX uq_workflow_schedules_idempotency_key
        ON dbo.workflow_schedules (tenant_id, idempotency_key)
        WHERE idempotency_key IS NOT NULL;
GO
