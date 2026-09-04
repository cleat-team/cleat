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
// MySQLStore mock-based tests (no real DB required)
//
// These tests reuse the mockConnector / mockRowsResult / mockExecResult
// infrastructure defined in db_methods_test.go to verify that MySQLStore
// methods form correct SQL and handle errors properly.
// ---------------------------------------------------------------------------

var testCtxMySQL = context.Background()

// ---------------------------------------------------------------------------
// ClaimWorkflow
// ---------------------------------------------------------------------------

func TestMySQLStore_ClaimWorkflow_NilWhenEmpty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewMySQLStore(db)
	wf, err := store.ClaimWorkflow(testCtxMySQL, "worker-1")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if wf != nil {
		t.Error("expected nil when no workflows available")
	}
}

func TestMySQLStore_ClaimWorkflow_ReturnsFirst(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		// Step 1: SELECT FOR UPDATE — return one ID.
		{match: "SELECT id FROM", data: [][]driver.Value{{"wf-1"}}, consume: true},
		// Step 3: SELECT after update — return full workflow row.
		// Columns: id, def_name, def_version, status, input, assigned_to,
		//          next_wake_at, tenant_id, created_at, error_code, error_op,
		//          generation, priority, trace_id, pending_terminal_status
		{match: "COALESCE(priority", data: [][]driver.Value{{
			"wf-1",                  // id
			"test-wf",               // def_name
			int64(1),                // def_version
			"running",               // status
			[]byte(`{"input":"x"}`), // input
			"worker-1",              // assigned_to
			now,                     // next_wake_at
			nil,                     // tenant_id (NULL)
			now,                     // created_at
			nil,                     // error_code (NULL)
			nil,                     // error_op (NULL)
			int64(1),                // generation
			int64(0),                // priority
			"",                      // trace_id
			"",                      // pending_terminal_status
		}}},
	}, []mockExecResult{
		// Step 2: UPDATE
		{match: "UPDATE workflow_instances SET status = 'running'", affected: 1},
	})
	defer db.Close()

	store := NewMySQLStore(db)
	wf, err := store.ClaimWorkflow(testCtxMySQL, "worker-1")
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

func TestMySQLStore_ClaimWorkflow_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("connection refused"), nil)
	defer db.Close()

	store := NewMySQLStore(db)
	_, err := store.ClaimWorkflow(testCtxMySQL, "worker-1")
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "claim workflows: begin") {
		t.Errorf("expected error to contain 'claim workflows: begin', got: %v", err)
	}
}

func TestMySQLStore_ClaimWorkflow_SelectError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT id FROM", err: errors.New("SELECT failed")},
	}, nil)
	defer db.Close()

	store := NewMySQLStore(db)
	_, err := store.ClaimWorkflow(testCtxMySQL, "worker-1")
	if err == nil {
		t.Fatal("expected error from SELECT failure, got nil")
	}
	if !strings.Contains(err.Error(), "claim workflows: select") {
		t.Errorf("expected error to contain 'claim workflows: select', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CompleteWorkflow
// ---------------------------------------------------------------------------

func TestMySQLStore_CompleteWorkflow_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET status = 'done'", affected: 1},
		{match: "UPDATE idempotency_keys SET result", affected: 1},
	})
	defer db.Close()

	store := NewMySQLStore(db)
	err := store.CompleteWorkflow(testCtxMySQL, "wf-1", "worker-1", 0, `{"result":"ok"}`, nil)
	if err != nil {
		t.Fatalf("CompleteWorkflow: %v", err)
	}
}

func TestMySQLStore_CompleteWorkflow_NilQueryState(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET status = 'done'", affected: 1},
	})
	defer db.Close()

	store := NewMySQLStore(db)
	err := store.CompleteWorkflow(testCtxMySQL, "wf-1", "worker-1", 0, `{}`, nil)
	if err != nil {
		t.Fatalf("CompleteWorkflow (nil qs): %v", err)
	}
}

func TestMySQLStore_CompleteWorkflow_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("connection failed"), nil)
	defer db.Close()

	store := NewMySQLStore(db)
	err := store.CompleteWorkflow(testCtxMySQL, "wf-1", "worker-1", 0, `{}`, nil)
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "complete workflow: begin") {
		t.Errorf("expected error to contain 'complete workflow: begin', got: %v", err)
	}
}

