-- Migration 014: Delete events from event_history when a workflow
-- reaches a terminal state (done/failed).  Once a workflow is final,
-- its events are no longer needed for replay and only bloat the table,
-- slowing down per-step INSERTs via FK checks and index maintenance.
--
-- The deletion happens inside the same transaction as FinalizeWorkflowSegment
-- so it is atomic with the status update — if the transaction rolls back,
-- the events are preserved.
--
-- Also drops the foreign key from event_history to workflow_instances.
-- Every per-step flushEvent INSERT paid the cost of a B-tree lookup
-- against a growing workflow_instances table.  Event cleanup on terminal
-- status removes the orphan risk that the FK was guarding against,
-- so the FK provides no benefit on the happy path.

ALTER TABLE event_history DROP CONSTRAINT IF EXISTS fk_event_history_workflow;

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
) RETURNS VOID AS $$
BEGIN
    -- Update workflow status
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

    -- Terminal status side-effects
    IF p_final_status = 'done' OR p_final_status = 'failed' THEN
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

        -- Delete this workflow's events — they are no longer needed
        -- for replay once the workflow has reached a terminal state.
        -- This keeps event_history bounded to active workflows only,
        -- preventing unbounded table growth that slows per-step INSERTs.
        DELETE FROM event_history WHERE workflow_id = p_workflow_id;
    END IF;

    -- Dispatch hint
    IF p_notify_channel IS NOT NULL AND p_notify_channel != '' THEN
        PERFORM pg_notify(p_notify_channel, '');
    END IF;
END;
$$ LANGUAGE plpgsql;
