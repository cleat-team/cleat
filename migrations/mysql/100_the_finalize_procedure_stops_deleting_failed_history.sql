-- finalize_workflow_status no longer has a 'failed' branch.
--
-- cleat#1973. No production code path ever calls this procedure with
-- p_final_status = 'failed'. FinalizeWorkflowSegment's one production call
-- site (cmd/cleat-worker/setup.go) only ever passes 'done' or 'ready'; the
-- real failure path is store.FailWorkflow (engine/mysql_lifecycle.go), a
-- plain UPDATE that never calls this procedure at all. So the 'failed'
-- branch below -- including its event_history DELETE -- has never run in
-- production. See migrations/postgres/101_the_finalize_procedure_stops_deleting_failed_history.sql
-- for the full reasoning; this is the same change on MySQL.
--
-- A dormant delete is the risk, not a current bug: removing it is removing a
-- branch that one call-site change would silently reactivate, deleting a
-- failed workflow's replay history at finalize instead of leaving it for
-- --retention-days to sweep (cleat#1973's corrected behaviour).
--
-- WHAT CHANGES, versus 071:
--   * The 'failed' arm of the status CASE is gone. A caller that passes
--     'failed' now hits the ELSE and gets the same "unknown final status"
--     SIGNAL any other unrecognized value gets.
--   * The terminal-status IF narrows from
--     (p_final_status = 'done' OR p_final_status = 'failed') to
--     p_final_status = 'done' alone.
--   * The idempotency_keys error_msg UPDATE that only ran for 'failed' is
--     removed with it -- store.FailWorkflow's own Go code already writes
--     that same UPDATE on the actual failure path.
--
-- WHAT DOES NOT CHANGE: the 'done' branch, byte for byte, and its DELETE FROM
-- event_history.
--
-- Everything else here is 071 verbatim, minus the 'failed' branch. Whole
-- redefinition rather than a patch, same as 071/053/075: the procedure is
-- CREATE (after DROP), and engine/store_backends_procedures_test.go
-- re-applies every listed procedure migration by name, so this file must be
-- idempotent and complete on its own.

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

    -- Terminal status side-effects -- only run if the fenced UPDATE above
    -- actually matched this caller's (worker_id, generation), and only for
    -- 'done': a real failure never reaches this procedure (cleat#1973).
    IF v_rows_updated > 0 AND p_final_status = 'done' THEN
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


        -- Delete this workflow's events
        DELETE FROM event_history WHERE workflow_id = p_workflow_id;
    END IF;

    -- Report whether the fence held so Go callers can distinguish a lost
    -- fence (normal under reaping) from a real error.
    SELECT v_rows_updated > 0 AS fence_held;
END //

DELIMITER ;
