-- cleat MySQL constraints, indexes
-- MySQL doesn't support ADD COLUMN IF NOT EXISTS; use a conditional prepared statement.
SET @s = IF((SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_instances' AND COLUMN_NAME = 'allowed_signals') = 0, 'ALTER TABLE workflow_instances ADD COLUMN allowed_signals JSON DEFAULT NULL', 'SELECT 1');
PREPARE stmt FROM @s;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
CREATE INDEX idx_instances_ready ON workflow_instances(status, next_wake_at);
CREATE INDEX idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at);
CREATE INDEX idx_defs_active ON workflow_defs(name, version);
CREATE INDEX idx_instances_stale ON workflow_instances(status, heartbeat_at);
CREATE INDEX idx_promises_status ON workflow_promises(workflow_id, status);
CREATE INDEX idx_concurrency_keys_workflow ON concurrency_keys(workflow_id);
CREATE INDEX idx_instances_sticky ON workflow_instances(sticky_worker_id);
CREATE INDEX idx_update_requests_pending ON workflow_update_requests(workflow_id, status);
CREATE INDEX idx_api_keys_hash ON tenant_api_keys(key_hash);
CREATE INDEX idx_defs_tenant_name_version ON workflow_defs(tenant_id, name, version);
CREATE INDEX idx_instances_tenant_ready ON workflow_instances(tenant_id, status, next_wake_at);
CREATE INDEX idx_event_history_tenant_wf ON event_history(tenant_id, workflow_id, step);
CREATE INDEX idx_signals_tenant_wf ON workflow_signals(tenant_id, workflow_id, signal_name);
CREATE INDEX idx_schedules_tenant_enabled ON workflow_schedules(tenant_id, enabled, next_run_at);
CREATE INDEX idx_instances_tenant_queue_ready ON workflow_instances(tenant_id, task_queue, status, next_wake_at);
CREATE INDEX idx_idempotency_workflow_id ON idempotency_keys(workflow_id);
CREATE INDEX idx_idempotency_expires ON idempotency_keys(expires_at);
CREATE INDEX idx_mem_samples_def ON workflow_memory_samples (def_name, recorded_at);
CREATE INDEX idx_instances_created_at ON workflow_instances(tenant_id, created_at);
CREATE INDEX idx_instances_terminal_completed ON workflow_instances(tenant_id, status, completed_at);
CREATE INDEX idx_concurrency_keys_expires ON concurrency_keys(expires_at);
CREATE INDEX idx_instances_parent_policy ON workflow_instances(parent_workflow_id, parent_close_policy, status);
