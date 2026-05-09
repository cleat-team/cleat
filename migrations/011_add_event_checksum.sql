-- Migration 011: Add checksum column to event_history for integrity verification.
-- This enables SHA-256 integrity verification of event records.
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS checksum TEXT;
