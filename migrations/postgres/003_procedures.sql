-- cleat consolidated procedures (003)
--
-- GENERATED from pg_get_functiondef via pg_dump, so each body is the LAST
-- definition of that routine. cleat#2059.

CREATE OR REPLACE FUNCTION batch_flush_events(p_events jsonb) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO event_history (
        workflow_id, step, event_type, service, operation,
        request, response, error, duration_ms, signal_names,
        timeout_ms, signal_name, signal_payload, defer_description,
        defer_id, child_name, child_input, run_id, new_input,
        plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
        promise_name, promise_id, promise_result, promise_error,
        payload, created_at, checksum, tenant_id
    )
    SELECT
        workflow_id, step, event_type, service, operation,
        request, response, error, duration_ms, signal_names,
        timeout_ms, signal_name, signal_payload, defer_description,
        defer_id, child_name, child_input, run_id, new_input,
        plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
        promise_name, promise_id, promise_result, promise_error,
        payload, created_at, checksum, tenant_id
    FROM jsonb_populate_recordset(NULL::event_history, p_events)
    ON CONFLICT (tenant_id, workflow_id, step) DO UPDATE
        SET response = EXCLUDED.response, error = EXCLUDED.error
        WHERE event_history.response = '' AND event_history.error IS NULL;
END;
$$;

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

CREATE OR REPLACE FUNCTION flush_event_step(p_workflow_id text, p_step integer, p_event_type text, p_service text, p_operation text, p_request text, p_response text, p_error text, p_duration_ms bigint, p_signal_names text, p_timeout_ms bigint, p_signal_name text, p_signal_payload text, p_defer_description text, p_defer_id text, p_child_name text, p_child_input text, p_run_id text, p_new_input text, p_plugin_name text, p_plugin_func text, p_plugin_input text, p_plugin_output text, p_plugin_error text, p_promise_name text, p_promise_id text, p_promise_result text, p_promise_error text, p_payload jsonb, p_checksum text, p_tenant_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO event_history (
        workflow_id, step, event_type, service, operation,
        request, response, error, duration_ms, signal_names,
        timeout_ms, signal_name, signal_payload, defer_description,
        defer_id, child_name, child_input, run_id, new_input,
        plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
        promise_name, promise_id, promise_result, promise_error,
        payload, checksum, created_at, tenant_id
    ) VALUES (
        p_workflow_id, p_step, p_event_type,
        nullif(p_service, ''), nullif(p_operation, ''),
        nullif(p_request, ''), nullif(p_response, ''), nullif(p_error, ''),
        nullif(p_duration_ms, 0), nullif(p_signal_names, ''),
        nullif(p_timeout_ms, 0), nullif(p_signal_name, ''),
        nullif(p_signal_payload, ''), nullif(p_defer_description, ''),
        nullif(p_defer_id, ''), nullif(p_child_name, ''),
        nullif(p_child_input, ''), nullif(p_run_id, ''),
        nullif(p_new_input, ''), nullif(p_plugin_name, ''),
        nullif(p_plugin_func, ''), nullif(p_plugin_input, ''),
        nullif(p_plugin_output, ''), nullif(p_plugin_error, ''),
        nullif(p_promise_name, ''), nullif(p_promise_id, ''),
        nullif(p_promise_result, ''), nullif(p_promise_error, ''),
        CASE WHEN p_payload IS NOT NULL THEN p_payload ELSE NULL END,
        p_checksum, now(), p_tenant_id
    ) ON CONFLICT (tenant_id, workflow_id, step) DO UPDATE
        SET response = EXCLUDED.response,
            error = EXCLUDED.error
        WHERE event_history.response = ''
          AND event_history.error IS NULL;
END;
$$;

GRANT ALL ON FUNCTION batch_flush_events(p_events jsonb) TO cleat_app;

GRANT ALL ON FUNCTION finalize_workflow_status(p_workflow_id text, p_worker_id text, p_generation bigint, p_final_status text, p_result text, p_error_code text, p_error_op text, p_query_state jsonb, p_next_wake_at TIMESTAMPTZ, p_notify_channel text) TO cleat_app;

GRANT ALL ON FUNCTION flush_event_step(p_workflow_id text, p_step integer, p_event_type text, p_service text, p_operation text, p_request text, p_response text, p_error text, p_duration_ms bigint, p_signal_names text, p_timeout_ms bigint, p_signal_name text, p_signal_payload text, p_defer_description text, p_defer_id text, p_child_name text, p_child_input text, p_run_id text, p_new_input text, p_plugin_name text, p_plugin_func text, p_plugin_input text, p_plugin_output text, p_plugin_error text, p_promise_name text, p_promise_id text, p_promise_result text, p_promise_error text, p_payload jsonb, p_checksum text, p_tenant_id uuid) TO cleat_app;