// TestMySQLStore_CompleteWorkflow_UpdateError covers the path where the status
// UPDATE itself fails: CompleteWorkflow must return that error rather than
// swallowing it the way the idempotency UPDATE below is deliberately allowed to
// be swallowed.
//
// It was skipped unconditionally with its body commented out, and the stated
// reason -- "CompleteWorkflow execs DELETE+UPDATE, mock matches DELETE first"
// -- does not describe the code. MySQLStore.CompleteWorkflow execs no DELETE at
// all; it is two UPDATEs, on workflow_instances and then idempotency_keys.
//
// The actual problem was the match string. mockExecResult matches on a
// substring of the query text, and the query reads
//
//	UPDATE workflow_instances
//	SET status = 'done', ...
//
// so "UPDATE workflow_instances SET status = 'done'" is not a substring of it
// -- there is a newline and a tab between the table name and SET. The mock
// therefore never fired, and rather than being fixed the test was disabled with
// a guess about why. TestMySQLStore_CompleteWorkflow_IdempotencyUpdateFails,
// three lines below, has always matched on "SET status = 'done'" and works.
//
// FailWorkflow has a near-identical statement, so the match is kept specific to
// 'done' rather than 'failed'.
func TestMySQLStore_CompleteWorkflow_UpdateError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET status = 'done'", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewMySQLStore(db)
	err := store.CompleteWorkflow(testCtxMySQL, "wf-1", "worker-1", 0, `{}`, nil)
	if err == nil {
		t.Fatal("expected error from update failure, got nil")
	}
	// Naming the injected error, not just asserting non-nil. An unmatched mock
	// returns zero rows affected, which CompleteWorkflow turns into
	// ErrFenceLost -- also non-nil, and it would let this test pass without the
	// mock ever having fired. That is exactly how the original match string
	// could have been declared working.
	if !strings.Contains(err.Error(), "update failed") {
		t.Errorf("CompleteWorkflow returned %v, want it to carry the injected "+
			"\"update failed\" -- a different error means the mock did not match "+
			"the UPDATE and the test is not exercising this path", err)
	}
}

func TestMySQLStore_CompleteWorkflow_IdempotencyUpdateFails(t *testing.T) {
	// Idempotency UPDATE is best-effort. When it fails, the error is logged
	// but CompleteWorkflow still succeeds.
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET status = 'done'", affected: 1},
		{match: "UPDATE idempotency_keys SET result", err: errors.New("idempotency update failed")},
	})
	defer db.Close()

	store := NewMySQLStore(db)
	err := store.CompleteWorkflow(testCtxMySQL, "wf-1", "worker-1", 0, `{}`, nil)
	if err != nil {
		t.Fatalf("CompleteWorkflow should succeed even when idempotency update fails: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AppendEventHistoryBatch
// ---------------------------------------------------------------------------

func TestMySQLStore_AppendEventHistoryBatch_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewMySQLStore(db)
	err := store.AppendEventHistoryBatch(testCtxMySQL, "wf-1", nil)
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch (nil): %v", err)
	}
	err = store.AppendEventHistoryBatch(testCtxMySQL, "wf-1", []EventRecord{})
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch (empty): %v", err)
	}
}

func TestMySQLStore_AppendEventHistoryBatch_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT IGNORE INTO event_history", affected: 1},
	})
	defer db.Close()

	store := NewMySQLStore(db)
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
	err := store.AppendEventHistoryBatch(testCtxMySQL, "wf-1", recs)
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}
}

func TestMySQLStore_AppendEventHistoryBatch_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("tx failed"), nil)
	defer db.Close()

	store := NewMySQLStore(db)
	recs := []EventRecord{{Step: 0, EventType: "call"}}
	err := store.AppendEventHistoryBatch(testCtxMySQL, "wf-1", recs)
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "append history batch: begin tx") {
		t.Errorf("expected error to contain 'append history batch: begin tx', got: %v", err)
	}
}

func TestMySQLStore_AppendEventHistoryBatch_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT IGNORE INTO event_history", err: errors.New("insert failed")},
	})
	defer db.Close()

	store := NewMySQLStore(db)
	recs := []EventRecord{{Step: 0, EventType: "call", Service: "svc"}}
	err := store.AppendEventHistoryBatch(testCtxMySQL, "wf-1", recs)
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

func TestMySQLStore_AppendEventHistory_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT IGNORE INTO event_history", affected: 1},
	})
	defer db.Close()

	store := NewMySQLStore(db)
	rec := EventRecord{
		Step:      0,
		EventType: "call",
		Service:   "svc",
		Op:        "op",
	}
	err := store.AppendEventHistory(testCtxMySQL, "wf-1", rec)
	if err != nil {
		t.Fatalf("AppendEventHistory: %v", err)
	}
}

// ---------------------------------------------------------------------------
// LoadEventHistory
// ---------------------------------------------------------------------------

