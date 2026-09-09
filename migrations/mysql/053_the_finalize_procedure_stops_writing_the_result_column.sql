-- Step 1 of 2: finalize_workflow_status stops writing idempotency result.
--
-- cleat-team/cleat#1049. Nine statements wrote this column -- three Go
-- lifecycle methods (one per dialect's CompleteWorkflow) and the
-- finalize_workflow_status procedure on each of the three backends -- and no
-- production code path ever selected it. The only SELECTs of idempotency_keys
-- outside tests read `workflow_id, def_name`, which is what the idempotency
-- lookup actually needs.
--
-- WHAT REPLACES IT: nothing, and that is the point. The `done` branch of the
-- outcome record now writes no idempotency row at all. The `failed` branch is
-- untouched and still writes error_msg through the same tenant predicate, so
-- FailWorkflow and MoveToDeadLetterQueue -- two of the three sites the Finding
-- S1 regression test names -- keep the protection that #1017/#1019 gave them.
--
-- WHY THIS IS NOT A LOSS OF COVERAGE. The tenant-scope regression test
-- (engine/idempotency_update_tenant_scope_test.go) moves from `result` to
-- `error_msg`: the same UPDATE shape, the same WHERE clause, the same
-- three-dialect table, the other branch of the same IF. What does go away is
-- TestIdempotencyResultSurvivesANonJSONResult, which existed only because
-- this column was cast to JSON on the way in (`CAST(p_result AS JSON)` on this backend).
-- error_msg is plain text with no cast and no constraint, so the failure mode that test
-- guarded stops being expressible rather than stopping being tested.
--
-- ORDER MATTERS: this file must sort before 054, which drops the column. The
-- reverse leaves a window in which the live procedure references a column
-- that is already gone.
--
-- WHY THIS IS TWO MIGRATIONS AND NOT ONE. The procedure lives in its own file
-- because engine/store_backends_procedures_test.go re-applies every procedure
-- migration on each test setup, by name. A file in that list runs many times
-- against one database, so it must be idempotent -- CREATE OR REPLACE is,
-- ALTER TABLE ... DROP COLUMN is not (MySQL has no DROP COLUMN IF EXISTS at
-- all). Leaving the procedure out of that list is not an option either: the
-- harness would re-apply the previous definition on top of this one and
-- restore the write to a column that no longer exists.

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
    -- actually matched this caller's (worker_id, generation). If it
    -- matched zero rows, another worker now owns this workflow and none
    -- of these effects (idempotency recording, parent wake, await_child
    -- injection, event history deletion) are safe to apply on its behalf.
    IF v_rows_updated > 0 AND (p_final_status = 'done' OR p_final_status = 'failed') THEN
        -- Record the idempotency outcome. Only a failure carries one now;
        -- #1049 dropped the result column nothing read.
        IF p_final_status = 'failed' THEN
            UPDATE idempotency_keys
            SET error_msg = p_result
            WHERE workflow_id = p_workflow_id
              AND tenant_id = (SELECT tenant_id FROM workflow_instances
                               WHERE id = p_workflow_id);
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


        -- Delete this workflow's events
        DELETE FROM event_history WHERE workflow_id = p_workflow_id;
    END IF;

    -- Report whether the fence held so Go callers can distinguish a lost
    -- fence (normal under reaping) from a real error.
    SELECT v_rows_updated > 0 AS fence_held;
END //

DELIMITER ;
