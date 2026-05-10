-- Migration 011: Add checksum column to event_history for integrity verification
-- (T-SQL for SQL Server 2017+ / Azure SQL Database)
-- Enables SHA-256 integrity verification of event records.

-- ===========================================================================
-- Add checksum column to event_history
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'checksum')
    ALTER TABLE dbo.event_history ADD checksum NVARCHAR(MAX) NULL;