func TestMySQLStore_LoadEventHistory_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewMySQLStore(db)
	history, err := store.LoadEventHistory(testCtxMySQL, "wf-1")
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("expected empty history, got %d events", len(history))
	}
}

func TestMySQLStore_LoadEventHistory_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM event_history", err: errors.New("query timeout")},
	}, nil)
	defer db.Close()

	store := NewMySQLStore(db)
	_, err := store.LoadEventHistory(testCtxMySQL, "wf-1")
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

func TestMySQLStore_NewStoreDefaults(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewMySQLStore(db)
	if store == nil {
		t.Fatal("NewMySQLStore returned nil")
	}
	if store.tenantID != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("default tenantID = %q, want zero UUID", store.tenantID)
	}
	if store.dialect != DialectMySQL {
		t.Errorf("dialect = %v, want DialectMySQL", store.dialect)
	}
	if len(store.taskQueues) != 1 || store.taskQueues[0] != "default" {
		t.Errorf("taskQueues = %v, want [default]", store.taskQueues)
	}
	if store.idempotencyKeyTTL != 720*time.Hour {
		t.Errorf("idempotencyKeyTTL = %v, want 720h", store.idempotencyKeyTTL)
	}
}

func TestMySQLStore_CustomTaskQueues(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewMySQLStore(db, "gpu", "high-memory")
	if len(store.taskQueues) != 2 {
		t.Fatalf("expected 2 task queues, got %d", len(store.taskQueues))
	}
	if store.taskQueues[0] != "gpu" || store.taskQueues[1] != "high-memory" {
		t.Errorf("taskQueues = %v", store.taskQueues)
	}
}

func TestMySQLStore_WithTenant(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewMySQLStore(db)
	tenantStore := store.WithTenant("tenant-abc")
	if tenantStore.tenantID != "tenant-abc" {
		t.Errorf("tenantID = %q, want %q", tenantStore.tenantID, "tenant-abc")
	}
	if store.tenantID == "tenant-abc" {
		t.Error("WithTenant mutated original store")
	}
}

func TestMySQLStore_WithReadRedactionDisabled(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewMySQLStore(db)
	result := store.WithReadRedactionDisabled(true)
	if !result.disableReadRedaction {
		t.Error("disableReadRedaction should be true")
	}
	if store.disableReadRedaction {
		t.Error("original store should not be mutated")
	}
}

func TestMySQLStore_WithIdempotencyKeyTTL(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewMySQLStore(db)
	ttl := 5 * time.Minute
	result := store.WithIdempotencyKeyTTL(ttl)
	if result.idempotencyKeyTTL != ttl {
		t.Errorf("idempotencyKeyTTL = %v, want %v", result.idempotencyKeyTTL, ttl)
	}
	if store.idempotencyKeyTTL == ttl {
		t.Error("original store should not be mutated")
	}
}

func TestMySQLStore_WithEncryption(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewMySQLStore(db)
	enc := &PayloadEncryption{}
	result := store.WithEncryption(enc, true)
	if result.encryption != enc {
		t.Error("encryption not set")
	}
	if !result.encryptSensitivePayloads {
		t.Error("encryptSensitivePayloads should be true")
	}
	if store.encryption != nil {
		t.Error("original store encryption should be nil")
	}
}

// ---------------------------------------------------------------------------
// Heartbeat
// ---------------------------------------------------------------------------

func TestMySQLHeartbeat_Owned(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances", affected: 1},
	})
	owned, err := store.Heartbeat(testCtxMySQL, "wf-1", "worker-1", 0)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !owned {
		t.Error("expected owned=true when RowsAffected=1")
	}
}

func TestMySQLHeartbeat_NotOwned(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances", affected: 0},
	})
	owned, err := store.Heartbeat(testCtxMySQL, "wf-1", "worker-1", 0)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if owned {
		t.Error("expected owned=false when RowsAffected=0")
	}
}

func TestMySQLHeartbeat_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances", err: errors.New("exec failed")},
	})
	_, err := store.Heartbeat(testCtxMySQL, "wf-1", "worker-1", 0)
	if err == nil {
		t.Fatal("expected error from exec failure, got nil")
	}
	if !strings.Contains(err.Error(), "heartbeat") {
		t.Errorf("expected 'heartbeat', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// BatchHeartbeat
// ---------------------------------------------------------------------------

func TestMySQLBatchHeartbeat(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances", affected: 5},
	})
	n, err := store.BatchHeartbeat(testCtxMySQL, "worker-1")
	if err != nil {
		t.Fatalf("BatchHeartbeat: %v", err)
	}
	if n != 5 {
		t.Errorf("expected 5, got %d", n)
	}
}

