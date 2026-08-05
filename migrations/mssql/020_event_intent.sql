-- cleat migration 020 (mssql): write-ahead call intent
--
-- SQL Server half of migrations/postgres/020_event_intent.sql; that file
-- carries the rationale. An event is PENDING iff intent_at IS NOT NULL AND
-- checksum IS NULL.
--
-- DATETIMEOFFSET rather than DATETIME2, to match every other timestamp in this
-- schema (001_schema.sql). The filtered index is the same shape as the
-- PostgreSQL partial index; SQL Server supports the predicate directly.
--
-- Both statements are guarded, so this file is safe to re-run against a
-- database whose schema engine/testutil built with the column already present.

IF COL_LENGTH(N'dbo.event_history', N'intent_at') IS NULL
    ALTER TABLE dbo.event_history ADD intent_at DATETIMEOFFSET NULL;
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_event_history_pending' AND object_id = OBJECT_ID(N'dbo.event_history'))
    CREATE INDEX idx_event_history_pending ON dbo.event_history(workflow_id, step)
        WHERE intent_at IS NOT NULL AND checksum IS NULL;
GO
