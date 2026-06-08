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
					"wf-1",              // id
					"test-wf",           // def_name
					int64(1),            // def_version
					"running",           // status
					inputJSON,           // input (string for MSSQL)
					"worker-1",          // assigned_to
					now,                 // next_wake_at
					nil,                 // tenant_id
					now,                 // created_at
					nil,                 // error_code
					nil,                 // error_op
					int64(1),            // generation
					int64(0),            // priority
					"",                  // trace_id
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