func TestMySQLBatchHeartbeat_Zero(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances", affected: 0},
	})
	n, err := store.BatchHeartbeat(testCtxMySQL, "worker-1")
	if err != nil {
		t.Fatalf("BatchHeartbeat: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestMySQLBatchHeartbeat_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances", err: errors.New("update failed")},
	})
	_, err := store.BatchHeartbeat(testCtxMySQL, "worker-1")
	if err == nil {
		t.Fatal("expected error from exec failure, got nil")
	}
	if !strings.Contains(err.Error(), "batch heartbeat") {
		t.Errorf("expected 'batch heartbeat', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetChildResult
// ---------------------------------------------------------------------------

func TestMySQLGetChildResult_Done(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("COALESCE(result, '{}')", `{"output":"ok"}`, "done"),
	}, nil)
	result, done, err := store.GetChildResult(testCtxMySQL, "child-1")
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if !done {
		t.Error("expected done=true")
	}
	if result != `{"output":"ok"}` {
		t.Errorf("expected '{\"output\":\"ok\"}', got %q", result)
	}
}

func TestMySQLGetChildResult_NotDone(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("COALESCE(result, '{}')", "{}", "running"),
	}, nil)
	result, done, err := store.GetChildResult(testCtxMySQL, "child-1")
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if done {
		t.Error("expected done=false for running workflow")
	}
	if result != "" {
		t.Errorf("expected empty result, got %q", result)
	}
}

func TestMySQLGetChildResult_NotFound(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "COALESCE(result, '{}')"},
	}, nil)
	_, done, err := store.GetChildResult(testCtxMySQL, "nonexistent")
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if done {
		t.Error("expected done=false when workflow not found")
	}
}

func TestMySQLGetChildResult_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "COALESCE(result, '{}')", err: errors.New("query error")},
	}, nil)
	_, _, err := store.GetChildResult(testCtxMySQL, "child-1")
	if err == nil {
		t.Fatal("expected error from query failure, got nil")
	}
}

// ---------------------------------------------------------------------------
// UpdateStickyWorker
// ---------------------------------------------------------------------------

func TestMySQLUpdateStickyWorker(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "SET sticky_worker_id", affected: 1},
	})
	err := store.UpdateStickyWorker(testCtxMySQL, "wf-1", "worker-1")
	if err != nil {
		t.Fatalf("UpdateStickyWorker: %v", err)
	}
}

func TestMySQLUpdateStickyWorker_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "SET sticky_worker_id", err: errors.New("update failed")},
	})
	err := store.UpdateStickyWorker(testCtxMySQL, "wf-1", "worker-1")
	if err == nil {
		t.Fatal("expected error from exec failure, got nil")
	}
	if !strings.Contains(err.Error(), "update sticky worker") {
		t.Errorf("expected 'update sticky worker', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetAllowedSignalCallers
// ---------------------------------------------------------------------------

func TestMySQLGetAllowedSignalCallers(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("allowed_signals FROM workflow_instances", `["caller1","caller2"]`),
	}, nil)
	callers, err := store.GetAllowedSignalCallers(testCtxMySQL, "wf-1")
	if err != nil {
		t.Fatalf("GetAllowedSignalCallers: %v", err)
	}
	if len(callers) != 2 || callers[0] != "caller1" || callers[1] != "caller2" {
		t.Errorf("expected [caller1 caller2], got %v", callers)
	}
}

func TestMySQLGetAllowedSignalCallers_Null(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("allowed_signals FROM workflow_instances", ""),
	}, nil)
	callers, err := store.GetAllowedSignalCallers(testCtxMySQL, "wf-1")
	if err != nil {
		t.Fatalf("GetAllowedSignalCallers: %v", err)
	}
	if len(callers) != 0 {
		t.Errorf("expected empty callers, got %v", callers)
	}
}

func TestMySQLGetAllowedSignalCallers_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "allowed_signals FROM workflow_instances", err: errors.New("query error")},
	}, nil)
	_, err := store.GetAllowedSignalCallers(testCtxMySQL, "wf-1")
	if err == nil {
		t.Fatal("expected error from query failure, got nil")
	}
}

// ---------------------------------------------------------------------------
// DriverName / Dialect (factory)
// ---------------------------------------------------------------------------

func TestMySQLDriverName(t *testing.T) {
	f := NewMySQLStoreFactory(nil, "")
	if got := f.DriverName(); got != "mysql" {
		t.Errorf("DriverName = %q, want 'mysql'", got)
	}
}

func TestMySQLDialect(t *testing.T) {
	f := NewMySQLStoreFactory(nil, "")
	if got := f.Dialect(); got != DialectMySQL {
		t.Errorf("Dialect = %v, want DialectMySQL", got)
	}
}
