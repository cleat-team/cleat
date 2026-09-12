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
-- this column was cast to JSON on the way in (`p_result::jsonb` on this backend).
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

-- WHERE THIS CREATE FUNCTION LANDS is load-bearing rather than tidy.
--
-- CREATE FUNCTION *creates* in the first schema of search_path; it does not
-- resolve an existing one the way ALTER TABLE does. The cluster's database
-- user is `cleat` and 001_schema.sql creates a schema of that name, so under
-- the default "$user", public an unqualified CREATE OR REPLACE lands in
-- `cleat` -- leaving the real function untouched and adding a SECOND function
-- with an identical argument list. Callers keep resolving the old one.
--
-- This file used to open with `SET search_path = public;` for that reason, as
-- every earlier migration defining this function did. It no longer needs to:
-- migration.Runner sets search_path before applying any file (cleat#1287), so
-- the CREATE still has a definite target and that target now follows --schema.
-- 051 and 052 never pinned, and were right not to, because ALTER TABLE on an
-- unqualified name falls through rather than creating.
--
-- It cost a red Cluster Integration job to find, and it could not fail
-- anywhere else: engine tests connect as `postgres`, no schema of that name
-- exists, so "$user" resolves to nothing and the function lands in public
-- either way. The bug needed a database whose user owns a schema.

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
                next_wake_at = CASE
                                    WHEN signal_seq <> signal_seq_at_claim
                                      OR signal_consumed_seq <> signal_consumed_at_claim
                                    THEN now() ELSE p_next_wake_at END,
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
        -- Record the idempotency outcome. Only a failure carries one
        -- now; #1049 dropped the result column nothing read. Still before
        -- the event delete, since the parent's await_child event
        -- references this workflow.
        IF p_final_status = 'failed' THEN
            UPDATE idempotency_keys
            SET error_msg = p_result
            WHERE workflow_id = p_workflow_id
              AND tenant_id = (SELECT tenant_id FROM workflow_instances
                               WHERE id = p_workflow_id);
        END IF;

        -- Wake parent workflow atomically.
        UPDATE workflow_instances
        SET next_wake_at = now()
        WHERE id = (
            SELECT parent_workflow_id FROM workflow_instances WHERE id = p_workflow_id
        )
        AND status IN ('ready', 'suspended');

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
