-- cleat#3171. See migrations/postgres/014 for the full rationale -- this is
-- signal_seq's sibling (cleat#953) on the promise/update paths, on the MySQL
-- dialect. Only the 'ready' arm's CASE WHEN changes, adding one OR clause.
-- Everything else here is the LAST finalize_workflow_status body on develop
-- (migrations/mysql/003_procedures.sql), reproduced whole because MySQL has
-- no CREATE OR ALTER PROCEDURE -- the routine is dropped and recreated.
DROP PROCEDURE IF EXISTS finalize_workflow_status;

DELIMITER ;;
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

    CASE p_final_status
        WHEN 'done' THEN
            UPDATE workflow_instances
            SET status = 'done',
                result = p_result,
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
                next_wake_at = CASE
                                    WHEN signal_seq <> signal_seq_at_claim
                                      OR signal_consumed_seq <> signal_consumed_at_claim
                                      OR promise_seq <> promise_seq_at_claim
                                    THEN NOW(6) ELSE p_next_wake_at END,
                query_state = CAST(p_query_state AS JSON)
            WHERE id = p_workflow_id
              AND assigned_to = p_worker_id
              AND generation = p_generation;

        ELSE
            SIGNAL SQLSTATE '45000'
                SET MESSAGE_TEXT = 'finalize_workflow_status: unknown final status';
    END CASE;

    SET v_rows_updated = ROW_COUNT();

    IF v_rows_updated > 0 AND p_final_status = 'done' THEN
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

        DELETE FROM event_history WHERE workflow_id = p_workflow_id;
    END IF;

    SELECT v_rows_updated > 0 AS fence_held;
END ;;
DELIMITER ;
