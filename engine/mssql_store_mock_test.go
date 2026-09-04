package engine

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// MSSQLStore mock-based tests (no real DB required)
//
// These tests reuse the mockConnector / mockRowsResult / mockExecResult
// infrastructure defined in db_methods_test.go to verify that MSSQLStore
// methods handle error paths and edge cases correctly.
//
// Many MSSQLStore methods use beginTxWithContext which calls
// EXEC sp_set_session_context inside the transaction. Mock exec results
// for "sp_set_session_context" must be provided when testing those paths.
// ---------------------------------------------------------------------------

var testCtxMSSQL = context.Background()

// ---------------------------------------------------------------------------
// ClaimWorkflow / ClaimWorkflows
// ---------------------------------------------------------------------------

func TestMSSQLStore_ClaimWorkflow_NilWhenEmpty(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		// UPDATE ... OUTPUT returns no rows -> empty result
		{match: "SET status = 'running'", data: [][]driver.Value{}},
	}, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	wf, err := store.ClaimWorkflow(testCtxMSSQL, "worker-1")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if wf != nil {
		t.Error("expected nil when no workflows available")
	}
}

func TestMSSQLStore_ClaimWorkflow_Success(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	// MSSQL scanWorkflowInstanceExtra reads input as string, then unmarshals.
	inputJSON := `{"input":"x"}`
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SET status = 'running'",
			data: [][]driver.Value{
				{
					"wf-1",     // id
					"test-wf",  // def_name
					int64(1),   // def_version
					"running",  // status
					inputJSON,  // input (string for MSSQL)
					"worker-1", // assigned_to
					now,        // next_wake_at
					nil,        // tenant_id
					now,        // created_at
					nil,        // error_code
					nil,        // error_op
					int64(1),   // generation
					int64(0),   // priority
					"",         // trace_id
					"",         // pending_terminal_status
				},
			},
		},
	}, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	wf, err := store.ClaimWorkflow(testCtxMSSQL, "worker-1")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if wf == nil {
		t.Fatal("expected non-nil workflow")
	}
	if wf.ID != "wf-1" || wf.DefName != "test-wf" || wf.Status != "running" {
		t.Errorf("unexpected workflow: id=%s name=%s status=%s", wf.ID, wf.DefName, wf.Status)
	}
	if wf.AssignedTo != "worker-1" {
		t.Errorf("expected worker-1, got %q", wf.AssignedTo)
	}
	if wf.Generation != 1 {
		t.Errorf("expected generation 1, got %d", wf.Generation)
	}
}

func TestMSSQLStore_ClaimWorkflow_ScanError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SET status = 'running'",
			err:   errors.New("scan failed"),
		},
	}, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.ClaimWorkflow(testCtxMSSQL, "worker-1")
	if err == nil {
		t.Fatal("expected error from scan failure, got nil")
	}
	if !strings.Contains(err.Error(), "claim workflows") {
		t.Errorf("expected error to contain 'claim workflows', got: %v", err)
	}
}

func TestMSSQLStore_ClaimWorkflow_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("connection refused"), nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.ClaimWorkflow(testCtxMSSQL, "worker-1")
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("expected error to contain 'begin tx', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CompleteWorkflow
// ---------------------------------------------------------------------------

func TestMSSQLStore_CompleteWorkflow_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "SET status = 'done'", affected: 1},
		// Idempotency update is best-effort — may succeed.
		{match: "UPDATE idempotency_keys SET result", affected: 1},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.CompleteWorkflow(testCtxMSSQL, "wf-1", "worker-1", 0, `{"result":"ok"}`, nil)
	if err != nil {
		t.Fatalf("CompleteWorkflow: %v", err)
	}
}

func TestMSSQLStore_CompleteWorkflow_NilQueryState(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "SET status = 'done'", affected: 1},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.CompleteWorkflow(testCtxMSSQL, "wf-1", "worker-1", 0, `{}`, nil)
	if err != nil {
		t.Fatalf("CompleteWorkflow (nil qs): %v", err)
	}
}

func TestMSSQLStore_CompleteWorkflow_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("tx failed"), nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.CompleteWorkflow(testCtxMSSQL, "wf-1", "worker-1", 0, `{}`, nil)
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "complete workflow: begin") {
		t.Errorf("expected error to contain 'complete workflow: begin', got: %v", err)
	}
}

