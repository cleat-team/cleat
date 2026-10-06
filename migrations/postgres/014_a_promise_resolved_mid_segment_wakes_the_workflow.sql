-- cleat#3171: a promise resolved/rejected, or an update dispatched, while the
-- workflow was AWAKE scheduled no wake. This is signal_seq's sibling
-- (cleat#953) on the promise/update paths.
--
-- ResolvePromise, RejectPromise and CreateUpdateRequest each pull
-- next_wake_at forward only for a workflow that is already suspended --
-- 'running' is excluded, because a claimed workflow's row is about to be
-- overwritten here anyway. That left a window: a resolution/dispatch arriving
-- mid-segment scheduled nothing, this procedure then wrote the workflow's own
-- timeout deadline over next_wake_at, and the settlement sat durable until
-- that timeout expired.
--
-- promise_seq is bumped by every one of the three writes
-- (engine/store_promises.go et al); promise_seq_at_claim is stamped by every
-- claim. Different means a settlement landed while this segment was running,
-- so the workflow wakes now instead of at its deadline.
--
-- THE COMPARISON IS IN THIS TRANSACTION, same reason as signal_seq: a
-- post-commit poll closes the same window in the steady state but loses the
-- wake entirely if the worker dies between the commit and the poll.
--
-- A COUNTER rather than "does workflow_promises/workflow_update_requests have
-- a row", which spins the same way the signal case would have: a workflow
-- awaiting promise {a} with an unrelated promise {z} pending would wake, poll,
-- find nothing it wants, re-suspend, and repeat. The counter only moves on a
-- NEW settlement.
--
-- SCOPE IS THE #981 SHAPE ONLY (one counter, no drain/burst handling).
-- cleat#953's burst extension (#985, signal_consumed_seq) exists because
-- signals have a separate poll-then-consume step whose consumption can itself
-- race finalize; promises and update requests have no such second step --
-- ResolvePromise/RejectPromise/CreateUpdateRequest are each one write, and
-- nothing "consumes" a settled promise the way ConsumeSignal removes a
-- delivered signal. If a burst-shaped gap is later measured here, it is a
-- new issue, not an omission from this one.
--
-- Only the 'ready' arm's CASE WHEN changes, adding one OR clause. Everything
-- else here is 011_tenant_allow_public_exposure.sql's effect on the schema
-- plus the LAST finalize_workflow_status body on develop (migrations/postgres/003_procedures.sql),
-- reproduced whole because the function is replaced in full.
CREATE OR REPLACE FUNCTION finalize_workflow_status(p_workflow_id text, p_worker_id text, p_generation bigint, p_final_status text, p_result text, p_error_code text, p_error_op text, p_query_state jsonb, p_next_wake_at TIMESTAMPTZ, p_notify_channel text) RETURNS boolean
    LANGUAGE plpgsql
    AS $$
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
                completed_by = assigned_to,
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
                                      OR promise_seq <> promise_seq_at_claim
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
    -- actually matched this caller's (worker_id, generation), and only for
    -- 'done': a real failure never reaches this procedure (cleat#1973), so
    -- there is no 'failed' case left to guard here.
    IF v_rows_updated > 0 AND p_final_status = 'done' THEN
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
$$;

GRANT ALL ON FUNCTION finalize_workflow_status(p_workflow_id text, p_worker_id text, p_generation bigint, p_final_status text, p_result text, p_error_code text, p_error_op text, p_query_state jsonb, p_next_wake_at TIMESTAMPTZ, p_notify_channel text) TO cleat_app;
