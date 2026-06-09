package engine

import (
	"database/sql/driver"
	"fmt"
	"testing"
	"time"
)

// Gap coverage tests for PostgresStore methods not yet covered by existing tests.
// Covers 14 uncovered methods with 33 test functions.

// ---------------------------------------------------------------------------
// BatchHeartbeat
// ---------------------------------------------------------------------------

func TestGap_BatchHeartbeat(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	n, err := store.BatchHeartbeat(testCtx, "worker-1")
	if err != nil {
		t.Fatalf("BatchHeartbeat: %v", err)
	}
	if n != 0 {
		t.Logf("BatchHeartbeat returned %d rows (expected 0 with noop driver)", n)
	}
}

func TestGap_BatchHeartbeat_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, fmt.Errorf("tx begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.BatchHeartbeat(testCtx, "worker-1")
	if err == nil {
		t.Fatal("expected error from BatchHeartbeat when BeginTx fails")
	}
}

// ---------------------------------------------------------------------------
// CountEventHistory
// ---------------------------------------------------------------------------

func TestGap_CountEventHistory(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "COUNT(*)", data: [][]driver.Value{{int64(5)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	count, err := store.CountEventHistory(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("CountEventHistory: %v", err)
	}
	if count != 5 {
		t.Errorf("expected 5, got %d", count)
	}
}

func TestGap_CountEventHistory_Error(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "COUNT(*)", err: fmt.Errorf("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.CountEventHistory(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected error from CountEventHistory")
	}
}

// ---------------------------------------------------------------------------
// DeleteDeadLetteredWorkflows
// ---------------------------------------------------------------------------

func TestGap_DeleteDeadLetteredWorkflows(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	n, err := store.DeleteDeadLetteredWorkflows(testCtx, time.Now())
	if err != nil {
		t.Fatalf("DeleteDeadLetteredWorkflows: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestGap_DeleteDeadLetteredWorkflows_Some(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "DELETE FROM workflow_instances", affected: 500, consume: true},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	n, err := store.DeleteDeadLetteredWorkflows(testCtx, time.Now())
	if err != nil {
		t.Fatalf("DeleteDeadLetteredWorkflows: %v", err)
	}
	if n != 500 {
		t.Errorf("expected 500, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// DeleteExpiredEvents
// ---------------------------------------------------------------------------

func TestGap_DeleteExpiredEvents(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	n, err := store.DeleteExpiredEvents(testCtx, time.Now())
	if err != nil {
		t.Fatalf("DeleteExpiredEvents: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestGap_DeleteExpiredEvents_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, fmt.Errorf("tx begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.DeleteExpiredEvents(testCtx, time.Now())
	if err == nil {
		t.Fatal("expected error from DeleteExpiredEvents when BeginTx fails")
	}
}

// ---------------------------------------------------------------------------
// GetAllowedSignalCallers
// ---------------------------------------------------------------------------

func TestGap_GetAllowedSignalCallers(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "allowed_signals", data: [][]driver.Value{{`["caller1","caller2"]`}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	callers, err := store.GetAllowedSignalCallers(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetAllowedSignalCallers: %v", err)
	}
	if len(callers) != 2 || callers[0] != "caller1" || callers[1] != "caller2" {
		t.Errorf("unexpected callers: %v", callers)
	}
}

func TestGap_GetAllowedSignalCallers_Null(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "allowed_signals", data: [][]driver.Value{{nil}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	callers, err := store.GetAllowedSignalCallers(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetAllowedSignalCallers (null): %v", err)
	}
	if callers != nil {
		t.Errorf("expected nil callers for null value, got %v", callers)
	}
}

func TestGap_GetAllowedSignalCallers_Empty(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "allowed_signals", data: [][]driver.Value{{""}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	callers, err := store.GetAllowedSignalCallers(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetAllowedSignalCallers (empty): %v", err)
	}
	if callers != nil {
		t.Errorf("expected nil callers for empty string, got %v", callers)
	}
}

func TestGap_GetAllowedSignalCallers_ErrNoRows(t *testing.T) {
	// Use a noop DB so QueryRow returns ErrNoRows.
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	callers, err := store.GetAllowedSignalCallers(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetAllowedSignalCallers (no rows): %v", err)
	}
	if callers != nil {
		t.Errorf("expected nil callers, got %v", callers)
	}
}

// ---------------------------------------------------------------------------
// GetChildCount
// ---------------------------------------------------------------------------

func TestGap_GetChildCount(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "parent_workflow_id", data: [][]driver.Value{{int64(3)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	count, err := store.GetChildCount(testCtx, "parent-1")
	if err != nil {
		t.Fatalf("GetChildCount: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3, got %d", count)
	}
}

func TestGap_GetChildCount_Error(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "parent_workflow_id", err: fmt.Errorf("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetChildCount(testCtx, "parent-1")
	if err == nil {
		t.Fatal("expected error from GetChildCount")
	}
}

// ---------------------------------------------------------------------------
// GetChildResultInSchema
// ---------------------------------------------------------------------------

func TestGap_GetChildResultInSchema_Done(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "COALESCE(result", data: [][]driver.Value{{`{"ok":true}`, "done"}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	result, done, err := store.GetChildResultInSchema(testCtx, "target_schema", "child-run-id")
	if err != nil {
		t.Fatalf("GetChildResultInSchema: %v", err)
	}
	if !done {
		t.Error("expected done=true")
	}
	if result != `{"ok":true}` {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestGap_GetChildResultInSchema_Running(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "COALESCE(result", data: [][]driver.Value{{`{}`, "running"}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	result, done, err := store.GetChildResultInSchema(testCtx, "target_schema", "child-run-id")
	if err != nil {
		t.Fatalf("GetChildResultInSchema (running): %v", err)
	}
	if done {
		t.Error("expected done=false for running workflow")
	}
	if result != "" {
		t.Logf("result for running workflow: %q", result)
	}
}

func TestGap_GetChildResultInSchema_NoRows(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	result, done, err := store.GetChildResultInSchema(testCtx, "target_schema", "nonexistent")
	if err != nil {
		t.Fatalf("GetChildResultInSchema (no rows): %v", err)
	}
	if done {
		t.Error("expected done=false for missing workflow")
	}
	if result != "" {
		t.Errorf("expected empty result, got %q", result)
	}
}

func TestGap_GetChildResultInSchema_Error(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "COALESCE(result", err: fmt.Errorf("db error")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, _, err := store.GetChildResultInSchema(testCtx, "target_schema", "child-run-id")
	if err == nil {
		t.Fatal("expected error from GetChildResultInSchema")
	}
}

// ---------------------------------------------------------------------------
// GetWASMLength
// ---------------------------------------------------------------------------

func TestGap_GetWASMLength(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "length(wasm_bytes)", data: [][]driver.Value{{int64(1024)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	length, err := store.GetWASMLength(testCtx, "my-wf", 1)
	if err != nil {
		t.Fatalf("GetWASMLength: %v", err)
	}
	if length != 1024 {
		t.Errorf("expected 1024, got %d", length)
	}
}

func TestGap_GetWASMLength_Error(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "length(wasm_bytes)", err: fmt.Errorf("not found")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetWASMLength(testCtx, "my-wf", 99)
	if err == nil {
		t.Fatal("expected error from GetWASMLength")
	}
}

// ---------------------------------------------------------------------------
// LoadEventHistoryPaginated
// ---------------------------------------------------------------------------

func TestGap_LoadEventHistoryPaginated(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "event_history",
			data: [][]driver.Value{{
				int64(0), "timer_started", // step, event_type
				nil, nil, nil, nil, nil, // service, op, request, response, errMsg
				nil, nil, nil, nil, nil, // durationMs, signalNames, timeoutMs, signalName, signalPayload
				nil, nil, nil, nil, nil, nil, // deferDesc, deferID, childName, childInput, runID, newInput
				nil, nil, nil, nil, nil, // pluginName, pluginFunc, pluginInput, pluginOutput, pluginErr
				nil,                // payload
				nil, nil, nil, nil, // promiseName, promiseID, promiseResult, promiseError
				nil, // createdAt
			}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	history, err := store.LoadEventHistoryPaginated(testCtx, "wf-1", 0, 100)
	if err != nil {
		t.Fatalf("LoadEventHistoryPaginated: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(history))
	}
	if history[0].Step != 0 {
		t.Errorf("expected step 0, got %d", history[0].Step)
	}
	if history[0].EventType != "timer_started" {
		t.Errorf("expected timer_started, got %q", history[0].EventType)
	}
}

func TestGap_LoadEventHistoryPaginated_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	history, err := store.LoadEventHistoryPaginated(testCtx, "wf-1", 0, 100)
	if err != nil {
		t.Fatalf("LoadEventHistoryPaginated (empty): %v", err)
	}
	if len(history) != 0 {
		t.Errorf("expected 0 events, got %d", len(history))
	}
}

func TestGap_LoadEventHistoryPaginated_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, fmt.Errorf("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.LoadEventHistoryPaginated(testCtx, "wf-1", 0, 100)
	if err == nil {
		t.Fatal("expected error from LoadEventHistoryPaginated when BeginTx fails")
	}
}

// ---------------------------------------------------------------------------
// MoveToDeadLetterQueue
// ---------------------------------------------------------------------------

func TestGap_MoveToDeadLetterQueue(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.MoveToDeadLetterQueue(testCtx, "wf-1", "worker-1", 0, "err msg", "ERR_CODE", "op")
	if err != nil {
		t.Fatalf("MoveToDeadLetterQueue: %v", err)
	}
}

func TestGap_MoveToDeadLetterQueue_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, fmt.Errorf("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.MoveToDeadLetterQueue(testCtx, "wf-1", "worker-1", 0, "err", "code", "op")
	if err == nil {
		t.Fatal("expected error from MoveToDeadLetterQueue when BeginTx fails")
	}
}

// ---------------------------------------------------------------------------
// ResolveTenantFromAPIKey
// ---------------------------------------------------------------------------

func TestGap_ResolveTenantFromAPIKey(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "tenant_api_keys", data: [][]driver.Value{{"550e8400-e29b-41d4-a716-446655440000"}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	tid, err := store.ResolveTenantFromAPIKey(testCtx, []byte("valid-key-hash"))
	if err != nil {
		t.Fatalf("ResolveTenantFromAPIKey: %v", err)
	}
	if tid.String() != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("expected 550e8400-e29b-41d4-a716-446655440000, got %v", tid)
	}
}

func TestGap_ResolveTenantFromAPIKey_NoRows(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "tenant_api_keys", err: fmt.Errorf("not found")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ResolveTenantFromAPIKey(testCtx, []byte("bad-key"))
	if err == nil {
		t.Fatal("expected error from ResolveTenantFromAPIKey for unknown key")
	}
}

// ---------------------------------------------------------------------------
// RetryWorkflow
// ---------------------------------------------------------------------------

func TestGap_RetryWorkflow(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RetryWorkflow(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("RetryWorkflow: %v", err)
	}
}

func TestGap_RetryWorkflow_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, fmt.Errorf("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RetryWorkflow(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected error from RetryWorkflow when BeginTx fails")
	}
}

// ---------------------------------------------------------------------------
// StartChildWorkflowInSchema
// ---------------------------------------------------------------------------

func TestGap_StartChildWorkflowInSchema(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "gen_random_uuid", data: [][]driver.Value{{"child-run-123"}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	runID, err := store.StartChildWorkflowInSchema(testCtx, "target_schema", "parent-1", "child-wf", `{}`, 0, "ABANDON", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflowInSchema: %v", err)
	}
	if runID != "child-run-123" {
		t.Errorf("expected child-run-123, got %q", runID)
	}
}

func TestGap_StartChildWorkflowInSchema_Error(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "gen_random_uuid", err: fmt.Errorf("insert failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.StartChildWorkflowInSchema(testCtx, "target_schema", "parent-1", "child-wf", `{}`, 0, "ABANDON", 0)
	if err == nil {
		t.Fatal("expected error from StartChildWorkflowInSchema")
	}
}

// ---------------------------------------------------------------------------
// TerminateWorkflow
// ---------------------------------------------------------------------------

func TestGap_TerminateWorkflow(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.TerminateWorkflow(testCtx, "wf-1", "user requested")
	if err != nil {
		t.Fatalf("TerminateWorkflow: %v", err)
	}
}

func TestGap_TerminateWorkflow_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, fmt.Errorf("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.TerminateWorkflow(testCtx, "wf-1", "error")
	if err == nil {
		t.Fatal("expected error from TerminateWorkflow when BeginTx fails")
	}
}