func TestMSSQLStore_CompleteWorkflow_IdempotencyUpdateFails(t *testing.T) {
	// Idempotency UPDATE is best-effort. When it fails, the error is logged
	// but CompleteWorkflow still succeeds.
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "SET status = 'done'", affected: 1},
		{match: "UPDATE idempotency_keys SET result", err: errors.New("idempotency failed")},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.CompleteWorkflow(testCtxMSSQL, "wf-1", "worker-1", 0, `{"result":"ok"}`, nil)
	if err != nil {
		t.Fatalf("CompleteWorkflow should succeed when idempotency update fails: %v", err)
	}
}

func TestMSSQLStore_CompleteWorkflow_UpdateError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "SET status = 'done'", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.CompleteWorkflow(testCtxMSSQL, "wf-1", "worker-1", 0, `{}`, nil)
	if err == nil {
		t.Fatal("expected error from update failure, got nil")
	}
}

// ---------------------------------------------------------------------------
// AppendEventHistoryBatch
// ---------------------------------------------------------------------------

func TestMSSQLStore_AppendEventHistoryBatch_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.AppendEventHistoryBatch(testCtxMSSQL, "wf-1", nil)
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch (nil): %v", err)
	}
	err = store.AppendEventHistoryBatch(testCtxMSSQL, "wf-1", []EventRecord{})
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch (empty): %v", err)
	}
}

func TestMSSQLStore_AppendEventHistoryBatch_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "INSERT INTO event_history", affected: 1},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	recs := []EventRecord{
		{
			Step:      0,
			EventType: "call",
			Service:   "svc",
			Op:        "op",
			Request:   `{}`,
			Response:  `{}`,
		},
	}
	err := store.AppendEventHistoryBatch(testCtxMSSQL, "wf-1", recs)
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}
}

func TestMSSQLStore_AppendEventHistoryBatch_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	recs := []EventRecord{{Step: 0, EventType: "call", Service: "svc"}}
	err := store.AppendEventHistoryBatch(testCtxMSSQL, "wf-1", recs)
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "append history batch: begin tx") {
		t.Errorf("expected error to contain 'append history batch: begin tx', got: %v", err)
	}
}

func TestMSSQLStore_AppendEventHistoryBatch_InsertError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "INSERT INTO event_history", err: errors.New("insert failed")},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	recs := []EventRecord{{Step: 0, EventType: "call", Service: "svc"}}
	err := store.AppendEventHistoryBatch(testCtxMSSQL, "wf-1", recs)
	if err == nil {
		t.Fatal("expected error from INSERT failure, got nil")
	}
	if !strings.Contains(err.Error(), "append events in tx: exec") {
		t.Errorf("expected error to contain 'append events in tx: exec', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AppendEventHistory (single event wrapper)
// ---------------------------------------------------------------------------

func TestMSSQLStore_AppendEventHistory_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "INSERT INTO event_history", affected: 1},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	rec := EventRecord{
		Step:      0,
		EventType: "call",
		Service:   "svc",
		Op:        "op",
	}
	err := store.AppendEventHistory(testCtxMSSQL, "wf-1", rec)
	if err != nil {
		t.Fatalf("AppendEventHistory: %v", err)
	}
}

// ---------------------------------------------------------------------------
// LoadEventHistory
// ---------------------------------------------------------------------------

func TestMSSQLStore_LoadEventHistory_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewMSSQLStore(db)
	history, err := store.LoadEventHistory(testCtxMSSQL, "wf-1")
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("expected empty history, got %d events", len(history))
	}
}

