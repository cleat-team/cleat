-- Migration 012: PL/pgSQL function for the terminal UPDATE portion of
-- FinalizeWorkflowSegment.  Event INSERTs are still handled in Go
-- (appendEventsInTx); this function bundles the status update, idempotency
-- recording, parent wake, await_child population, and pg_notify into one
-- server-side call, replacing 5 individual round-trips with 1.
--
-- Called within an existing transaction after events have been appended and
-- event_count has been incremented.

CREATE OR REPLACE FUNCTION finalize_workflow_status(
    p_workflow_id      TEXT,
    p_worker_id        TEXT,
    p_generation       BIGINT,
    p_final_status     TEXT,           -- 'done', 'failed', or 'ready'
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
        -- Record idempotency outcome
        IF p_final_status = 'done' THEN
            UPDATE idempotency_keys
            SET result = p_result
            WHERE workflow_id = p_workflow_id;
        ELSE
            UPDATE idempotency_keys
            SET error_msg = p_result
            WHERE workflow_id = p_workflow_id;
        END IF;

        -- Wake parent workflow atomically
        UPDATE workflow_instances
        SET next_wake_at = now()
        WHERE id = (
            SELECT parent_workflow_id FROM workflow_instances WHERE id = p_workflow_id
        )
        AND status IN ('ready', 'suspended');

        -- Populate parent's await_child event result
        UPDATE event_history
        SET response = p_result
        WHERE workflow_id = (
            SELECT parent_workflow_id FROM workflow_instances WHERE id = p_workflow_id
        )
        AND event_type = 'await_child'
        AND run_id = p_workflow_id
        AND (response IS NULL OR response = '');
    END IF;

    -- Dispatch hint
    IF p_notify_channel IS NOT NULL AND p_notify_channel != '' THEN
        PERFORM pg_notify(p_notify_channel, '');
    END IF;
END;
$$ LANGUAGE plpgsql;
