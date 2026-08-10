-- cleat migration 034 (mssql): widen event_history.request to NVARCHAR(MAX)
--
-- Finding S5. 001_schema.sql declares dbo.event_history.request as
-- NVARCHAR(255) while its sibling `response` is NVARCHAR(MAX), and both
-- PostgreSQL (TEXT) and MySQL (LONGTEXT) store it unbounded. Every dialect
-- writes the same thing here: engine/mssql_events.go's
-- appendEventsInTxOpts base64-encodes rec.Request before the INSERT
-- (base64.StdEncoding.EncodeToString), so the effective payload ceiling on
-- SQL Server was ~190 raw bytes (255 NVARCHAR(255) characters / 4 * 3,
-- base64's 4-characters-per-3-bytes expansion) before encoding -- far below
-- what a durable-call request body can legitimately be.
--
-- Measured against a live SQL Server (Azure SQL Edge, CLEAT_TEST_MSSQL,
-- 2026-08-09) what actually happens on overflow, because it changes the
-- severity and the fix does not follow from the column definition alone:
--
--   INSERT INTO dbo.event_history (..., request, ...) VALUES (..., @tooLong, ...)
--
-- with @tooLong 400 base64 characters against a NVARCHAR(255) column raises
--
--   mssql: String or binary data would be truncated in table
--   'cleat.dbo.event_history', column 'request'. Truncated value: '...'
--
-- and the statement fails -- it does NOT silently truncate. Because this
-- INSERT runs inside appendEventsInTxOpts's transaction
-- (AppendEventHistoryBatch defers tx.Rollback()), the whole batch rolls
-- back: the event is never persisted, not even truncated. So the practical
-- failure mode on unpatched SQL Server is "the durable-call step's event
-- record fails to write and the call errors out", not the checksum-vs-stored-
-- value corruption a silent truncation would have caused (computeEventChecksum
-- in engine/store_promises.go runs over the untruncated in-memory rec.Request
-- before the base64-encoded, width-limited value ever reaches this column --
-- had SQL Server truncated silently, a reload's recomputed checksum would
-- permanently disagree with what was stored, i.e. false corruption on every
-- subsequent VerifyWorkflowEvents. That scenario does not occur here).
--
-- A hard write failure on an oversized request is still wrong -- it is data
-- loss / an availability bug, not a security or integrity one -- and it is
-- also a dialect-parity gap: identical Go code succeeds on PostgreSQL and
-- MySQL and fails on SQL Server purely because of this column's width.
--
-- No index or CHECK constraint references this column's width (verified:
-- `grep -n request migrations/mssql/*.sql` finds no index and no CHECK; the
-- table's ISJSON checks in 001_schema.sql are on workflow_instances'
-- input/result/query_state, not on event_history at all -- request is a
-- base64 blob, never JSON), so widening it is unconstrained.
--
-- Idempotent: ALTER COLUMN to the same type SQL Server already has is a
-- no-op (no error), but this is still guarded on sys.columns so a repeat run
-- costs a catalogue lookup rather than a DDL statement.

IF EXISTS (
    SELECT 1 FROM sys.columns
    WHERE object_id = OBJECT_ID(N'dbo.event_history')
      AND name = N'request'
      AND max_length <> -1
)
    ALTER TABLE dbo.event_history ALTER COLUMN request NVARCHAR(MAX) NULL;
GO