func TestMSSQLStore_LoadEventHistory_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM event_history", err: errors.New("query timeout")},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.LoadEventHistory(testCtxMSSQL, "wf-1")
	if err == nil {
		t.Fatal("expected error from query failure, got nil")
	}
	if !strings.Contains(err.Error(), "load history") {
		t.Errorf("expected error to contain 'load history', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Factory / constructor validation tests (no DB required)
// ---------------------------------------------------------------------------

func TestMSSQLStore_FactoryDefaultTTL(t *testing.T) {
	f := NewMSSQLStoreFactory("sqlserver://localhost")
	if f.idempotencyKeyTTL != 720*time.Hour {
		t.Errorf("default idempotencyKeyTTL = %v, want 720h", f.idempotencyKeyTTL)
	}

	f2 := NewMSSQLStoreFactory("sqlserver://localhost", 1*time.Hour)
	if f2.idempotencyKeyTTL != 1*time.Hour {
		t.Errorf("custom idempotencyKeyTTL = %v, want 1h", f2.idempotencyKeyTTL)
	}
}

func TestMSSQLStore_StoreDialect(t *testing.T) {
	store := NewMSSQLStore(nil)
	if store.dialect != DialectMSSQL {
		t.Errorf("dialect = %v, want DialectMSSQL", store.dialect)
	}
}

func TestMSSQLStore_DefaultTaskQueue(t *testing.T) {
	store := NewMSSQLStore(nil)
	if len(store.taskQueues) != 1 || store.taskQueues[0] != "default" {
		t.Errorf("taskQueues = %v, want [default]", store.taskQueues)
	}
}

func TestMSSQLStore_TenantPreservedOnCopy(t *testing.T) {
	store := NewMSSQLStore(nil)
	copy1 := store.WithTenant("11111111-1111-1111-1111-111111111111")
	copy2 := store.WithTenant("22222222-2222-2222-2222-222222222222")

	if store.tenantID != "00000000-0000-0000-0000-000000000000" {
		t.Error("original store tenantID changed")
	}
	if copy1.tenantID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("copy1 tenantID = %q", copy1.tenantID)
	}
	if copy2.tenantID != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("copy2 tenantID = %q", copy2.tenantID)
	}
}

// ---------------------------------------------------------------------------
// GetWASMLength
// ---------------------------------------------------------------------------

func TestMSSQLGetWASMLength(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		queryRowOk("len(wasm_bytes)", int64(2048)),
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	length, err := store.GetWASMLength(testCtxMSSQL, "test-wf", 1)
	if err != nil {
		t.Fatalf("GetWASMLength: %v", err)
	}
	if length != 2048 {
		t.Errorf("expected 2048, got %d", length)
	}
}

