-- MSSQL migration 013: Drop JSON CHECK constraints on event_history.
-- Event data is stored as base64-encoded strings, not JSON documents.
-- The ISJSON() check constraint rejects bare base64 strings.
IF EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_event_history_request')
    ALTER TABLE dbo.event_history DROP CONSTRAINT ck_event_history_request;

IF EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_event_history_response')
    ALTER TABLE dbo.event_history DROP CONSTRAINT ck_event_history_response;
