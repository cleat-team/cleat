-- cleat migration 004 (postgres): fix finalize_workflow_status generation fence
--
-- Bug: the generation-fenced status UPDATE inside finalize_workflow_status
-- correctly matches zero rows when a stale caller's (worker_id, generation)
-- no longer owns the workflow (e.g. the worker stalled, was reaped, and the
-- workflow was reclaimed by another worker). But the terminal side-effect
-- block that follows -- idempotency-key overwrite, parent wake, injecting
-- this caller's result into the parent's await_child event, and
-- unconditionally DELETE FROM event_history for the workflow -- ran
-- regardless of whether the fenced UPDATE actually matched. A stalled
-- worker calling this function after being reaped and reclaimed could
-- therefore delete the new owner's live event history and inject its own
-- stale result into the parent workflow.
--
-- Fix: capture the fenced UPDATE's affected-row count via GET DIAGNOSTICS
-- and skip the entire terminal side-effect block when it is zero. The
-- function now returns BOOLEAN (previously VOID) indicating whether the
-- fence held, so Go callers can detect a lost fence and treat it as the
-- normal "reassigned to another worker" case rather than an error.

-- 003 defines this function RETURNS VOID; this one returns BOOLEAN.
-- PostgreSQL rejects a return-type change through CREATE OR REPLACE:
--   ERROR: cannot change return type of existing function (42P13)
--   HINT:  Use DROP FUNCTION finalize_workflow_status(...) first.
-- so the old signature must be dropped first. The argument list below must
-- match 003's exactly, or the DROP matches nothing and the CREATE fails again.
-- Pin the creation target; see the note in 001_schema.sql. The default
-- search_path is "$user", public, so unqualified names below would resolve
-- against a schema named after the connecting role -- and 001 creates a schema
-- called "cleat" while the shipped compose connects as POSTGRES_USER=cleat.
SET search_path = public;

DROP FUNCTION IF EXISTS finalize_workflow_status(
    TEXT, TEXT, BIGINT, TEXT, TEXT, TEXT, TEXT, JSONB, TIMESTAMPTZ, TEXT
);

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
                next_wake_at = p_next_wake_at
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
