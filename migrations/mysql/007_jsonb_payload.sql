-- cleat MySQL migration 007: event payload
-- Add JSON payload column to event_history for structured event-type-specific data.
-- Dual-write period: old columns + new payload column are both populated.
-- After all workflows have migrated, the old columns can be dropped.
--
-- MySQL differences from PostgreSQL:
--   - JSONB becomes JSON
--   - ADD COLUMN IF NOT EXISTS becomes ADD COLUMN with safety comment
--   - Idempotent: verify column absence before running

-- ensure column does not exist before running
ALTER TABLE event_history ADD COLUMN payload JSON;
