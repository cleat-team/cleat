-- cleat migration 064 (mssql): record how request/response were encoded.
--
-- See migrations/postgres/067_event_payload_encoding.sql for why. In short:
-- tryDecodeBase64 fell back on decode FAILURE, and ordinary text decodes fine --
-- every four-character alphanumeric string is valid base64 -- so a legacy
-- plaintext value could be silently replaced with different bytes (cleat#1319).
--
-- NULL means the row predates the column and its encoding is genuinely unknown,
-- so reads keep the historical fallback. 1 = base64, 0 = plaintext (reserved).
-- Deliberately not backfilled.
--
-- SMALLINT rather than BIT: BIT cannot carry a third state usefully and cannot
-- be widened later without another migration on three dialects.
IF NOT EXISTS (
    SELECT 1 FROM sys.columns
    WHERE object_id = OBJECT_ID('dbo.event_history') AND name = 'payload_encoding'
)
BEGIN
    ALTER TABLE dbo.event_history ADD payload_encoding SMALLINT NULL;
END;
