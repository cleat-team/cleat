-- cleat#3245 B3c: finalize_workflow_status stops writing the lease columns
-- (status, assigned_to, completed_by, next_wake_at) to workflow_instances.
--
-- Since B3a every PostgreSQL reader of those columns reads workflow_leases,
-- and since B3c every other writer writes them there alone. So the fenced
-- UPDATE on workflow_leases is now the fence: its row count is what this
-- function returns, as the workflow_instances UPDATE's was before.
--
-- Two things change with the move, both because the instance copy is no
-- longer maintained:
--
--   * The 'ready' arm computes the immediate-wake CASE from the lease row's
--     own signal and promise counters. 005 computed it on workflow_instances
--     and copied the result across, which reads counters that B3c no longer
--     bumps there.
--   * The parent wake finds the parent through workflow_leases and checks the
--     parent's lease status, the only status written.
--
-- parent_close_policy stays on workflow_instances and is not read here. The
-- columns themselves are dropped in the baseline regeneration (cleat#3362).
--
-- idx_leases_parent: the parent-close cascade and the parent wake now find a
-- parent's children through workflow_leases.parent_workflow_id, which had no
-- index. workflow_instances has idx_instances_parent_policy for the same
-- lookup; this is its lease twin, partial because most runs have no parent.
--
-- CREATE OR REPLACE with the signature and return type 003 defines, so this
-- replaces that definition in place and is safe to re-run. Unqualified, like
-- 003: the migration runner sets search_path to the target schema.

CREATE INDEX IF NOT EXISTS idx_leases_parent ON workflow_leases USING btree (parent_workflow_id) WHERE (parent_workflow_id IS NOT NULL);

CREATE OR REPLACE FUNCTION finalize_workflow_status(p_workflow_id text, p_worker_id text, p_generation bigint, p_final_status text, p_result text, p_error_code text, p_error_op text, p_query_state jsonb, p_next_wake_at TIMESTAMPTZ, p_notify_channel text) RETURNS boolean
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_rows_updated INT;
    v_parent_workflow_id text;
BEGIN
    -- Update workflow status, fenced on (assigned_to, generation) so a
    -- caller that no longer owns the workflow cannot modify it.
    CASE p_final_status
        WHEN 'done' THEN
            UPDATE workflow_leases
            SET status = 'done',
                completed_by = assigned_to,
                assigned_to = NULL
            WHERE id = p_workflow_id
              AND assigned_to = p_worker_id
              AND generation = p_generation
            RETURNING parent_workflow_id INTO v_parent_workflow_id;

        WHEN 'ready' THEN
            UPDATE workflow_leases
            SET status = 'ready',
                assigned_to = NULL,
                next_wake_at = CASE
                                    WHEN signal_seq <> signal_seq_at_claim
                                      OR signal_consumed_seq <> signal_consumed_at_claim
                                      OR promise_seq <> promise_seq_at_claim
                                    THEN now() ELSE p_next_wake_at END
            WHERE id = p_workflow_id
              AND assigned_to = p_worker_id
              AND generation = p_generation;

        ELSE
            RAISE EXCEPTION 'finalize_workflow_status: unknown final status: %', p_final_status;
    END CASE;

    GET DIAGNOSTICS v_rows_updated = ROW_COUNT;

    IF v_rows_updated > 0 THEN
        CASE p_final_status
            WHEN 'done' THEN
                UPDATE workflow_payloads
                SET result = p_result::jsonb, query_state = p_query_state, completed_at = now()
                WHERE id = p_workflow_id;

            WHEN 'ready' THEN
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
        IF v_parent_workflow_id IS NOT NULL THEN
            UPDATE workflow_leases
            SET next_wake_at = now()
            WHERE id = v_parent_workflow_id
              AND status IN ('ready', 'suspended');
        END IF;

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
