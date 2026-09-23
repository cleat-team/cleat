-- ===========================================================================
-- 101: finalize_workflow_status no longer has a 'failed' branch
--
-- cleat#1973. No production code path ever calls this procedure with
-- p_final_status = 'failed'. FinalizeWorkflowSegment's one production call
-- site (cmd/cleat-worker/setup.go) only ever passes 'done' or 'ready'; the
-- real failure path is store.FailWorkflow (engine/store_lifecycle.go), a
-- plain UPDATE that never calls this procedure at all. So the 'failed'
-- branch below -- including its event_history DELETE -- has never run in
-- production.
--
-- A DORMANT DELETE IS THE RISK, NOT A CURRENT BUG. Removing it is not fixing
-- something broken; it is removing a branch that ONE call-site change would
-- silently reactivate, deleting a failed workflow's replay history at
-- finalize again -- exactly the behaviour cleat#1973 corrects the docs and
-- comments to say does NOT happen. A workflow's failure history is meant to
-- survive until --retention-days sweeps it (engine/db.go's
-- DeleteExpiredEvents, which already matches 'failed' rows); a call site
-- that started passing 'failed' here would bypass that policy entirely and
-- fail silently -- no error, just a workflow whose history vanished at the
-- moment it failed instead of 30 days later.
--
-- WHAT CHANGES, versus 075:
--   * The 'failed' arm of the status CASE is gone. A caller that passes
--     'failed' now hits the ELSE and gets the same "unknown final status"
--     exception any other unrecognized value gets -- loud, not silent.
--   * The terminal-status IF narrows from
--     (p_final_status = 'done' OR p_final_status = 'failed') to
--     p_final_status = 'done' alone, so the parent-wake and event_history
--     DELETE below it only ever run for 'done'.
--   * The idempotency_keys error_msg UPDATE that only ran for 'failed' is
--     removed with it. It is not a loss: store.FailWorkflow's own Go code
--     (engine/store_lifecycle.go) already writes that same UPDATE, unrelated
--     to this procedure, on the actual failure path.
--
-- WHAT DOES NOT CHANGE: the 'done' branch, byte for byte, and its DELETE FROM
-- event_history -- a done workflow's history is still purged at finalize,
-- same as always.
--
-- EXTRACTED, NOT RETYPED, per this repo's own rule about a body superseded
-- across several migrations:
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
-- 075's body is the base; the 'failed' arm and its two conditions are the
-- only removals.
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
    -- actually matched this caller's (worker_id, generation), and only for
    -- 'done': a real failure never reaches this procedure (cleat#1973), so
    -- there is no 'failed' case left to guard here.
    IF v_rows_updated > 0 AND p_final_status = 'done' THEN
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
