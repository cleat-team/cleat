-- cleat#3245 B5. Applied to a database built from the chain at the --prior-rebaseline
-- commit, BEFORE it is dumped, so that the regenerated baseline is the chain's
-- resolved state with workflow_instances narrowed. It is not a migration: nothing
-- applies it to a deployment. Fresh databases only (cleat#3405, D18).
--
-- workflow_instances kept id, tenant_id and three columns that exist nowhere else:
-- parent_close_policy, event_count and history_swept_at. Every other column was
-- already on workflow_leases or workflow_payloads (Phase 3 steps 2-3), and the
-- readers had moved there in B3. The indexes, CHECK constraints and the def FK that
-- named a dropped column went with it. DROP COLUMN does that on its own; it is not
-- repeated here.
--
-- Two things were not column drops:
--
--   * admin.claim_workflows read and wrote the dropped columns and had no caller
--     since the rotating claim replaced it (engine/store_lifecycle.go), so it was
--     deleted rather than rewritten.
--
--   * workflow_instances_def_fkey (tenant_id, def_name, def_version) ->
--     workflow_defs went with def_name and def_version, and it was what refused
--     deleting a definition a run still used. workflow_payloads already carried the
--     same three values, unconstrained, so the FK moved there. Both columns there
--     are nullable and the FK is MATCH SIMPLE, so a payloads row with no definition
--     is unaffected.

DROP FUNCTION IF EXISTS admin.claim_workflows(text, text[], integer);

ALTER TABLE workflow_instances
    DROP COLUMN def_name,
    DROP COLUMN def_version,
    DROP COLUMN status,
    DROP COLUMN input,
    DROP COLUMN assigned_to,
    DROP COLUMN heartbeat_at,
    DROP COLUMN next_wake_at,
    DROP COLUMN created_at,
    DROP COLUMN completed_at,
    DROP COLUMN result,
    DROP COLUMN error_msg,
    DROP COLUMN error_code,
    DROP COLUMN error_op,
    DROP COLUMN parent_workflow_id,
    DROP COLUMN query_state,
    DROP COLUMN trace_id,
    DROP COLUMN sticky_worker_id,
    DROP COLUMN task_queue,
    DROP COLUMN compaction_state,
    DROP COLUMN compaction_step,
    DROP COLUMN allowed_signals,
    DROP COLUMN priority,
    DROP COLUMN generation,
    DROP COLUMN pending_terminal_status,
    DROP COLUMN defer_phase_deadline,
    DROP COLUMN continued_from,
    DROP COLUMN signal_seq,
    DROP COLUMN signal_seq_at_claim,
    DROP COLUMN signal_consumed_seq,
    DROP COLUMN signal_consumed_at_claim,
    DROP COLUMN reclaim_count,
    DROP COLUMN started_at,
    DROP COLUMN concurrency_key_hash,
    DROP COLUMN run_wasm_instance_timeout_ms,
    DROP COLUMN run_wasm_wall_clock_ceiling_ms,
    DROP COLUMN run_host_retry_budget_ms,
    DROP COLUMN completed_by,
    DROP COLUMN run_max_workflow_duration_ms,
    DROP COLUMN promise_seq,
    DROP COLUMN promise_seq_at_claim;

ALTER TABLE workflow_payloads
    ADD CONSTRAINT workflow_payloads_def_fkey
    FOREIGN KEY (tenant_id, def_name, def_version)
    REFERENCES workflow_defs (tenant_id, name, version);
