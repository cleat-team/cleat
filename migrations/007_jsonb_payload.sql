-- Add JSONB payload column to event_history for structured event-type-specific data.
-- Dual-write period: old columns + new payload column are both populated.
-- After all workflows have migrated, the old columns can be dropped.
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS payload JSONB;
