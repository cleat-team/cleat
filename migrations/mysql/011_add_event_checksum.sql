-- cleat MySQL migration 011: event checksum
-- Add checksum column to event_history for SHA-256 integrity verification.
--
-- MySQL differences from PostgreSQL:
--   - ADD COLUMN IF NOT EXISTS becomes ADD COLUMN with safety comment
--   - PG's digest() function is not used; checksums are computed in Go code
--   - Idempotent: verify column absence before running

-- ensure column does not exist before running
ALTER TABLE event_history ADD COLUMN checksum TEXT;

-- Checksums are computed and verified in Go application code. The PostgreSQL
-- digest() function for SHA-256 has no MySQL equivalent here.
