-- cleat migration 042 (mysql): a suspending segment stores its query state
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
                next_wake_at = p_next_wake_at,
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
