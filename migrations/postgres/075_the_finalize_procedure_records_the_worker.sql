-- ===========================================================================
-- 075: finalize_workflow_status records which worker finished the run
--
-- cleat#1118. 074 added the column; this fills it on PostgreSQL's SEGMENT
-- finalize path, which goes through this procedure rather than through Go.
--
-- WHY A SEPARATE MIGRATION FROM 074. 074 is an ALTER TABLE and this is a
-- CREATE OR REPLACE of a procedure whose body has been superseded four times
-- since 003 (by 004, 043, 044, 047, 049, 050 and 053). The body below is
-- 053's -- the authoritative one -- with two lines added, and it was EXTRACTED
-- rather than retyped: re-deriving it by hand across four supersessions is how
-- a migration silently reverts three others.
--
--     python3 - <<'EOF'
--     import re, glob, os
--     def strip(s):
--         s = re.sub(r'/\*.*?\*/', '', s, flags=re.S)
--         return '\n'.join(re.sub(r'--.*$', '', l) for l in s.split('\n'))
--     for f in sorted(glob.glob('migrations/postgres/*.sql')):
--         if re.search(r'CREATE\s+(OR\s+REPLACE\s+)?(FUNCTION|PROCEDURE)\s+\S*finalize_workflow_status',
--                      strip(open(f).read()), re.I):
--             print(os.path.basename(f))   # the LAST line is authoritative
--     EOF
--
-- TWO BRANCHES OF THREE. 'done' and 'failed' are terminal and record the
-- worker. 'ready' is NOT terminal -- the run goes back on the queue -- so
-- recording there would name whoever yielded, which is a different question
-- and not the one cleat#1118 asks.
--
-- completed_by = assigned_to, AND THE ORDER IS LOAD-BEARING ON ONE DIALECT.
-- PostgreSQL and SQL Server evaluate every right-hand side against the OLD
-- row, so the order within SET is irrelevant there. MySQL does not: it assigns
-- left to right and later assignments see earlier ones. Measured:
--
--     SET c = a, a = NULL   ->  pg w1   mssql w1   mysql w1
--     SET a = NULL, c = a   ->  pg w2   mssql w2   mysql <null>
--
-- So `completed_by = assigned_to` must precede `assigned_to = NULL`
-- everywhere, and a reordering would fail on MySQL alone, silently, as a blank
-- column. The guard test asserts the ORDER rather than the presence.
-- ===========================================================================

CREATE OR REPLACE FUNCTION finalize_workflow_status(
    p_workflow_id      TEXT,
    p_worker_id        TEXT,
    p_generation       BIGINT,
    p_final_status     TEXT,
    p_result           TEXT,
    p_error_code       TEXT,
    p_error_op         TEXT,
    p_query_state      JSONB,
    p_next_wake_at     TIMESTAMPTZ,
    p_notify_channel   TEXT
) RETURNS BOOLEAN AS $$
DECLARE
    v_rows_updated INT;
BEGIN
    -- Update workflow status, fenced on (assigned_to, generation) so a
    -- caller that no longer owns the workflow cannot modify it.
    CASE p_final_status
        WHEN 'done' THEN
            UPDATE workflow_instances
            SET status = 'done',
                result = p_result::jsonb,
                completed_at = now(),
                completed_by = assigned_to,
                assigned_to = NULL,
                query_state = p_query_state
            WHERE id = p_workflow_id
              AND assigned_to = p_worker_id
              AND generation = p_generation;

        WHEN 'failed' THEN
            UPDATE workflow_instances
            SET status = 'failed',
                error_msg = p_result,
                error_code = p_error_code,
                error_op = p_error_op,
                completed_at = now(),
                completed_by = assigned_to,
                assigned_to = NULL,
                query_state = p_query_state
            WHERE id = p_workflow_id
              AND assigned_to = p_worker_id
              AND generation = p_generation;

        WHEN 'ready' THEN
            UPDATE workflow_instances
            SET status = 'ready',
                assigned_to = NULL,
                next_wake_at = CASE
                                    WHEN signal_seq <> signal_seq_at_claim
                                      OR signal_consumed_seq <> signal_consumed_at_claim
                                    THEN now() ELSE p_next_wake_at END,
                query_state = p_query_state
            WHERE id = p_workflow_id
              AND assigned_to = p_worker_id
              AND generation = p_generation;

        ELSE
            RAISE EXCEPTION 'finalize_workflow_status: unknown final status: %', p_final_status;
    END CASE;

    GET DIAGNOSTICS v_rows_updated = ROW_COUNT;

    -- Terminal status side-effects -- only run if the fenced UPDATE above
    -- actually matched this caller's (worker_id, generation). If it
    -- matched zero rows, another worker now owns this workflow and none
    -- of these effects (idempotency recording, parent wake, await_child
    -- injection, event history deletion) are safe to apply on its behalf.
    IF v_rows_updated > 0 AND (p_final_status = 'done' OR p_final_status = 'failed') THEN
        -- Record the idempotency outcome. Only a failure carries one
        -- now; #1049 dropped the result column nothing read. Still before
        -- the event delete, since the parent's await_child event
        -- references this workflow.
        IF p_final_status = 'failed' THEN
            UPDATE idempotency_keys
            SET error_msg = p_result
            WHERE workflow_id = p_workflow_id
              AND tenant_id = (SELECT tenant_id FROM workflow_instances
                               WHERE id = p_workflow_id);
        END IF;

        -- Wake parent workflow atomically.
        UPDATE workflow_instances
        SET next_wake_at = now()
        WHERE id = (
            SELECT parent_workflow_id FROM workflow_instances WHERE id = p_workflow_id
        )
        AND status IN ('ready', 'suspended');

        -- Delete this workflow's events -- they are no longer needed
        -- for replay once the workflow has reached a terminal state.
        -- This keeps event_history bounded to active workflows only,
        -- preventing unbounded table growth that slows per-step INSERTs.
        DELETE FROM event_history WHERE workflow_id = p_workflow_id;
    END IF;

    -- Dispatch hint
    IF p_notify_channel IS NOT NULL AND p_notify_channel != '' THEN
        PERFORM pg_notify(p_notify_channel, '');
    END IF;

    RETURN v_rows_updated > 0;
END;
$$ LANGUAGE plpgsql;
