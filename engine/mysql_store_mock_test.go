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
		//          generation, priority, trace_id
		{match: "COALESCE(priority", data: [][]driver.Value{{
			"wf-1",                   // id
			"test-wf",                // def_name
			int64(1),                 // def_version
			"running",                // status
			[]byte(`{"input":"x"}`),  // input
			"worker-1",               // assigned_to
			now,                      // next_wake_at
			nil,                      // tenant_id (NULL)
			now,                      // created_at
			nil,                      // error_code (NULL)
			nil,                      // error_op (NULL)
			int64(1),                 // generation
			int64(0),                 // priority
			"",                       // trace_id
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
		{match: "UPDATE workflow_instances SET status = 'done'", affected: 1},
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
		{match: "UPDATE workflow_instances SET status = 'done'", affected: 1},
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

func TestMySQLStore_CompleteWorkflow_UpdateError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances SET status = 'done'", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewMySQLStore(db)
	err := store.CompleteWorkflow(testCtxMySQL, "wf-1", "worker-1", 0, `{}`, nil)
	if err == nil {
		t.Fatal("expected error from update failure, got nil")
	}
}

func TestMySQLStore_CompleteWorkflow_IdempotencyUpdateFails(t *testing.T) {
	// Idempotency UPDATE is best-effort. When it fails, the error is logged
	// but CompleteWorkflow still succeeds.
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances SET status = 'done'", affected: 1},
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

