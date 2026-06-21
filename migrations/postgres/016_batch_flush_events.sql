-- Migration 016: Bulk event persistence via jsonb_populate_recordset.
--
-- Replaces per-event INSERTs with a single set-based INSERT that uses
-- jsonb_populate_recordset to convert a JSONB array into rows in one
-- operation.  This is orders of magnitude faster than FOR-loop INSERTs
-- for large batches because Postgres processes the entire recordset
-- as a single plan node.
--
-- Used by the AdaptiveFlusher batch path. The JSON array is an array of
-- objects whose keys match the column names of event_history.

CREATE OR REPLACE FUNCTION batch_flush_events(
    p_events JSONB
) RETURNS VOID AS $$
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
    ON CONFLICT (workflow_id, step) DO UPDATE
        SET response = EXCLUDED.response, error = EXCLUDED.error
        WHERE event_history.response = '' AND event_history.error IS NULL;
END;
$$ LANGUAGE plpgsql;
