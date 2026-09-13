-- cleat migration 061 (mysql): record how request/response were encoded.
--
-- See migrations/postgres/067_event_payload_encoding.sql for why. In short:
-- tryDecodeBase64 fell back on decode FAILURE, and ordinary text decodes fine --
-- every four-character alphanumeric string is valid base64 -- so a legacy
-- plaintext value could be silently replaced with different bytes (cleat#1319).
--
-- NULL means the row predates the column and its encoding is genuinely unknown,
-- so reads keep the historical fallback. 1 = base64, 0 = plaintext (reserved).
-- Deliberately not backfilled: a backfill would have to answer the very
-- question the column exists because nobody can answer.
--
-- TINYINT rather than BOOLEAN, which MySQL aliases to TINYINT(1) anyway; the
-- wider domain leaves room for a future encoding.
ALTER TABLE event_history ADD COLUMN payload_encoding TINYINT NULL
    COMMENT 'NULL = unknown (pre-cleat#1319 row), 1 = base64, 0 = plaintext';
