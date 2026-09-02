package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
)

func (s *PostgresStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("load history: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT step, event_type, service, operation, request, response, error,
		       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		       defer_description, defer_id, child_name, child_input, run_id, new_input,
		       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
		       payload,
		       promise_name, promise_id, promise_result, promise_error,
		       EXTRACT(EPOCH FROM created_at)::BIGINT * 1000 AS timestamp_ms,
		       created_at,
		       (intent_at IS NOT NULL AND checksum IS NULL) AS pending
		FROM event_history
		WHERE workflow_id = $1 AND tenant_id = $2
		ORDER BY step
	`, workflowID, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	defer rows.Close()

	var history []EventRecord
	for rows.Next() {
		var rec EventRecord
		var service, op, request, response, errMsg sql.NullString
		var durationMs, timeoutMs sql.NullInt64
		var signalNames, signalName, signalPayload sql.NullString
		var deferDesc, deferID sql.NullString
		var childName, childInput, runID, newInput sql.NullString
		var pluginName, pluginFunc, pluginInput, pluginOutput, pluginErr sql.NullString
		var payload sql.NullString
		var promiseName, promiseID, promiseResult, promiseError sql.NullString
		var createdAt sql.NullTime

		if err := rows.Scan(&rec.Step, &rec.EventType,
			&service, &op, &request, &response, &errMsg,
			&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
			&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
			&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr,
			&payload,
			&promiseName, &promiseID, &promiseResult, &promiseError,
			&rec.TimestampMs, &createdAt, &rec.Pending); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}

		if createdAt.Valid {
			rec.CreatedAt = createdAt.Time
		}
		rec.Service = service.String
		rec.Op = op.String
		rec.Request = tryDecodeBase64(request.String)
		rec.Response = tryDecodeBase64(response.String)
		rec.Err = errMsg.String
		rec.DurationMs = durationMs.Int64
		rec.SignalNames = signalNames.String
		rec.TimeoutMs = timeoutMs.Int64
		rec.SignalName = signalName.String
		rec.SignalPayload = signalPayload.String
		rec.DeferDescription = deferDesc.String
		rec.DeferID = deferID.String
		rec.ChildName = childName.String
		rec.ChildInput = childInput.String
		rec.RunID = runID.String
		rec.NewInput = newInput.String
		rec.PluginName = pluginName.String
		rec.PluginFunc = pluginFunc.String
		rec.PluginInput = pluginInput.String
		rec.PluginOutput = pluginOutput.String
		rec.PluginError = pluginErr.String
		rec.PromiseName = promiseName.String
		rec.PromiseID = promiseID.String
		rec.PromiseResult = promiseResult.String
		rec.PromiseError = promiseError.String

		// Decrypt and redact event record.
		s.decryptAndRedactEventRecord(&rec, workflowID)

		// Retroactive redaction on read path: ensure sensitive fields are
		// redacted even if they were stored before redaction was mandatory.
		// Redaction runs AFTER decryption (see block above) since redacting
		// ciphertext would yield meaningless "[REDACTED]" placeholders.
		if !s.disableReadRedaction {
			rec.Request = RedactOnRead(rec.Request)
			rec.Response = RedactOnRead(rec.Response)
			rec.Err = RedactOnRead(rec.Err)
			rec.SignalPayload = RedactOnRead(rec.SignalPayload)
			rec.ChildInput = RedactOnRead(rec.ChildInput)
			rec.NewInput = RedactOnRead(rec.NewInput)
			rec.PluginInput = RedactOnRead(rec.PluginInput)
			rec.PluginOutput = RedactOnRead(rec.PluginOutput)
			rec.PromiseResult = RedactOnRead(rec.PromiseResult)
			rec.PromiseError = RedactOnRead(rec.PromiseError)
		}

		if payload.Valid {
			payloadStr := s.decryptPayloadJSON(payload.String)
			populateFromPayload(&rec, []byte(payloadStr))
		}

		history = append(history, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return history, tx.Commit()
}

// chainOrder returns indices into recs in ascending Step order.
//
// The checksum chain has to be built in step order because that is the order
// VerifyWorkflowEvents recomputes it in (LoadEventHistory is ORDER BY step). A
// batch whose records arrive in some other order would otherwise persist a
// chain that verification can never reproduce, and the workflow would read as
// corrupt for the rest of its life.
func chainOrder(recs []EventRecord) []int {
	order := make([]int, len(recs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return recs[order[a]].Step < recs[order[b]].Step })
	return order
}

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func nullInt64(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: v != 0}
}

func eventRecordToPayload(rec EventRecord) ([]byte, error) {
	payload := make(map[string]any)
	switch rec.EventType {
	case "call":
		payload["service"] = rec.Service
		payload["operation"] = rec.Op
		payload["request_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.Request))
		if rec.Response != "" {
			payload["response_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.Response))
		}
		if rec.Err != "" {
			payload["error"] = rec.Err
		}
		// Only when true, so an ordinary retryable failure produces byte-identical
		// payload to what it always did. See EventRecord.ErrNonRetryable.
		if rec.ErrNonRetryable {
			payload["error_non_retryable"] = true
		}
		// Only when set, for the same reason: an unclassified failure keeps
		// producing the payload it always did. "error_code" matches the
		// column name on workflow_instances, which stores the same strings.
		if rec.ErrCode != "" {
			payload["error_code"] = rec.ErrCode
		}
		if rec.DurationMs > 0 {
			payload["duration_ms"] = rec.DurationMs
		}
	case "sleep":
		payload["duration_ms"] = rec.DurationMs
	case "await_signals":
		if rec.SignalNames != "" {
			payload["signal_names"] = rec.SignalNames
		}
		if rec.TimeoutMs > 0 {
			payload["timeout_ms"] = rec.TimeoutMs
		}
	case "signal_received":
		if rec.SignalName != "" {
			payload["signal_name"] = rec.SignalName
		}
		if rec.SignalPayload != "" {
			payload["signal_payload"] = rec.SignalPayload
		}
	case "defer":
		if rec.DeferDescription != "" {
			payload["defer_description"] = rec.DeferDescription
		}
		if rec.DeferID != "" {
			payload["defer_id"] = rec.DeferID
		}
	case "child_workflow":
		if rec.ChildName != "" {
			payload["child_name"] = rec.ChildName
		}
		if rec.ChildInput != "" {
			payload["child_input"] = rec.ChildInput
		}
		if rec.RunID != "" {
			payload["run_id"] = rec.RunID
		}
	case "continue_as_new":
		if rec.NewInput != "" {
			payload["new_input"] = rec.NewInput
		}
	case "plugin_call":
		if rec.PluginName != "" {
			payload["plugin_name"] = rec.PluginName
		}
		if rec.PluginFunc != "" {
			payload["plugin_func"] = rec.PluginFunc
		}
		if rec.PluginInput != "" {
			payload["plugin_input"] = rec.PluginInput
		}
		if rec.PluginOutput != "" {
			payload["plugin_output"] = rec.PluginOutput
		}
		if rec.PluginError != "" {
			payload["plugin_error"] = rec.PluginError
		}
	case "create_promise":
		payload["promise_name"] = rec.PromiseName
		payload["promise_id"] = rec.PromiseID
	case "await_promise", "promise_resolved", "promise_rejected":
		payload["promise_id"] = rec.PromiseID
		if rec.PromiseResult != "" {
			payload["promise_result"] = rec.PromiseResult
		}
		if rec.PromiseError != "" {
			payload["promise_error"] = rec.PromiseError
		}
	case "update_handler":
		if rec.UpdateHandlerName != "" {
			payload["update_handler_name"] = rec.UpdateHandlerName
		}
	case "state_mutation":
		if rec.StateKey != "" {
			payload["state_key"] = rec.StateKey
		}
		if rec.StateValue != "" {
			payload["state_value"] = rec.StateValue
		}
		if rec.StateDelta != 0 {
			payload["state_delta"] = rec.StateDelta
		}
		if rec.StateOp != "" {
			payload["state_op"] = rec.StateOp
		}
	case "run_detached":
		if rec.DetachedName != "" {
			payload["detached_name"] = rec.DetachedName
		}
		if rec.DetachedInput != "" {
			payload["detached_input"] = rec.DetachedInput
		}
		if rec.DetachedRunID != "" {
			payload["detached_run_id"] = rec.DetachedRunID
		}
	case "admin_action":
		// Without this arm the payload is "{}", and computeEventChecksum
		// hashes payload alone -- so who forced a workflow and what they did
		// to it would sit entirely outside the checksum, editable in the
		// columns afterwards with VerifyWorkflowEvents still calling the
		// workflow clean. For an audit record that is the whole point of
		// writing it into the history rather than beside it.
		//
		// Note that verifyShadowColumns does NOT cover this: it compares the
		// columns against a record populated from payload, and
		// populateFromPayload only overwrites keys the payload carries, so an
		// absent key inherits the column's own value and always compares
		// equal. An empty payload is invisible to it, not a divergence.
		// TestAdminActionEventPayloadRoundTrip is what holds this arm up.
		// See store_admin.go.
		if rec.Service != "" {
			payload["service"] = rec.Service
		}
		if rec.Op != "" {
			payload["operation"] = rec.Op
		}
		if rec.Err != "" {
			payload["error"] = rec.Err
		}
	case "side_effect":
		if rec.SideEffectResult != "" {
			payload["side_effect_result"] = rec.SideEffectResult
		}
	case "plugin_call_stream_chunk":
		if rec.PluginName != "" {
			payload["plugin_name"] = rec.PluginName
		}
		if rec.PluginFunc != "" {
			payload["plugin_func"] = rec.PluginFunc
		}
		if rec.PluginInput != "" {
			payload["plugin_input"] = rec.PluginInput
		}
		if rec.PluginOutput != "" {
			payload["plugin_output"] = rec.PluginOutput
		}
		if rec.PluginError != "" {
			payload["plugin_error"] = rec.PluginError
		}
	case "scope_acquired":
		if rec.ScopeKey != "" {
			payload["scope_key"] = rec.ScopeKey
		}
		if rec.Err != "" {
			payload["error"] = rec.Err
		}
	case "durable_log":
		if rec.Message != "" {
			payload["message"] = rec.Message
		}
		if rec.LogLevel != "" {
			payload["log_level"] = rec.LogLevel
		}
		if rec.LogKV != "" {
			payload["log_kv"] = rec.LogKV
		}
	case "fetch":
		if rec.FetchMethod != "" {
			payload["fetch_method"] = rec.FetchMethod
		}
		if rec.FetchURL != "" {
			payload["fetch_url"] = rec.FetchURL
		}
		if rec.FetchHeaders != "" {
			payload["fetch_headers"] = rec.FetchHeaders
		}
		if rec.FetchBody != "" {
			payload["fetch_body"] = rec.FetchBody
		}
		if rec.FetchResponse != "" {
			payload["fetch_response_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.FetchResponse))
		}
		if rec.Err != "" {
			payload["error"] = rec.Err
		}
	case "schedule_cron", "delete_cron", "list_crons":
		if rec.CronWorkflowName != "" {
			payload["cron_workflow_name"] = rec.CronWorkflowName
		}
		if rec.CronExpr != "" {
			payload["cron_expr"] = rec.CronExpr
		}
		if rec.CronTimezone != "" {
			payload["cron_timezone"] = rec.CronTimezone
		}
		if rec.CronInput != "" {
			payload["cron_input"] = rec.CronInput
		}
		if rec.CronScheduleID != "" {
			payload["cron_schedule_id"] = rec.CronScheduleID
		}
		if rec.CronResult != "" {
			payload["cron_result"] = rec.CronResult
		}
		if rec.Err != "" {
			payload["error"] = rec.Err
		}
	case "acquire_lock":
		if rec.LockKey != "" {
			payload["lock_key"] = rec.LockKey
		}
		if rec.LockTTLMs > 0 {
			payload["lock_ttl_ms"] = rec.LockTTLMs
		}
		payload["lock_acquired"] = rec.LockAcquired
	case "release_lock":
		if rec.LockKey != "" {
			payload["lock_key"] = rec.LockKey
		}
	case "durable_send":
		if rec.Service != "" {
			payload["service"] = rec.Service
		}
		if rec.Op != "" {
			payload["operation"] = rec.Op
		}
		if rec.Request != "" {
			payload["request_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.Request))
		}
	case "durable_schedule_invoke":
		if rec.Service != "" {
			payload["service"] = rec.Service
		}
		if rec.Op != "" {
			payload["operation"] = rec.Op
		}
		if rec.Request != "" {
			payload["request_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.Request))
		}
		if rec.DurationMs > 0 {
			payload["duration_ms"] = rec.DurationMs
		}
	case "await_child":
		if rec.RunID != "" {
			payload["run_id"] = rec.RunID
		}
		if rec.Response != "" {
			payload["response_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.Response))
		}
		if rec.Err != "" {
			payload["error"] = rec.Err
		}
	case "heartbeat":
		if rec.Service != "" {
			payload["service"] = rec.Service
		}
		if rec.Op != "" {
			payload["operation"] = rec.Op
		}
	case "await_all_children":
		if rec.Request != "" {
			payload["request_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.Request))
		}
		if rec.Response != "" {
			payload["response_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.Response))
		}
	}
	return sortedJSONMarshal(payload)
}

// sortedJSONMarshal marshals a map to JSON with keys in deterministic
// (lexicographic) order. This is critical for checksum stability: Go's
// json.Marshal iterates map keys in random order, so two calls with the
// same data can produce different byte sequences.
//
// NOTE: This only sorts top-level keys. If any value in the map were a
// nested map[string]any, the keys in that nested map would not be sorted
// and would be non-deterministic. Currently safe because all payload
// values are primitives (string, int, bool).
func sortedJSONMarshal(m map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyBytes, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(keyBytes)
		buf.WriteByte(':')
		valBytes, err := json.Marshal(m[k])
		if err != nil {
			return nil, err
		}
		buf.Write(valBytes)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func populateFromPayload(rec *EventRecord, payload []byte) {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return
	}
	switch rec.EventType {
	case "call":
		if v, ok := m["service"].(string); ok {
			rec.Service = v
		}
		if v, ok := m["operation"].(string); ok {
			rec.Op = v
		}
		// Try base64-encoded fields first, fall back to raw strings for backward compat.
		if v, ok := m["request_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.Request = string(decoded)
			}
		} else if v, ok := m["request"].(string); ok {
			rec.Request = v
		}
		if v, ok := m["response_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.Response = string(decoded)
			}
		} else if v, ok := m["response"].(string); ok {
			rec.Response = v
		}
		if v, ok := m["error"].(string); ok {
			rec.Err = v
		}
		// Absent on every event written before 2.35, which is exactly right:
		// those replay as retryable, the behaviour they were recorded under.
		if v, ok := m["error_non_retryable"].(bool); ok {
			rec.ErrNonRetryable = v
		}
		// Absent on every event written before 2.35's second half, and on any
		// failure no ServiceCaller classified. Empty means "nobody said" --
		// never a class -- so nothing downstream can mistake it for one.
		if v, ok := m["error_code"].(string); ok {
			rec.ErrCode = v
		}
		if v, ok := m["duration_ms"].(float64); ok {
			rec.DurationMs = int64(v)
		}
	case "sleep":
		if v, ok := m["duration_ms"].(float64); ok {
			rec.DurationMs = int64(v)
		}
	case "await_signals":
		if v, ok := m["signal_names"].(string); ok {
			rec.SignalNames = v
		}
		if v, ok := m["timeout_ms"].(float64); ok {
			rec.TimeoutMs = int64(v)
		}
	case "signal_received":
		if v, ok := m["signal_name"].(string); ok {
			rec.SignalName = v
		}
		if v, ok := m["signal_payload"].(string); ok {
			rec.SignalPayload = v
		}
	case "defer":
		if v, ok := m["defer_description"].(string); ok {
			rec.DeferDescription = v
		}
		if v, ok := m["defer_id"].(string); ok {
			rec.DeferID = v
		}
	case "child_workflow":
		if v, ok := m["child_name"].(string); ok {
			rec.ChildName = v
		}
		if v, ok := m["child_input"].(string); ok {
			rec.ChildInput = v
		}
		if v, ok := m["run_id"].(string); ok {
			rec.RunID = v
		}
	case "continue_as_new":
		if v, ok := m["new_input"].(string); ok {
			rec.NewInput = v
		}
	case "plugin_call":
		if v, ok := m["plugin_name"].(string); ok {
			rec.PluginName = v
		}
		if v, ok := m["plugin_func"].(string); ok {
			rec.PluginFunc = v
		}
		if v, ok := m["plugin_input"].(string); ok {
			rec.PluginInput = v
		}
		if v, ok := m["plugin_output"].(string); ok {
			rec.PluginOutput = v
		}
		if v, ok := m["plugin_error"].(string); ok {
			rec.PluginError = v
		}
	case "create_promise", "await_promise", "promise_resolved", "promise_rejected":
		if v, ok := m["promise_name"].(string); ok {
			rec.PromiseName = v
		}
		if v, ok := m["promise_id"].(string); ok {
			rec.PromiseID = v
		}
		if v, ok := m["promise_result"].(string); ok {
			rec.PromiseResult = v
		}
		if v, ok := m["promise_error"].(string); ok {
			rec.PromiseError = v
		}
	case "update_handler":
		if v, ok := m["update_handler_name"].(string); ok {
			rec.UpdateHandlerName = v
		}
	case "state_mutation":
		if v, ok := m["state_key"].(string); ok {
			rec.StateKey = v
		}
		if v, ok := m["state_value"].(string); ok {
			rec.StateValue = v
		}
		if v, ok := m["state_delta"].(float64); ok {
			rec.StateDelta = int64(v)
		}
		if v, ok := m["state_op"].(string); ok {
			rec.StateOp = v
		}
	case "run_detached":
		if v, ok := m["detached_name"].(string); ok {
			rec.DetachedName = v
		}
		if v, ok := m["detached_input"].(string); ok {
			rec.DetachedInput = v
		}
		if v, ok := m["detached_run_id"].(string); ok {
			rec.DetachedRunID = v
		}
	case "admin_action":
		if v, ok := m["service"].(string); ok {
			rec.Service = v
		}
		if v, ok := m["operation"].(string); ok {
			rec.Op = v
		}
		if v, ok := m["error"].(string); ok {
			rec.Err = v
		}
	case "side_effect":
		if v, ok := m["side_effect_result"].(string); ok {
			rec.SideEffectResult = v
		}
	case "plugin_call_stream_chunk":
		if v, ok := m["plugin_name"].(string); ok {
			rec.PluginName = v
		}
		if v, ok := m["plugin_func"].(string); ok {
			rec.PluginFunc = v
		}
		if v, ok := m["plugin_input"].(string); ok {
			rec.PluginInput = v
		}
		if v, ok := m["plugin_output"].(string); ok {
			rec.PluginOutput = v
		}
		if v, ok := m["plugin_error"].(string); ok {
			rec.PluginError = v
		}
	case "scope_acquired":
		if v, ok := m["scope_key"].(string); ok {
			rec.ScopeKey = v
		}
		if v, ok := m["error"].(string); ok {
			rec.Err = v
		}
	case "durable_log":
		if v, ok := m["message"].(string); ok {
			rec.Message = v
		}
		if v, ok := m["log_level"].(string); ok {
			rec.LogLevel = v
		}
		if v, ok := m["log_kv"].(string); ok {
			rec.LogKV = v
		}
	case "fetch":
		if v, ok := m["fetch_method"].(string); ok {
			rec.FetchMethod = v
		}
		if v, ok := m["fetch_url"].(string); ok {
			rec.FetchURL = v
		}
		if v, ok := m["fetch_headers"].(string); ok {
			rec.FetchHeaders = v
		}
		if v, ok := m["fetch_body"].(string); ok {
			rec.FetchBody = v
		}
		if v, ok := m["fetch_response_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.FetchResponse = string(decoded)
			}
		} else if v, ok := m["fetch_response"].(string); ok {
			rec.FetchResponse = v
		}
		if v, ok := m["error"].(string); ok {
			rec.Err = v
		}
	case "schedule_cron", "delete_cron", "list_crons":
		if v, ok := m["cron_workflow_name"].(string); ok {
			rec.CronWorkflowName = v
		}
		if v, ok := m["cron_expr"].(string); ok {
			rec.CronExpr = v
		}
		if v, ok := m["cron_timezone"].(string); ok {
			rec.CronTimezone = v
		}
		if v, ok := m["cron_input"].(string); ok {
			rec.CronInput = v
		}
		if v, ok := m["cron_schedule_id"].(string); ok {
			rec.CronScheduleID = v
		}
		if v, ok := m["cron_result"].(string); ok {
			rec.CronResult = v
		}
		if v, ok := m["error"].(string); ok {
			rec.Err = v
		}
	case "acquire_lock":
		if v, ok := m["lock_key"].(string); ok {
			rec.LockKey = v
		}
		if v, ok := m["lock_ttl_ms"].(float64); ok {
			rec.LockTTLMs = int64(v)
		}
		if v, ok := m["lock_acquired"].(float64); ok {
			rec.LockAcquired = int(v)
		}
	case "release_lock":
		if v, ok := m["lock_key"].(string); ok {
			rec.LockKey = v
		}
	case "durable_send":
		if v, ok := m["service"].(string); ok {
			rec.Service = v
		}
		if v, ok := m["operation"].(string); ok {
			rec.Op = v
		}
		if v, ok := m["request_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.Request = string(decoded)
			}
		}
	case "durable_schedule_invoke":
		if v, ok := m["service"].(string); ok {
			rec.Service = v
		}
		if v, ok := m["operation"].(string); ok {
			rec.Op = v
		}
		if v, ok := m["request_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.Request = string(decoded)
			}
		}
		if v, ok := m["duration_ms"].(float64); ok {
			rec.DurationMs = int64(v)
		}
	case "await_child":
		if v, ok := m["run_id"].(string); ok {
			rec.RunID = v
		}
		if v, ok := m["response_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.Response = string(decoded)
			}
		} else if v, ok := m["response"].(string); ok {
			rec.Response = v
		}
		if v, ok := m["error"].(string); ok {
			rec.Err = v
		}
	case "heartbeat":
		if v, ok := m["service"].(string); ok {
			rec.Service = v
		}
		if v, ok := m["operation"].(string); ok {
			rec.Op = v
		}
	case "await_all_children":
		if v, ok := m["request_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.Request = string(decoded)
			}
		} else if v, ok := m["request"].(string); ok {
			rec.Request = v
		}
		if v, ok := m["response_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.Response = string(decoded)
			}
		} else if v, ok := m["response"].(string); ok {
			rec.Response = v
		}
	}
}
