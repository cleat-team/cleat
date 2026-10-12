-- cleat#3245 Phase 3 step 3 piece F: finalize_workflow_status's 'done' arm
-- mirrors completed_at onto workflow_payloads now that migration 020 added
-- the column there. Everything else here is migration 019's function body,
-- reproduced whole because CREATE OR REPLACE replaces it in full. The only
-- change is adding `completed_at = now()` to the existing
-- `UPDATE workflow_payloads SET result = ..., query_state = ...` in the
-- 'done' arm -- same statement, same predicate, one more column.
CREATE OR REPLACE FUNCTION finalize_workflow_status(p_workflow_id text, p_worker_id text, p_generation bigint, p_final_status text, p_result text, p_error_code text, p_error_op text, p_query_state jsonb, p_next_wake_at TIMESTAMPTZ, p_notify_channel text) RETURNS boolean
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_rows_updated INT;
    v_resolved_next_wake_at TIMESTAMPTZ;
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
              AND generation = p_generation
            RETURNING next_wake_at INTO v_resolved_next_wake_at;

        ELSE
            RAISE EXCEPTION 'finalize_workflow_status: unknown final status: %', p_final_status;
    END CASE;

    GET DIAGNOSTICS v_rows_updated = ROW_COUNT;

    -- cleat#3245 Phase 3 step 2 piece 4b: mirror the same transition onto
    -- workflow_leases/workflow_payloads, gated on the same fence the
    -- workflow_instances UPDATE above already enforced.
    IF v_rows_updated > 0 THEN
        CASE p_final_status
            WHEN 'done' THEN
                UPDATE workflow_leases
                SET status = 'done', completed_by = assigned_to, assigned_to = NULL
                WHERE id = p_workflow_id
                  AND assigned_to = p_worker_id
                  AND generation = p_generation;

                -- cleat#3245 Phase 3 step 3 piece F: completed_at added,
                -- migration 020.
                UPDATE workflow_payloads
                SET result = p_result::jsonb, query_state = p_query_state, completed_at = now()
                WHERE id = p_workflow_id;

            WHEN 'ready' THEN
                UPDATE workflow_leases
                SET status = 'ready', assigned_to = NULL, next_wake_at = v_resolved_next_wake_at
                WHERE id = p_workflow_id
                  AND assigned_to = p_worker_id
                  AND generation = p_generation;

                UPDATE workflow_payloads
                SET query_state = p_query_state
                WHERE id = p_workflow_id;
        END CASE;
    END IF;

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

        -- cleat#3245 Phase 3 step 3 piece B: mirror the parent wake onto
        -- workflow_leases -- same predicate as the UPDATE directly above,
        -- so the two agree on exactly which parent row they mean. Read by
        -- ClaimWorkflows' candidate scan (this PR); without this the
        -- parent's workflow_leases.next_wake_at stays stale and the parent
        -- is never claimable again.
        UPDATE workflow_leases
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
