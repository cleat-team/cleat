-- Migration 013: PL/pgSQL function for per-step event flush.
-- Replaces the BEGIN+INSERT+COMMIT transaction in flushEvent with a single
-- SELECT call that does the INSERT server-side, eliminating 2 RTs per step.

CREATE OR REPLACE FUNCTION flush_event_step(
    p_workflow_id TEXT,
    p_step        INT,
    p_event_type  TEXT,
    p_service     TEXT,
    p_operation   TEXT,
    p_request     TEXT,
    p_response    TEXT,
    p_error       TEXT,
    p_duration_ms BIGINT,
    p_signal_names TEXT,
    p_timeout_ms  BIGINT,
    p_signal_name TEXT,
    p_signal_payload TEXT,
    p_defer_description TEXT,
    p_defer_id    TEXT,
    p_child_name  TEXT,
    p_child_input TEXT,
    p_run_id      TEXT,
    p_new_input   TEXT,
    p_plugin_name TEXT,
    p_plugin_func TEXT,
    p_plugin_input TEXT,
    p_plugin_output TEXT,
    p_plugin_error TEXT,
    p_promise_name TEXT,
    p_promise_id  TEXT,
    p_promise_result TEXT,
    p_promise_error TEXT,
    p_payload     JSONB,
    p_checksum    TEXT,
    p_tenant_id   UUID
) RETURNS VOID AS $$
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
    ) ON CONFLICT (workflow_id, step) DO UPDATE
        SET response = EXCLUDED.response,
            error = EXCLUDED.error
        WHERE event_history.response = ''
          AND event_history.error IS NULL;
END;
$$ LANGUAGE plpgsql;
