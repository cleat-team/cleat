-- cleat consolidated stored procedures (003)
-- Combines: 012_finalize_workflow_segment_fn, 013_flush_event_step_fn,
--           014_cleanup_events_on_terminal, 016_batch_flush_events
--
-- finalize_workflow_status() is the final version from 014 (includes event
-- cleanup on terminal status, idempotency recording, parent wake, and FK drop).
-- flush_event_step() is a per-step event INSERT (from 013).
-- batch_flush_events() is the bulk variant using jsonb_populate_recordset (016).

-- Pin the creation target; see the note in 001_schema.sql. The default
-- search_path is "$user", public, so unqualified names below would resolve
-- against a schema named after the connecting role -- and 001 creates a schema
-- called "cleat" while the shipped compose connects as POSTGRES_USER=cleat.
SET search_path = public;

-- ── Drop FK on event_history (no longer needed; events are deleted on terminal) ─

ALTER TABLE event_history DROP CONSTRAINT IF EXISTS fk_event_history_workflow;

-- ── Finalize workflow status ─────────────────────────────────────────────────
-- Updates the workflow instance status, records idempotency outcome, wakes
-- the parent workflow, populates the parent's await_child event result, deletes
-- events for terminal workflows, and dispatches a pg_notify hint.
-- Called within an existing transaction after events have been appended and
-- event_count has been incremented.

-- Drop before creating, for the same reason 004 does: this file declares
-- RETURNS VOID and 004 replaces it with RETURNS BOOLEAN, and PostgreSQL
-- rejects a return-type change through CREATE OR REPLACE with
--   ERROR: cannot change return type of existing function (42P13)
-- Re-applying the migration set to a database that already has 004's version
-- would otherwise fail here -- and re-applying is exactly what an operator
-- upgrading an existing deployment does. The files are advertised as
-- idempotent (docs/explanation/postgresql-schema.md) and
-- TestShippedSchema_IsIdempotent enforces it.
--
-- Dropping 004's version here is safe: 004 sorts after 003, so any run that
-- applies this file also re-applies 004 afterwards and the BOOLEAN version
-- with the fence guard is always the end state.
DROP FUNCTION IF EXISTS finalize_workflow_status(
    TEXT, TEXT, BIGINT, TEXT, TEXT, TEXT, TEXT, JSONB, TIMESTAMPTZ, TEXT
);

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
) RETURNS VOID AS $$
BEGIN
    -- Update workflow status
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
                next_wake_at = p_next_wake_at
            WHERE id = p_workflow_id
              AND assigned_to = p_worker_id
              AND generation = p_generation;

        ELSE
            RAISE EXCEPTION 'finalize_workflow_status: unknown final status: %', p_final_status;
    END CASE;

    -- Terminal status side-effects
    IF p_final_status = 'done' OR p_final_status = 'failed' THEN
        -- Record idempotency outcome (before deleting events, since
        -- the parent's await_child event references this workflow).
        IF p_final_status = 'done' THEN
            UPDATE idempotency_keys
            SET result = p_result::jsonb
            WHERE workflow_id = p_workflow_id;
        ELSE
            UPDATE idempotency_keys
            SET error_msg = p_result
            WHERE workflow_id = p_workflow_id;
        END IF;

        -- Wake parent workflow atomically.
        UPDATE workflow_instances
        SET next_wake_at = now()
        WHERE id = (
            SELECT parent_workflow_id FROM workflow_instances WHERE id = p_workflow_id
        )
        AND status IN ('ready', 'suspended');

        -- Populate parent's await_child event result (reads from the
        -- parent's events, not the child's, so this is safe to run
        -- before deleting the child's events).
        UPDATE event_history
        SET response = p_result
        WHERE workflow_id = (
            SELECT parent_workflow_id FROM workflow_instances WHERE id = p_workflow_id
        )
        AND event_type = 'await_child'
        AND run_id = p_workflow_id
        AND (response IS NULL OR response = '');

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
END;
$$ LANGUAGE plpgsql;

-- ── Per-step event flush ─────────────────────────────────────────────────────
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

-- ── Batch event flush ────────────────────────────────────────────────────────
-- Bulk event persistence via jsonb_populate_recordset.
-- Replaces per-event INSERTs with a single set-based INSERT that processes
-- an entire JSONB array of event objects in one operation.

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
