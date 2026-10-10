-- cleat#3245 Phase 3 step 2, piece 4b: finalize_workflow_status (the
-- WASM-segment-ending path behind finalizeWorkflowSegmentInner /
-- FinalizeWorkflowSegment) now mirrors its terminal write onto
-- workflow_leases and workflow_payloads, same as pieces 1-4a did for their
-- own call sites -- but reached through a migration rather than a Go
-- change, because this path's write is delegated into one PL/pgSQL
-- function rather than issued as Go-level UPDATE statements (migration
-- 016's own "A REAL COST THIS SURFACES" note flagged this one by name).
--
-- Only the two CASE arms' own workflow_instances UPDATEs are unchanged.
-- Everything else here is the LAST finalize_workflow_status body on
-- develop (migrations/postgres/014), reproduced whole because the
-- function is replaced in full, plus two additions:
--
-- 1. The 'ready' arm's UPDATE now RETURNS next_wake_at INTO a local
--    variable. That CASE expression reads signal_seq/signal_seq_at_claim/
--    signal_consumed_seq/signal_consumed_at_claim/promise_seq/
--    promise_seq_at_claim -- all six of which moved to workflow_leases in
--    migration 016, but none of which piece 4b dual-writes (that is piece
--    5's job, signal/promise delivery, not yet landed). Comparing those
--    columns on workflow_leases right now would always read 0/0 on a
--    pair neither piece 5 nor anything before it has ever touched since
--    creation -- the exact shape of piece 2's pending_terminal_status GAP
--    (cleat#3245, PR #3299 review), caught here before writing rather than
--    after. RETURNING the ALREADY-RESOLVED value from the workflow_instances
--    UPDATE and reusing it on workflow_leases sidesteps the question
--    entirely: workflow_instances remains the one place that decides what
--    next_wake_at becomes, exactly as it does today, and workflow_leases
--    only ever copies the decided value -- never re-derives it from columns
--    this piece knows are not yet trustworthy there.
--
-- 2. A second CASE, gated on v_rows_updated > 0 (the fence that already
--    guards this function's terminal side-effects below), mirrors each
--    arm's workflow_instances write onto workflow_leases/workflow_payloads:
--      'done'  -> leases: status/completed_by/assigned_to (identical shape
--                 to CompleteWorkflow's completeLeaseRow, piece 4a);
--                 payloads: result/query_state (writeResultPayload).
--      'ready' -> leases: status/assigned_to/next_wake_at (the RETURNING
--                 value from point 1); payloads: query_state only --
--                 'ready' never carries a result.
--
-- completed_at stays workflow_instances-only (absent from both new tables'
-- column lists, migration 016), same as piece 4a.
--
-- 'suspended' is accepted by Go's validFinalStatus but never reaches this
-- function in practice -- store_lifecycle.go's own survey of every status
-- writer (cleat#1997) found no statement anywhere sets it; a suspension is
-- always written as 'ready' with a next_wake_at. The ELSE branch's
-- RAISE EXCEPTION is therefore unreachable in production today and is left
-- exactly as migration 014 wrote it.
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

                UPDATE workflow_payloads
                SET result = p_result::jsonb, query_state = p_query_state
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
