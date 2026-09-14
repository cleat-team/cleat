-- ===========================================================================
-- 064: a completed run names the worker that ran it
--
-- cleat#1118, on the owner decision of 2026-09-10 (option a: a durable record).
--
-- WHY THIS IS NOT SIMPLY A MISSING COLUMN. workflow_instances.assigned_to is a
-- LEASE, not an audit field, and every terminal write clears it *while fencing
-- on it*:
--
--     UPDATE workflow_instances
--     SET status = '\''done'\'', ..., assigned_to = NULL
--     WHERE id = $1 AND assigned_to = $2 AND generation = $3
--
-- The worker identity authorises the terminal write and is erased by the same
-- statement. Measured on a ports database of 185 terminal runs: assigned_to
-- blank on 185 of 185, across done, failed, terminated and dead_lettered.
--
-- So the record has to be taken at exactly the moment the lease is surrendered.
-- Every terminal write now carries `completed_by = assigned_to` alongside the
-- `assigned_to = NULL` it already had. Reading it from assigned_to rather than
-- from the worker parameter is deliberate: in the fenced paths the WHERE
-- already guarantees they are equal, and it is the only form that also works
-- for the ADMIN TERMINATE path, which has no worker parameter at all but does
-- hold the lease it is about to clear.
--
-- NULL means no worker ever held it -- a run terminated before it was claimed.
-- That is a real state and is why the column is nullable.
--
-- sticky_worker_id does NOT answer this. It records which worker a run MUST
-- use, not which one ran it, and it is blank unless sticky routing is opted
-- into.
-- ===========================================================================

-- MySQL 8.0 has no ADD COLUMN IF NOT EXISTS, and re-applying the whole set to
-- an existing database is exactly what an operator upgrading does. Guarded
-- through information_schema and dynamic SQL, matching 020_event_intent.sql.
SET @add_completed_by := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_instances ADD COLUMN completed_by VARCHAR(255) NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_instances'
      AND COLUMN_NAME = 'completed_by'
);
PREPARE add_completed_by FROM @add_completed_by;
EXECUTE add_completed_by;
DEALLOCATE PREPARE add_completed_by;
