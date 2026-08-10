-- cleat MySQL stored procedures (003)
-- Merge of: 010_finalize_workflow_segment_proc
--
-- Called within an existing transaction after events have been appended and
-- event_count has been incremented.  Tenant isolation is handled at the
-- database level (each tenant has its own database), so no explicit
-- tenant_id filter is needed inside the body.

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
    -- Update workflow status
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

    -- Terminal status side-effects
    IF p_final_status = 'done' OR p_final_status = 'failed' THEN
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
END //

DELIMITER ;
