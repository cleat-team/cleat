-- cleat migration 099 (mssql): a swept history leaves a marker
--
-- See migrations/postgres/100_a_swept_history_leaves_a_marker.sql for the
-- full reasoning: cleat#2038, nullable history_swept_at with no backfill, set
-- by DeleteExpiredEvents alongside its existing DELETE so ReReplay's
-- pending-intent guard can tell "never attempted" from "swept, cannot tell"
-- rather than treating both as empty history. Retention semantics (what
-- --retention-days deletes) are unchanged.
--
-- DATETIMEOFFSET NULL, matching completed_at/compacted_at (001_schema.sql).
--
-- GUARDED, following 098's own precedent.

IF COL_LENGTH(N'dbo.workflow_instances', N'history_swept_at') IS NULL
    ALTER TABLE dbo.workflow_instances
        ADD history_swept_at DATETIMEOFFSET NULL;
GO
