-- cleat migration 043 (postgres): a suspending segment stores its query state
--
-- finalize_workflow_status writes query_state on the 'done' and 'failed'
-- branches and NOT on 'ready', which is the branch a suspension takes. So a
-- workflow's queryable state reached the database only when the workflow
-- reached a terminal status -- by which point its result is available and the
-- query is the least useful it will ever be.
--
-- That inverts what the mechanism is for. docs/determinism.md:
--
--   the workflow proactively records queryable state to the database at points
--   of its choosing, and that state is durable and externally readable -- via
--   GetQueryState or GET /api/workflows/:id/query?key=X -- REGARDLESS OF
--   WHETHER ANY WORKER CURRENTLY HAS THE WORKFLOW LOADED. It answers "what is
--   this workflow's status" without needing anything to be running at query
--   time.
--
-- A suspended workflow is precisely the case that sentence names, and it was
-- the one case that returned nothing. Measured 2026-09-06 on PostgreSQL: a
-- workflow that set phase="started" and then slept read back "" for phase
-- while suspended, and "finished" only after it completed.
--
-- All three dialects had it, identically, so this is a template that omitted
-- the assignment rather than a divergence between them.
--
-- The write is unconditional, matching the terminal branches. It cannot blank
-- previously stored state: query state is not a durable event, it is rebuilt
-- by re-executing the body, so a resumed segment replays every SetQueryState
-- call made before its suspension point and arrives here with the full map.
-- The one caller that must NOT write it is the defer phase, which passes none
-- and goes through FinalizeDeferPhase rather than this routine -- see the
-- DeferPhaseStore doc comment, which says so for this exact reason.
--
-- Only the 'ready' branch changes. Everything else is 004 verbatim.

-- No DROP here. 004 needed one because it changed the return type from VOID to
-- BOOLEAN, which CREATE OR REPLACE rejects (42P13). This migration changes only
-- the body, so REPLACE is enough -- and dropping a function other objects may
-- depend on, to put back an identical signature, is a risk taken for nothing.
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
                assigned_to = NULL,
                query_state = p_query_state
            WHERE id = p_workflow_id
              AND assigned_to = p_worker_id
              AND generation = p_generation;

        WHEN 'ready' THEN
            UPDATE workflow_instances
            SET status = 'ready',
                assigned_to = NULL,
                next_wake_at = p_next_wake_at,
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
        -- Record idempotency outcome (before deleting events, since
        -- the parent's await_child event references this workflow).
        IF p_final_status = 'done' THEN
            UPDATE idempotency_keys
            SET result = p_result::jsonb
            WHERE workflow_id = p_workflow_id;
        ELSE
            UPDATE idempotency_keys
            SET error_msg = p_result
            WHERE workflow_id = p_workflow_id;
        END IF;

        -- Wake parent workflow atomically.
        UPDATE workflow_instances
        SET next_wake_at = now()
        WHERE id = (
            SELECT parent_workflow_id FROM workflow_instances WHERE id = p_workflow_id
        )
        AND status IN ('ready', 'suspended');

        -- Populate parent's await_child event result (reads from the
        -- parent's events, not the child's, so this is safe to run
        -- before deleting the child's events).
        UPDATE event_history
        SET response = p_result
        WHERE workflow_id = (
            SELECT parent_workflow_id FROM workflow_instances WHERE id = p_workflow_id
        )
        AND event_type = 'await_child'
        AND run_id = p_workflow_id
        AND (response IS NULL OR response = '');

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