func TestMSSQLGetWASMLength_Error(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "len(wasm_bytes)", err: errors.New("db error")},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.GetWASMLength(testCtxMSSQL, "test-wf", 1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// LoadWASM
// ---------------------------------------------------------------------------

func TestMSSQLLoadWASM(t *testing.T) {
	wasmData := []byte("mock-wasm-binary-data")
	db := newMockDBForPostgres(t, []mockRowsResult{
		queryRowOk("wasm_bytes FROM workflow_defs", wasmData),
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	result, err := store.LoadWASM(testCtxMSSQL, "test-wf", 1)
	if err != nil {
		t.Fatalf("LoadWASM: %v", err)
	}
	if string(result) != string(wasmData) {
		t.Errorf("expected %q, got %q", wasmData, result)
	}
}

func TestMSSQLLoadWASM_NotFound(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "wasm_bytes FROM workflow_defs"},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.LoadWASM(testCtxMSSQL, "test-wf", 999)
	if err == nil {
		t.Fatal("expected error from not found, got nil")
	}
	if !strings.Contains(err.Error(), "wasm not found") {
		t.Errorf("expected 'wasm not found', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// LoadWorkflowConfig
// ---------------------------------------------------------------------------

func TestMSSQLLoadWorkflowConfig(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		queryRowOk("max_history_length", int64(500)),
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	mhl, err := store.LoadWorkflowConfig(testCtxMSSQL, "test-wf", 1)
	if err != nil {
		t.Fatalf("LoadWorkflowConfig: %v", err)
	}
	if mhl != 500 {
		t.Errorf("expected 500, got %d", mhl)
	}
}

func TestMSSQLLoadWorkflowConfig_NotFound(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "max_history_length"},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.LoadWorkflowConfig(testCtxMSSQL, "test-wf", 999)
	if err == nil {
		t.Fatal("expected error from not found, got nil")
	}
	if !strings.Contains(err.Error(), "workflow def not found") {
		t.Errorf("expected 'workflow def not found', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// LoadDAGSpec
// ---------------------------------------------------------------------------

func TestMSSQLLoadDAGSpec(t *testing.T) {
	dagJSON := []byte(`{"steps":["a","b"]}`)
	db := newMockDBForPostgres(t, []mockRowsResult{
		queryRowOk("dag_spec", dagJSON),
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	spec, err := store.LoadDAGSpec(testCtxMSSQL, "test-wf", 1)
	if err != nil {
		t.Fatalf("LoadDAGSpec: %v", err)
	}
	if string(spec) != string(dagJSON) {
		t.Errorf("expected %q, got %q", dagJSON, spec)
	}
}

func TestMSSQLLoadDAGSpec_NotFound(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "dag_spec"},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.LoadDAGSpec(testCtxMSSQL, "test-wf", 999)
	if err == nil {
		t.Fatal("expected error from not found, got nil")
	}
	if !strings.Contains(err.Error(), "workflow def not found") {
		t.Errorf("expected 'workflow def not found', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Close (factory)
// ---------------------------------------------------------------------------

func TestMSSQLClose(t *testing.T) {
	factory := NewMSSQLStoreFactory("sqlserver://localhost")
	if err := factory.Close(); err != nil {
		t.Errorf("Close on empty factory: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetChildCount
// ---------------------------------------------------------------------------

func TestMSSQLGetChildCount(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		queryRowOk("SELECT COUNT", int64(3)),
	}, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	count, err := store.GetChildCount(testCtxMSSQL, "parent-1")
	if err != nil {
		t.Fatalf("GetChildCount: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3, got %d", count)
	}
}

func TestMSSQLGetChildCount_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.GetChildCount(testCtxMSSQL, "parent-1")
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "get child count") {
		t.Errorf("expected error to contain 'get child count', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetEventCount
// ---------------------------------------------------------------------------

func TestMSSQLGetEventCount(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		queryRowOk("event_count FROM workflow_instances", int64(42)),
	}, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	count, err := store.GetEventCount(testCtxMSSQL, "wf-1")
	if err != nil {
		t.Fatalf("GetEventCount: %v", err)
	}
	if count != 42 {
		t.Errorf("expected 42, got %d", count)
	}
}

func TestMSSQLGetEventCount_Zero(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "event_count FROM workflow_instances"},
	}, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	count, err := store.GetEventCount(testCtxMSSQL, "wf-1")
	if err != nil {
		t.Fatalf("GetEventCount: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestMSSQLGetEventCount_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("tx failed"), nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.GetEventCount(testCtxMSSQL, "wf-1")
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
}

// ---------------------------------------------------------------------------
// GetConcurrencyKeyCount
// ---------------------------------------------------------------------------

func TestMSSQLGetConcurrencyKeyCount(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		queryRowOk("COUNT(*) FROM concurrency_keys", int64(5)),
	}, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	count, err := store.GetConcurrencyKeyCount(testCtxMSSQL, "wf-1")
	if err != nil {
		t.Fatalf("GetConcurrencyKeyCount: %v", err)
	}
	if count != 5 {
		t.Errorf("expected 5, got %d", count)
	}
}

func TestMSSQLGetConcurrencyKeyCount_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin err"), nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.GetConcurrencyKeyCount(testCtxMSSQL, "wf-1")
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
}

// ---------------------------------------------------------------------------
// UpdateStickyWorker
// ---------------------------------------------------------------------------

func TestMSSQLUpdateStickyWorker(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "SET sticky_worker_id", affected: 1},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.UpdateStickyWorker(testCtxMSSQL, "wf-1", "worker-1")
	if err != nil {
		t.Fatalf("UpdateStickyWorker: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ClearStickyWorker
// ---------------------------------------------------------------------------

func TestMSSQLClearStickyWorker(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "sticky_worker_id = NULL", affected: 1},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.ClearStickyWorker(testCtxMSSQL, "wf-1")
	if err != nil {
		t.Fatalf("ClearStickyWorker: %v", err)
	}
}

// ---------------------------------------------------------------------------
// MarkVersionDeprecated
// ---------------------------------------------------------------------------

func TestMSSQLMarkVersionDeprecated(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET deprecated", affected: 1},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.MarkVersionDeprecated(testCtxMSSQL, "test-wf", 1, true)
	if err != nil {
		t.Fatalf("MarkVersionDeprecated: %v", err)
	}
}

func TestMSSQLMarkVersionDeprecated_Error(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET deprecated", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.MarkVersionDeprecated(testCtxMSSQL, "test-wf", 1, true)
	if err == nil {
		t.Fatal("expected error from exec failure, got nil")
	}
	if !strings.Contains(err.Error(), "mark version deprecated") {
		t.Errorf("expected 'mark version deprecated', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// PurgeWorkflowDef
// ---------------------------------------------------------------------------

func TestMSSQLPurgeWorkflowDef(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "DELETE FROM workflow_defs", affected: 1},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.PurgeWorkflowDef(testCtxMSSQL, "test-wf", 1)
	if err != nil {
		t.Fatalf("PurgeWorkflowDef: %v", err)
	}
}

func TestMSSQLPurgeWorkflowDef_Error(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "DELETE FROM workflow_defs", err: errors.New("delete failed")},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.PurgeWorkflowDef(testCtxMSSQL, "test-wf", 1)
	if err == nil {
		t.Fatal("expected error from exec failure, got nil")
	}
	if !strings.Contains(err.Error(), "purge workflow def") {
		t.Errorf("expected 'purge workflow def', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TraceWorkflow
// ---------------------------------------------------------------------------

func TestMSSQLTraceWorkflow(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET trace_id", affected: 1},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.TraceWorkflow(testCtxMSSQL, "wf-1", "trace-abc")
	if err != nil {
		t.Fatalf("TraceWorkflow: %v", err)
	}
}

func TestMSSQLTraceWorkflow_Error(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET trace_id", err: errors.New("exec failed")},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.TraceWorkflow(testCtxMSSQL, "wf-1", "trace-abc")
	if err == nil {
		t.Fatal("expected error from exec failure, got nil")
	}
}

// ---------------------------------------------------------------------------
// ResolveLatestVersion
// ---------------------------------------------------------------------------

func TestMSSQLResolveLatestVersion(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		queryRowOk("MAX(version)", int64(3)),
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	version, err := store.ResolveLatestVersion(testCtxMSSQL, "test-wf")
	if err != nil {
		t.Fatalf("ResolveLatestVersion: %v", err)
	}
	if version != 3 {
		t.Errorf("expected 3, got %d", version)
	}
}

func TestMSSQLResolveLatestVersion_Zero(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		queryRowOk("MAX(version)", int64(0)),
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.ResolveLatestVersion(testCtxMSSQL, "test-wf")
	if err == nil {
		t.Fatal("expected error when version is 0, got nil")
	}
	if !strings.Contains(err.Error(), "no non-deprecated version") {
		t.Errorf("expected 'no non-deprecated version', got: %v", err)
	}
}

func TestMSSQLResolveLatestVersion_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "MAX(version)", err: errors.New("query error")},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.ResolveLatestVersion(testCtxMSSQL, "test-wf")
	if err == nil {
		t.Fatal("expected error from query failure, got nil")
	}
}

// ---------------------------------------------------------------------------
// ValidateVersion
// ---------------------------------------------------------------------------

func TestMSSQLValidateVersion_Valid(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		queryRowOk("CASE WHEN EXISTS", int64(1)),
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	valid, err := store.ValidateVersion(testCtxMSSQL, "test-wf", 1)
	if err != nil {
		t.Fatalf("ValidateVersion: %v", err)
	}
	if !valid {
		t.Error("expected valid=true")
	}
}

func TestMSSQLValidateVersion_Invalid(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		queryRowOk("CASE WHEN EXISTS", int64(0)),
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	valid, err := store.ValidateVersion(testCtxMSSQL, "test-wf", 999)
	if err != nil {
		t.Fatalf("ValidateVersion: %v", err)
	}
	if valid {
		t.Error("expected valid=false")
	}
}

func TestMSSQLValidateVersion_Error(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "CASE WHEN EXISTS", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.ValidateVersion(testCtxMSSQL, "test-wf", 1)
	if err == nil {
		t.Fatal("expected error from query failure, got nil")
	}
}
