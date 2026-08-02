-- cleat migration 004 (mysql): fix finalize_workflow_status generation fence
--
-- Bug: the generation-fenced status UPDATE inside finalize_workflow_status
-- correctly matches zero rows when a stale caller's (worker_id, generation)
-- no longer owns the workflow (e.g. the worker stalled, was reaped, and the
-- workflow was reclaimed by another worker). But the terminal side-effect
-- block that follows -- idempotency-key overwrite, parent wake, injecting
-- this caller's result into the parent's await_child event, and
-- unconditionally DELETE FROM event_history for the workflow -- ran
-- regardless of whether the fenced UPDATE actually matched. A stalled
-- worker calling this procedure after being reaped and reclaimed could
-- therefore delete the new owner's live event history and inject its own
-- stale result into the parent workflow.
--
-- Fix: capture the fenced UPDATE's affected-row count via ROW_COUNT() and
-- skip the entire terminal side-effect block when it is zero. The
-- procedure now returns a single-row, single-column result set (via a
-- trailing SELECT) indicating whether the fence held, so Go callers can
-- detect a lost fence and treat it as the normal "reassigned to another
-- worker" case rather than an error.

DROP PROCEDURE IF EXISTS finalize_workflow_status;

DELIMITER //

CREATE PROCEDURE finalize_workflow_status(
    p_workflow_id      VARCHAR(255),
    p_worker_id        VARCHAR(255),
    p_generation       BIGINT,
    p_final_status     VARCHAR(32),
    p_result           LONGTEXT,
    p_error_code       VARCHAR(255),
    p_error_op         VARCHAR(255),
    p_query_state      JSON,
    p_next_wake_at     DATETIME(6),
    p_notify_channel   VARCHAR(255)
)
BEGIN
    DECLARE v_rows_updated INT DEFAULT 0;

    -- Update workflow status, fenced on (assigned_to, generation) so a
    -- caller that no longer owns the workflow cannot modify it.
    CASE p_final_status
        WHEN 'done' THEN
            UPDATE workflow_instances
            SET status = 'done',
                result = CAST(p_result AS JSON),
                completed_at = NOW(6),
                assigned_to = NULL,
                query_state = CAST(p_query_state AS JSON)
            WHERE id = p_workflow_id
              AND assigned_to = p_worker_id
              AND generation = p_generation;

        WHEN 'failed' THEN
            UPDATE workflow_instances
            SET status = 'failed',
                error_msg = p_result,
                error_code = p_error_code,
                error_op = p_error_op,
                completed_at = NOW(6),
                assigned_to = NULL,
                query_state = CAST(p_query_state AS JSON)
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
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'finalize_workflow_status: unknown final status';
    END CASE;

    SET v_rows_updated = ROW_COUNT();

    -- Terminal status side-effects -- only run if the fenced UPDATE above
    -- actually matched this caller's (worker_id, generation). If it
    -- matched zero rows, another worker now owns this workflow and none
    -- of these effects (idempotency recording, parent wake, await_child
    -- injection, event history deletion) are safe to apply on its behalf.
    IF v_rows_updated > 0 AND (p_final_status = 'done' OR p_final_status = 'failed') THEN
        -- Record idempotency outcome
        IF p_final_status = 'done' THEN
            UPDATE idempotency_keys
            SET result = CAST(p_result AS JSON)
            WHERE workflow_id = p_workflow_id;
        ELSE
            UPDATE idempotency_keys
            SET error_msg = p_result
            WHERE workflow_id = p_workflow_id;
        END IF;

        -- Wake parent workflow atomically
        UPDATE workflow_instances
        SET next_wake_at = NOW(6)
        WHERE id = (
            SELECT parent_id FROM (
                SELECT parent_workflow_id AS parent_id
                FROM workflow_instances
                WHERE id = p_workflow_id
            ) AS tmp
        )
        AND status IN ('ready', 'suspended');

        -- Populate parent's await_child event result
        UPDATE event_history
        SET response = p_result
        WHERE workflow_id = (
            SELECT parent_id FROM (
                SELECT parent_workflow_id AS parent_id
                FROM workflow_instances
                WHERE id = p_workflow_id
            ) AS tmp
        )
        AND event_type = 'await_child'
        AND run_id = p_workflow_id
        AND (response IS NULL OR response = '');

        -- Delete this workflow's events
        DELETE FROM event_history WHERE workflow_id = p_workflow_id;
    END IF;

    -- Report whether the fence held so Go callers can distinguish a lost
    -- fence (normal under reaping) from a real error.
    SELECT v_rows_updated > 0 AS fence_held;
END //

DELIMITER ;
