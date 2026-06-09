package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Enhanced mock SQL driver for testing PostgresStore methods that need to
// return specific rows from queries or specific RowsAffected from Exec.
//
// We extend the noopConnector pattern from db_behavioral_test.go with a
// configurable mock that matches SQL queries by substring.
// ---------------------------------------------------------------------------

// mockRowsResult associates a SQL substring match with rows to return.
type mockRowsResult struct {
	match   string
	data    [][]driver.Value // each element is one row
	consume bool             // if true, this result is removed after first use (for sequential matching)
	err     error            // if non-nil, return this error from Query
}

// mockExecResult associates a SQL substring match with a RowsAffected count.
type mockExecResult struct {
	match    string
	affected int64
	err      error // if non-nil, return this error from Exec
	consume  bool  // if true, this result is removed after first use
}

// mockConnector implements driver.Connector and returns mock connections
// that serve pre-configured results.
type mockConnector struct {
	rowsResults []mockRowsResult
	execResults []mockExecResult
	beginErr    error // if set, Begin() returns this error
	commitErr   error // if set, Commit() returns this error
}

func (c *mockConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &mockConn{
		rowsResults: c.rowsResults,
		execResults: c.execResults,
		beginErr:    c.beginErr,
		commitErr:   c.commitErr,
	}, nil
}

func (c *mockConnector) Driver() driver.Driver {
	return &noopDriver{}
}

type mockConn struct {
	rowsResults []mockRowsResult
	execResults []mockExecResult
	beginErr    error
	commitErr   error
}

func (c *mockConn) Prepare(query string) (driver.Stmt, error) {
	return &mockStmt{
		query:       query,
		rowsResults: c.rowsResults,
		execResults: c.execResults,
	}, nil
}

func (c *mockConn) Close() error { return nil }

func (c *mockConn) Begin() (driver.Tx, error) {
	if c.beginErr != nil {
		return nil, c.beginErr
	}
	return &mockTx{commitErr: c.commitErr}, nil
}

func (c *mockConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.beginErr != nil {
		return nil, c.beginErr
	}
	return &mockTx{commitErr: c.commitErr}, nil
}

// mockTx implements driver.Tx with configurable commit error.
type mockTx struct {
	commitErr error
}

func (tx *mockTx) Commit() error   { return tx.commitErr }
func (tx *mockTx) Rollback() error { return nil }

// mockStmt implements driver.Stmt with configurable results.
type mockStmt struct {
	query       string
	rowsResults []mockRowsResult
	execResults []mockExecResult
}

func (s *mockStmt) Close() error { return nil }

func (s *mockStmt) NumInput() int { return -1 }

func (s *mockStmt) Exec(_ []driver.Value) (driver.Result, error) {
	for i, er := range s.execResults {
		if er.match == "" {
			continue
		}
		if strings.Contains(s.query, er.match) {
			if er.consume {
				s.execResults[i].match = ""
			}
			if er.err != nil {
				return nil, er.err
			}
			return &mockResult{affected: er.affected}, nil
		}
	}
	return &mockResult{}, nil
}

func (s *mockStmt) Query(_ []driver.Value) (driver.Rows, error) {
	for i, rr := range s.rowsResults {
		if rr.match == "" {
			continue
		}
		if strings.Contains(s.query, rr.match) {
			if rr.err != nil {
				return nil, rr.err
			}
			if rr.consume {
				s.rowsResults[i].match = "" // mark as consumed so subsequent queries fall through
			}
			return &mockRows{data: rr.data}, nil
		}
	}
	return &mockRows{}, nil
}

// mockResult implements driver.Result with a configurable RowsAffected.
type mockResult struct {
	affected int64
}

func (r *mockResult) LastInsertId() (int64, error) { return 0, nil }
func (r *mockResult) RowsAffected() (int64, error) { return r.affected, nil }

// mockRows implements driver.Rows with pre-configured data.
type mockRows struct {
	data [][]driver.Value
	pos  int
}

func (r *mockRows) Columns() []string {
	if len(r.data) == 0 {
		return nil
	}
	return make([]string, len(r.data[0]))
}

func (r *mockRows) Close() error { return nil }

func (r *mockRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// newMockDBForPostgres creates a *sql.DB backed by a mock connector that returns
// the configured rows and exec results based on SQL substring matching.
// Named differently from child_version_test.go's newMockDB to avoid conflict.
func newMockDBForPostgres(t *testing.T, rows []mockRowsResult, execs []mockExecResult) *sql.DB {
	t.Helper()
	return sql.OpenDB(&mockConnector{
		rowsResults: rows,
		execResults: execs,
	})
}

// newMockDBWithErrors creates a *sql.DB backed by a mock connector with
// configurable transaction-level error injection for testing error paths.
func newMockDBWithErrors(t *testing.T, rows []mockRowsResult, execs []mockExecResult, beginErr, commitErr error) *sql.DB {
	t.Helper()
	return sql.OpenDB(&mockConnector{
		rowsResults: rows,
		execResults: execs,
		beginErr:    beginErr,
		commitErr:   commitErr,
	})
}

// ---------------------------------------------------------------------------
// Utility: shortcut context for tests
// ---------------------------------------------------------------------------

var testCtx = context.Background()

// ---------------------------------------------------------------------------
// Exec-only methods (work with the basic noop driver)
// ---------------------------------------------------------------------------

func TestPostgresStore_TraceWorkflow(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.TraceWorkflow(testCtx, "wf-1", "trace-abc")
	if err != nil {
		t.Fatalf("TraceWorkflow: %v", err)
	}
}

func TestPostgresStore_DeployWorkflowDef(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	def := &WorkflowDef{
		Name:       "my-workflow",
		Version:    1,
		WASMBytes:  []byte("wasm"),
		ABIVersion: 1,
		MinVersion: 0,
		PluginDeps: map[string]string{"plugin-a": "1.0"},
		Deprecated: false,
	}
	err := store.DeployWorkflowDef(testCtx, def)
	if err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
}

func TestPostgresStore_DeployWorkflowDef_NilPluginDeps(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	def := &WorkflowDef{
		Name:       "my-workflow",
		Version:    1,
		WASMBytes:  []byte("wasm"),
		ABIVersion: 1,
		MinVersion: 0,
		PluginDeps: nil,
	}
	err := store.DeployWorkflowDef(testCtx, def)
	if err != nil {
		t.Fatalf("DeployWorkflowDef (nil deps): %v", err)
	}
}

func TestPostgresStore_MarkVersionDeprecated(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.MarkVersionDeprecated(testCtx, "wf", 1, true)
	if err != nil {
		t.Fatalf("MarkVersionDeprecated: %v", err)
	}
}

func TestPostgresStore_PurgeWorkflowDef(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.PurgeWorkflowDef(testCtx, "wf", 1)
	if err != nil {
		t.Fatalf("PurgeWorkflowDef: %v", err)
	}
}

func TestPostgresStore_UpdateStickyWorker(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.UpdateStickyWorker(testCtx, "wf-1", "worker-1")
	if err != nil {
		t.Fatalf("UpdateStickyWorker: %v", err)
	}
}

func TestPostgresStore_ClearStickyWorker(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ClearStickyWorker(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("ClearStickyWorker: %v", err)
	}
}

func TestPostgresStore_ReleaseConcurrencyKey(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ReleaseConcurrencyKey(testCtx, "my-key")
	if err != nil {
		t.Fatalf("ReleaseConcurrencyKey: %v", err)
	}
}

func TestPostgresStore_ReleaseWorkflowConcurrencyKeys(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ReleaseWorkflowConcurrencyKeys(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("ReleaseWorkflowConcurrencyKeys: %v", err)
	}
}

func TestPostgresStore_CreateUpdateRequest(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CreateUpdateRequest(testCtx, "wf-1", "update-name", "{}", "promise-1")
	if err != nil {
		t.Fatalf("CreateUpdateRequest: %v", err)
	}
}

func TestPostgresStore_CompleteUpdateRequest(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompleteUpdateRequest(testCtx, "wf-1", "update-name", "ok", "")
	if err != nil {
		t.Fatalf("CompleteUpdateRequest: %v", err)
	}
}

func TestPostgresStore_CreatePromise(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CreatePromise(testCtx, "wf-1", "my-promise", "promise-uuid")
	if err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}
}

func TestPostgresStore_ResolvePromise(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ResolvePromise(testCtx, "wf-1", "promise-uuid", `{"ok":true}`)
	if err != nil {
		t.Fatalf("ResolvePromise: %v", err)
	}
}

func TestPostgresStore_RejectPromise(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RejectPromise(testCtx, "wf-1", "promise-uuid", "something went wrong")
	if err != nil {
		t.Fatalf("RejectPromise: %v", err)
	}
}

// ---- Schedule methods ----

func TestPostgresStore_CreateSchedule(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	sch := Schedule{
		Name:           "daily-backup",
		DefName:        "backup-workflow",
		EntryPoint:     "main",
		CronExpression: "0 2 * * *",
		Input:          json.RawMessage(`{}`),
		Enabled:        true,
		NextRunAt:      time.Date(2025, 1, 1, 2, 0, 0, 0, time.UTC),
	}
	err := store.CreateSchedule(testCtx, sch)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
}

func TestPostgresStore_DeleteSchedule(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.DeleteSchedule(testCtx, "daily-backup")
	if err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
}

func TestPostgresStore_SetScheduleEnabled(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.SetScheduleEnabled(testCtx, "daily-backup", false)
	if err != nil {
		t.Fatalf("SetScheduleEnabled: %v", err)
	}
}

func TestPostgresStore_UpdateScheduleNextRun(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.UpdateScheduleNextRun(testCtx, "daily-backup", time.Date(2025, 1, 2, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("UpdateScheduleNextRun: %v", err)
	}
}

// ---- DeliverSignal ----

func TestPostgresStore_DeliverSignal(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.DeliverSignal(testCtx, "wf-1", "my-signal", `{"data":"hello"}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}
}

// ---- ReleaseWorkflow ----

func TestPostgresStore_ReleaseWorkflow(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ReleaseWorkflow(testCtx, "wf-1", "worker-1", 0, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ReleaseWorkflow: %v", err)
	}
}

// ---- RequestCancellation ----

func TestPostgresStore_RequestCancellation(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RequestCancellation(testCtx, "wf-1", "user requested")
	if err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}
}

// ---- RecordWorkflowMemorySample ----

func TestPostgresStore_RecordWorkflowMemorySample(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RecordWorkflowMemorySample(testCtx, "wf-def", 4096)
	if err != nil {
		t.Fatalf("RecordWorkflowMemorySample: %v", err)
	}
}

// ---- CompactHistory ----

func TestPostgresStore_CompactHistory(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompactHistory(testCtx, "wf-1", []byte(`{"version":1}`), 100, 50)
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Methods needing RowsAffected
// ---------------------------------------------------------------------------

func TestPostgresStore_Heartbeat_NotOwned(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances", affected: 0},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	owned, err := store.Heartbeat(testCtx, "wf-1", "worker-1", 0)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if owned {
		t.Error("expected owned=false when RowsAffected=0")
	}
}

func TestPostgresStore_Heartbeat_Owned(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	owned, err := store.Heartbeat(testCtx, "wf-1", "worker-1", 0)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !owned {
		t.Error("expected owned=true when RowsAffected=1")
	}
}

func TestPostgresStore_ReapStaleInstances(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances", affected: 3},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	n, err := store.ReapStaleInstances(testCtx, 30*time.Second)
	if err != nil {
		t.Fatalf("ReapStaleInstances: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3, got %d", n)
	}
}

func TestPostgresStore_ReapExpiredConcurrencyKeys(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "DELETE FROM concurrency_keys", affected: 5},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	n, err := store.ReapExpiredConcurrencyKeys(testCtx)
	if err != nil {
		t.Fatalf("ReapExpiredConcurrencyKeys: %v", err)
	}
	if n != 5 {
		t.Errorf("expected 5, got %d", n)
	}
}

func TestPostgresStore_ReapExpiredConcurrencyKeys_Zero(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "DELETE FROM concurrency_keys", affected: 0},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	n, err := store.ReapExpiredConcurrencyKeys(testCtx)
	if err != nil {
		t.Fatalf("ReapExpiredConcurrencyKeys: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// QueryRow methods (single row returned)
// ---------------------------------------------------------------------------

func TestPostgresStore_LoadWASM_Success(t *testing.T) {
	expected := []byte("some-wasm-bytes")
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT wasm_bytes", data: [][]driver.Value{{expected}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wasm, err := store.LoadWASM(testCtx, "test-wf", 1)
	if err != nil {
		t.Fatalf("LoadWASM: %v", err)
	}
	if string(wasm) != string(expected) {
		t.Errorf("expected %q, got %q", expected, wasm)
	}
}

func TestPostgresStore_LoadWASM_NotFound(t *testing.T) {
	// With no mock rows, QueryRow returns ErrNoRows.
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.LoadWASM(testCtx, "test-wf", 1)
	if err == nil {
		t.Fatal("expected error for not-found WASM")
	}
	if !strings.Contains(err.Error(), "wasm not found") {
		t.Errorf("expected 'wasm not found' error, got: %v", err)
	}
}

func TestPostgresStore_LoadWorkflowConfig_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT max_history_length", data: [][]driver.Value{{int64(500)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	maxHist, err := store.LoadWorkflowConfig(testCtx, "test-wf", 1)
	if err != nil {
		t.Fatalf("LoadWorkflowConfig: %v", err)
	}
	if maxHist != 500 {
		t.Errorf("expected 500, got %d", maxHist)
	}
}

func TestPostgresStore_LoadWorkflowConfig_NotFound(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.LoadWorkflowConfig(testCtx, "test-wf", 1)
	if err == nil {
		t.Fatal("expected error for not-found workflow def")
	}
	if !strings.Contains(err.Error(), "workflow def not found") {
		t.Errorf("expected 'workflow def not found' error, got: %v", err)
	}
}

func TestPostgresStore_LoadDAGSpec_Success(t *testing.T) {
	specJSON := json.RawMessage(`{"steps":["a","b"]}`)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT dag_spec", data: [][]driver.Value{{[]byte(specJSON)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	spec, err := store.LoadDAGSpec(testCtx, "test-wf", 1)
	if err != nil {
		t.Fatalf("LoadDAGSpec: %v", err)
	}
	if string(spec) != string(specJSON) {
		t.Errorf("expected %q, got %q", specJSON, spec)
	}
}

func TestPostgresStore_LoadDAGSpec_NotFound(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.LoadDAGSpec(testCtx, "test-wf", 1)
	if err == nil {
		t.Fatal("expected error for not-found DAG spec")
	}
	if !strings.Contains(err.Error(), "workflow def not found") {
		t.Errorf("expected 'workflow def not found' error, got: %v", err)
	}
}

func TestPostgresStore_CountActiveInstances(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT COUNT(*)", data: [][]driver.Value{{int64(7)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	count, err := store.CountActiveInstances(testCtx, "test-wf", 1)
	if err != nil {
		t.Fatalf("CountActiveInstances: %v", err)
	}
	if count != 7 {
		t.Errorf("expected 7, got %d", count)
	}
}

func TestPostgresStore_CountActiveInstances_Zero(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{{match: "SELECT COUNT(*)", data: [][]driver.Value{{int64(0)}}}}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	count, err := store.CountActiveInstances(testCtx, "test-wf", 1)
	if err != nil {
		t.Fatalf("CountActiveInstances: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestPostgresStore_QueueDepth(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT COUNT(*)", data: [][]driver.Value{{int64(42)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	depth, err := store.QueueDepth(testCtx)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 42 {
		t.Errorf("expected 42, got %d", depth)
	}
}

func TestPostgresStore_QueueDepth_Zero(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{{match: "SELECT COUNT(*)", data: [][]driver.Value{{int64(0)}}}}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	depth, err := store.QueueDepth(testCtx)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 0 {
		t.Errorf("expected 0, got %d", depth)
	}
}

func TestPostgresStore_ResolveLatestVersion(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT COALESCE", data: [][]driver.Value{{int64(3)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	version, err := store.ResolveLatestVersion(testCtx, "test-wf")
	if err != nil {
		t.Fatalf("ResolveLatestVersion: %v", err)
	}
	if version != 3 {
		t.Errorf("expected 3, got %d", version)
	}
}

func TestPostgresStore_ResolveLatestVersion_Zero(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{{match: "SELECT COALESCE", data: [][]driver.Value{{int64(0)}}}}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	version, _ := store.ResolveLatestVersion(testCtx, "test-wf")
	if version != 0 {
		t.Errorf("expected 0, got %d", version)
	}
}

func TestPostgresStore_ValidateVersion_True(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT EXISTS", data: [][]driver.Value{{true}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	valid, err := store.ValidateVersion(testCtx, "test-wf", 1)
	if err != nil {
		t.Fatalf("ValidateVersion: %v", err)
	}
	if !valid {
		t.Error("expected valid=true")
	}
}

func TestPostgresStore_ValidateVersion_False(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT EXISTS", data: [][]driver.Value{{false}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	valid, err := store.ValidateVersion(testCtx, "test-wf", 1)
	if err != nil {
		t.Fatalf("ValidateVersion: %v", err)
	}
	if valid {
		t.Error("expected valid=false")
	}
}

func TestPostgresStore_GetChildResult_NotFound(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	_, completed, err := store.GetChildResult(testCtx, "child-1")
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if completed {
		t.Error("expected completed=false for not found")
	}
}

func TestPostgresStore_GetChildResult_Done(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT COALESCE", data: [][]driver.Value{{`{"result":"ok"}`, "done"}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	result, completed, err := store.GetChildResult(testCtx, "child-1")
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if !completed {
		t.Error("expected completed=true")
	}
	if result != `{"result":"ok"}` {
		t.Errorf("expected result json, got %q", result)
	}
}

func TestPostgresStore_GetChildResult_StillRunning(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT COALESCE", data: [][]driver.Value{{"{}", "running"}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, completed, err := store.GetChildResult(testCtx, "child-1")
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if completed {
		t.Error("expected completed=false for running child")
	}
}

func TestPostgresStore_CheckCancellation_NotCancelled(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT cancellation_requested", data: [][]driver.Value{{false, nil}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	cancelled, reason, err := store.CheckCancellation(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("CheckCancellation: %v", err)
	}
	if cancelled {
		t.Error("expected cancelled=false")
	}
	if reason != "" {
		t.Errorf("expected empty reason, got %q", reason)
	}
}

func TestPostgresStore_GetQueryState_Found(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "query_state", data: [][]driver.Value{{"my-value"}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	val, err := store.GetQueryState(testCtx, "wf-1", "my-key")
	if err != nil {
		t.Fatalf("GetQueryState: %v", err)
	}
	if val != "my-value" {
		t.Errorf("expected 'my-value', got %q", val)
	}
}

func TestPostgresStore_GetQueryState_NotFound(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	val, err := store.GetQueryState(testCtx, "wf-1", "missing-key")
	if err != nil {
		t.Fatalf("GetQueryState: %v", err)
	}
	if val != "" {
		t.Errorf("expected empty string, got %q", val)
	}
}

func TestPostgresStore_GetWorkflowDef_Success(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT name, version, wasm_bytes",
			data: [][]driver.Value{{
				"test-wf",             // name
				int64(2),              // version
				[]byte("wasm-data"),   // wasm_bytes
				int64(1),              // abi_version
				int64(0),              // min_version
				[]byte(`{"p":"1.0"}`), // plugin_deps
				createdAt,             // created_at
				false,                 // deprecated
			}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	def, err := store.GetWorkflowDef(testCtx, "test-wf", 2)
	if err != nil {
		t.Fatalf("GetWorkflowDef: %v", err)
	}
	if def == nil {
		t.Fatal("expected non-nil def")
	}
	if def.Name != "test-wf" || def.Version != 2 {
		t.Errorf("unexpected name/version: %s v%d", def.Name, def.Version)
	}
	if string(def.WASMBytes) != "wasm-data" {
		t.Errorf("unexpected wasm: got %s", def.WASMBytes)
	}
	if def.PluginDeps["p"] != "1.0" {
		t.Errorf("unexpected plugin deps: %v", def.PluginDeps)
	}
}

func TestPostgresStore_GetWorkflowDef_NilPluginDeps(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT name, version, wasm_bytes",
			data: [][]driver.Value{{
				"test-wf",   // name
				int64(1),    // version
				[]byte(nil), // wasm_bytes
				int64(1),    // abi_version
				int64(0),    // min_version
				[]byte(nil), // plugin_deps (NULL)
				createdAt,   // created_at
				false,       // deprecated
			}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	def, err := store.GetWorkflowDef(testCtx, "test-wf", 1)
	if err != nil {
		t.Fatalf("GetWorkflowDef: %v", err)
	}
	if def == nil {
		t.Fatal("expected non-nil def")
	}
	if def.PluginDeps == nil {
		t.Error("expected non-nil PluginDeps map (should be initialized to empty)")
	}
}

func TestPostgresStore_GetWorkflowDef_NotFound(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	def, err := store.GetWorkflowDef(testCtx, "test-wf", 999)
	if err != nil {
		t.Fatalf("GetWorkflowDef: %v", err)
	}
	if def != nil {
		t.Error("expected nil def for not-found")
	}
}

func TestPostgresStore_GetWorkflowByID_Success(t *testing.T) {
	nextWakeAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	heartbeatAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	completedAt := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)

	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT id, def_name, def_version",
			data: [][]driver.Value{{
				"wf-1",                     // id
				"test-wf",                  // def_name
				int64(1),                   // def_version
				"done",                     // status
				[]byte(`{"input":"data"}`), // input
				"worker-1",                 // assigned_to
				heartbeatAt,                // heartbeat_at
				nextWakeAt,                 // next_wake_at
				completedAt,                // completed_at
				`{"result":"ok"}`,          // result::text
				"",                         // error_msg
				nil,                        // error_code
				nil,                        // error_op
				int64(0),                   // generation
				int64(0),                   // priority
				"",                         // trace_id
			}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wf, err := store.GetWorkflowByID(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf == nil {
		t.Fatal("expected non-nil workflow")
	}
	if wf.ID != "wf-1" || wf.Status != "done" || wf.AssignedTo != "worker-1" {
		t.Errorf("unexpected workflow fields: %+v", wf)
	}
}

func TestPostgresStore_GetWorkflowByID_NotFound(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	wf, err := store.GetWorkflowByID(testCtx, "nonexistent")
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf != nil {
		t.Error("expected nil for not-found")
	}
}

func TestPostgresStore_GetPromise_NotFound(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	status, result, errMsg, err := store.GetPromise(testCtx, "wf-1", "promise-1")
	if err != nil {
		t.Fatalf("GetPromise: %v", err)
	}
	if status != "pending" {
		t.Errorf("expected pending, got %q", status)
	}
	if result != "" || errMsg != "" {
		t.Errorf("expected empty result/errMsg, got %q / %q", result, errMsg)
	}
}

func TestPostgresStore_GetPromise_Resolved(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT status, result #>> '{}'",
			data:  [][]driver.Value{{"resolved", `{"ok":true}`, ""}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	status, result, errMsg, err := store.GetPromise(testCtx, "wf-1", "promise-1")
	if err != nil {
		t.Fatalf("GetPromise: %v", err)
	}
	if status != "resolved" || result != `{"ok":true}` || errMsg != "" {
		t.Errorf("unexpected: status=%q result=%q errMsg=%q", status, result, errMsg)
	}
}

func TestPostgresStore_LoadCompactionState_Empty(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT compaction_state", data: [][]driver.Value{{nil}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	cs, err := store.LoadCompactionState(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("LoadCompactionState: %v", err)
	}
	if cs != nil {
		t.Error("expected nil for empty compaction state")
	}
}

func TestPostgresStore_LoadCompactionState_NotFound(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	cs, err := store.LoadCompactionState(testCtx, "nonexistent")
	if err != nil {
		t.Fatalf("LoadCompactionState: %v", err)
	}
	if cs != nil {
		t.Error("expected nil for not-found")
	}
}

func TestPostgresStore_LoadCompactionState_Present(t *testing.T) {
	csJSON := []byte(`{"version":1,"compacted_step":50}`)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT compaction_state", data: [][]driver.Value{{csJSON}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	cs, err := store.LoadCompactionState(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("LoadCompactionState: %v", err)
	}
	if cs == nil {
		t.Fatal("expected non-nil compaction state")
	}
	if cs.Version != 1 || cs.CompactedStep != 50 {
		t.Errorf("unexpected state: %+v", cs)
	}
}

func TestPostgresStore_StartChildWorkflow(t *testing.T) {
	runID := "child-run-uuid"
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "INSERT INTO workflow_instances", data: [][]driver.Value{{runID}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	id, err := store.StartChildWorkflow(testCtx, "parent-1", "child-wf", `{"x":1}`, 0, "ABANDON", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}
	if id != runID {
		t.Errorf("expected %q, got %q", runID, id)
	}
}

func TestPostgresStore_StartChildWorkflow_ExplicitVersion(t *testing.T) {
	runID := "child-run-uuid"
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "INSERT INTO workflow_instances", data: [][]driver.Value{{runID}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	id, err := store.StartChildWorkflow(testCtx, "parent-1", "child-wf", `{"x":1}`, 3, "TERMINATE", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}
	if id != runID {
		t.Errorf("expected %q, got %q", runID, id)
	}
}

// ---------------------------------------------------------------------------
// Multi-row Query methods
// ---------------------------------------------------------------------------

func TestPostgresStore_ListVersions(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT version FROM workflow_defs",
			data: [][]driver.Value{
				{int64(3)},
				{int64(2)},
				{int64(1)},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	versions, err := store.ListVersions(testCtx, "test-wf")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(versions))
	}
	if versions[0] != 3 || versions[1] != 2 || versions[2] != 1 {
		t.Errorf("unexpected versions: %v", versions)
	}
}

func TestPostgresStore_ListVersions_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	versions, err := store.ListVersions(testCtx, "test-wf")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 0 {
		t.Errorf("expected 0 versions, got %d", len(versions))
	}
}

func TestPostgresStore_ListWorkflowDefs_All(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT name, version, abi_version",
			data: [][]driver.Value{
				{"wf-a", int64(2), int64(1), int64(0), []byte(`{}`), createdAt, false},
				{"wf-a", int64(1), int64(1), int64(0), []byte(`{"p":"1.0"}`), createdAt, true},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	defs, err := store.ListWorkflowDefs(testCtx, "")
	if err != nil {
		t.Fatalf("ListWorkflowDefs: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected 2 defs, got %d", len(defs))
	}
	if defs[0].Name != "wf-a" || defs[0].Version != 2 {
		t.Errorf("unexpected first def: %s v%d", defs[0].Name, defs[0].Version)
	}
	if defs[1].Deprecated != true {
		t.Error("expected second def to be deprecated")
	}
}

func TestPostgresStore_ListWorkflowDefs_ByName(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT name, version, abi_version",
			data: [][]driver.Value{
				{"wf-a", int64(1), int64(1), int64(0), []byte(`{}`), createdAt, false},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	defs, err := store.ListWorkflowDefs(testCtx, "wf-a")
	if err != nil {
		t.Fatalf("ListWorkflowDefs: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 def, got %d", len(defs))
	}
	if defs[0].Name != "wf-a" || defs[0].PluginDeps == nil {
		t.Errorf("unexpected def or nil deps: %+v", defs[0])
	}
}

func TestPostgresStore_ListWorkflowDefs_NilPluginDeps(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT name, version, abi_version",
			data: [][]driver.Value{
				{"wf-a", int64(1), int64(1), int64(0), []byte(nil), createdAt, false},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	defs, err := store.ListWorkflowDefs(testCtx, "wf-a")
	if err != nil {
		t.Fatalf("ListWorkflowDefs: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 def, got %d", len(defs))
	}
	if defs[0].PluginDeps == nil {
		t.Error("expected non-nil PluginDeps map")
	}
}

func TestPostgresStore_GetActiveInstanceCountsByVersion(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "FROM workflow_instances",
			data: [][]driver.Value{
				{"test-wf", int64(1), int64(5)},
				{"other-wf", int64(2), int64(3)},
				{"test-wf", int64(3), int64(1)},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	counts, err := store.GetActiveInstanceCountsByVersion(testCtx)
	if err != nil {
		t.Fatalf("GetActiveInstanceCountsByVersion: %v", err)
	}
	if counts["test-wf:1"] != 5 {
		t.Errorf("expected test-wf:1=5, got %d", counts["test-wf:1"])
	}
	if counts["other-wf:2"] != 3 {
		t.Errorf("expected other-wf:2=3, got %d", counts["other-wf:2"])
	}
	if counts["test-wf:3"] != 1 {
		t.Errorf("expected test-wf:3=1, got %d", counts["test-wf:3"])
	}
}

func TestPostgresStore_GetActiveInstanceCountsByVersion_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	counts, err := store.GetActiveInstanceCountsByVersion(testCtx)
	if err != nil {
		t.Fatalf("GetActiveInstanceCountsByVersion: %v", err)
	}
	if len(counts) != 0 {
		t.Errorf("expected empty map, got %d entries", len(counts))
	}
}

// ---------------------------------------------------------------------------
// Version Management — error path tests
// ---------------------------------------------------------------------------

func TestPostgresStore_ListVersions_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT version FROM workflow_defs",
			err:   fmt.Errorf("simulated query error"),
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ListVersions(testCtx, "test-wf")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "list versions") {
		t.Errorf("expected error to contain 'list versions', got: %v", err)
	}
}

func TestPostgresStore_DeployWorkflowDef_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{
			match: "INSERT INTO workflow_defs",
			err:   fmt.Errorf("simulated exec error"),
		},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	def := &WorkflowDef{Name: "wf", Version: 1, WASMBytes: []byte("wasm"), ABIVersion: 1}
	err := store.DeployWorkflowDef(testCtx, def)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "deploy workflow def") {
		t.Errorf("expected error to contain 'deploy workflow def', got: %v", err)
	}
}

func TestPostgresStore_ListWorkflowDefs_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT name, version, abi_version",
			err:   fmt.Errorf("simulated query error"),
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ListWorkflowDefs(testCtx, "wf")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "list workflow defs") {
		t.Errorf("expected error to contain 'list workflow defs', got: %v", err)
	}
}

func TestPostgresStore_GetWorkflowDef_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT name, version, wasm_bytes",
			err:   fmt.Errorf("simulated query error"),
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetWorkflowDef(testCtx, "wf", 1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "get workflow def") {
		t.Errorf("expected error to contain 'get workflow def', got: %v", err)
	}
}

func TestPostgresStore_MarkVersionDeprecated_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{
			match: "UPDATE workflow_defs SET deprecated",
			err:   fmt.Errorf("simulated exec error"),
		},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.MarkVersionDeprecated(testCtx, "wf", 1, true)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "mark version deprecated") {
		t.Errorf("expected error to contain 'mark version deprecated', got: %v", err)
	}
}

func TestPostgresStore_PurgeWorkflowDef_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{
			match: "DELETE FROM workflow_defs",
			err:   fmt.Errorf("simulated exec error"),
		},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.PurgeWorkflowDef(testCtx, "wf", 1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "purge workflow def") {
		t.Errorf("expected error to contain 'purge workflow def', got: %v", err)
	}
}

func TestPostgresStore_CountActiveInstances_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "FROM workflow_instances WHERE def_name",
			err:   fmt.Errorf("simulated query error"),
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.CountActiveInstances(testCtx, "wf", 1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "count active instances") {
		t.Errorf("expected error to contain 'count active instances', got: %v", err)
	}
}

func TestPostgresStore_GetActiveInstanceCountsByVersion_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "FROM workflow_instances",
			err:   fmt.Errorf("simulated query error"),
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetActiveInstanceCountsByVersion(testCtx)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "get active instance counts") {
		t.Errorf("expected error to contain 'get active instance counts', got: %v", err)
	}
}

func TestPostgresStore_ResolveLatestVersion_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT COALESCE",
			err:   fmt.Errorf("simulated query error"),
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ResolveLatestVersion(testCtx, "wf")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "resolve latest version") {
		t.Errorf("expected error to contain 'resolve latest version', got: %v", err)
	}
}

func TestPostgresStore_ValidateVersion_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT EXISTS",
			err:   fmt.Errorf("simulated query error"),
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ValidateVersion(testCtx, "wf", 1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "validate version") {
		t.Errorf("expected error to contain 'validate version', got: %v", err)
	}
}

func TestPostgresStore_ListWorkflows_WithStatus(t *testing.T) {
	nextWakeAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT id, def_name, def_version",
			data: [][]driver.Value{
				{"wf-1", "test-wf", int64(1), "running", []byte(`{"in":1}`), "worker-1", nextWakeAt, nil, nil, nil, nil, int64(0), int64(0), ""},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ListWorkflows(testCtx, WorkflowFilter{Status: "running", Limit: 10})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	if wfs[0].ID != "wf-1" || wfs[0].Status != "running" {
		t.Errorf("unexpected workflow: %+v", wfs[0])
	}
}

func TestPostgresStore_ListWorkflows_NoFilter(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT id, def_name, def_version",
			data: [][]driver.Value{
				{"wf-1", "test-wf", int64(1), "running", []byte(`{}`), "worker-1", time.Time{}, nil, nil, nil, nil, int64(0), int64(0), ""},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ListWorkflows(testCtx, WorkflowFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListWorkflows (no filter): %v", err)
	}
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
}

func TestPostgresStore_ListWorkflows_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ListWorkflows(testCtx, WorkflowFilter{Status: "running", Limit: 10})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 0 {
		t.Errorf("expected empty list, got %d", len(wfs))
	}
}

func TestPostgresStore_ListSchedules(t *testing.T) {
	nextRunAt := time.Date(2025, 1, 1, 2, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT name, def_name, entry_point",
			data: [][]driver.Value{
				{"sched-1", "wf-a", "main", "0 2 * * *", []byte(`{}`), true, nextRunAt, nextRunAt},
				{"sched-2", "wf-b", "handler", "*/5 * * * *", []byte(`{"x":1}`), false, nextRunAt, nil},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	scheds, err := store.ListSchedules(testCtx)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(scheds) != 2 {
		t.Fatalf("expected 2 schedules, got %d", len(scheds))
	}
	if scheds[0].Name != "sched-1" || !scheds[0].Enabled {
		t.Errorf("unexpected first schedule: %+v", scheds[0])
	}
	if scheds[1].Name != "sched-2" || scheds[1].LastRunAt != nil {
		t.Errorf("unexpected second schedule: %+v", scheds[1])
	}
}

func TestPostgresStore_GetDueSchedules(t *testing.T) {
	nextRunAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT name, def_name, entry_point",
			data: [][]driver.Value{
				{"due-sched", "wf-a", "main", "0 2 * * *", []byte(`{}`), true, nextRunAt, nil},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	scheds, err := store.GetDueSchedules(testCtx)
	if err != nil {
		t.Fatalf("GetDueSchedules: %v", err)
	}
	if len(scheds) != 1 {
		t.Fatalf("expected 1 schedule, got %d", len(scheds))
	}
	if scheds[0].Name != "due-sched" {
		t.Errorf("unexpected name: %q", scheds[0].Name)
	}
}

func TestPostgresStore_GetDueSchedules_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	scheds, err := store.GetDueSchedules(testCtx)
	if err != nil {
		t.Fatalf("GetDueSchedules: %v", err)
	}
	if len(scheds) != 0 {
		t.Errorf("expected empty, got %d", len(scheds))
	}
}

func TestPostgresStore_ListPromises(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	resolvedAt := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT promise_id, promise_name",
			data: [][]driver.Value{
				{"p-1", "promise-a", "resolved", "ok", "", createdAt, resolvedAt},
				{"p-2", "promise-b", "pending", "", "", createdAt, nil},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	promises, err := store.ListPromises(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("ListPromises: %v", err)
	}
	if len(promises) != 2 {
		t.Fatalf("expected 2 promises, got %d", len(promises))
	}
	if promises[0].PromiseID != "p-1" || promises[0].Status != "resolved" {
		t.Errorf("unexpected first promise: %+v", promises[0])
	}
	if promises[1].PromiseID != "p-2" || promises[1].Status != "pending" {
		t.Errorf("unexpected second promise: %+v", promises[1])
	}
	if promises[1].ResolvedAt != nil {
		t.Error("expected nil ResolvedAt for pending promise")
	}
}

func TestPostgresStore_GetPendingUpdateRequests(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT workflow_id, update_name",
			data: [][]driver.Value{
				{"wf-1", "update-a", `{}`, "prom-1", "pending", "", "", createdAt},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	reqs, err := store.GetPendingUpdateRequests(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetPendingUpdateRequests: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	if reqs[0].UpdateName != "update-a" || reqs[0].Status != "pending" {
		t.Errorf("unexpected request: %+v", reqs[0])
	}
}

func TestPostgresStore_GetCompactionCandidates(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT w.id",
			data: [][]driver.Value{
				{"wf-1"},
				{"wf-2"},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	candidates, err := store.GetCompactionCandidates(testCtx, 100, 10)
	if err != nil {
		t.Fatalf("GetCompactionCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates[0] != "wf-1" || candidates[1] != "wf-2" {
		t.Errorf("unexpected candidates: %v", candidates)
	}
}

func TestPostgresStore_LoadMemoryEstimates(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT def_name, mean_bytes",
			data: [][]driver.Value{
				{"wf-a", float64(4096.5)},
				{"wf-b", float64(8192.0)},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	estimates, err := store.LoadMemoryEstimates(testCtx)
	if err != nil {
		t.Fatalf("LoadMemoryEstimates: %v", err)
	}
	if len(estimates) != 2 {
		t.Fatalf("expected 2 estimates, got %d", len(estimates))
	}
	if estimates["wf-a"] != 4096.5 || estimates["wf-b"] != 8192.0 {
		t.Errorf("unexpected estimates: %v", estimates)
	}
}

func TestPostgresStore_LoadMemoryEstimates_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	estimates, err := store.LoadMemoryEstimates(testCtx)
	if err != nil {
		t.Fatalf("LoadMemoryEstimates: %v", err)
	}
	if len(estimates) != 0 {
		t.Errorf("expected empty map, got %v", estimates)
	}
}

func TestPostgresStore_LoadMemoryStats(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT def_name",
			data: [][]driver.Value{
				{"wf-a", int64(100), float64(250.5), int64(500), int64(120), int64(180), int64(240), int64(350), int64(450), int64(490), int64(10)},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	stats, err := store.LoadMemoryStats(testCtx)
	if err != nil {
		t.Fatalf("LoadMemoryStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 stat, got %d", len(stats))
	}
	if stats[0].DefName != "wf-a" || stats[0].MinBytes != 100 || stats[0].AvgBytes != 250.5 {
		t.Errorf("unexpected stats: %+v", stats[0])
	}
	if stats[0].P10 != 120 || stats[0].P50 != 240 || stats[0].P99 != 490 {
		t.Errorf("unexpected percentiles: %+v", stats[0])
	}
}

func TestPostgresStore_LoadMemoryStats_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	stats, err := store.LoadMemoryStats(testCtx)
	if err != nil {
		t.Fatalf("LoadMemoryStats: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected empty, got %d", len(stats))
	}
}

// ---------------------------------------------------------------------------
// ClaimWorkflows (complex UPDATE ... RETURNING)
// ---------------------------------------------------------------------------

func TestPostgresStore_ClaimWorkflows_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ClaimWorkflows(testCtx, "worker-1", 5)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	if len(wfs) != 0 {
		t.Errorf("expected 0 workflows, got %d", len(wfs))
	}
}

func TestPostgresStore_ClaimWorkflows_Success(t *testing.T) {
	nextWakeAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "UPDATE workflow_instances",
			data: [][]driver.Value{
				{"wf-1", "test-wf", int64(1), "running", []byte(`{"input":"data"}`), "worker-1", nextWakeAt, "tenant-1", createdAt, nil, nil, int64(0), int64(0), ""},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ClaimWorkflows(testCtx, "worker-1", 5)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	if wfs[0].ID != "wf-1" || wfs[0].Status != "running" || wfs[0].TenantID != "tenant-1" {
		t.Errorf("unexpected workflow: %+v", wfs[0])
	}
}

func TestPostgresStore_ClaimWorkflows_NoTenantID(t *testing.T) {
	nextWakeAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "UPDATE workflow_instances",
			data: [][]driver.Value{
				{"wf-1", "test-wf", int64(1), "running", []byte(`{}`), "worker-1", nextWakeAt, nil, nil, nil, nil, int64(0), int64(0), ""},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ClaimWorkflows(testCtx, "worker-1", 5)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	if wfs[0].TenantID != "" {
		t.Errorf("expected empty tenant_id, got %q", wfs[0].TenantID)
	}
}

func TestPostgresStore_ClaimStickyWorkflows_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ClaimStickyWorkflows(testCtx, "worker-1", 5)
	if err != nil {
		t.Fatalf("ClaimStickyWorkflows: %v", err)
	}
	if len(wfs) != 0 {
		t.Errorf("expected 0 workflows, got %d", len(wfs))
	}
}

func TestPostgresStore_ClaimStickyWorkflows_Success(t *testing.T) {
	nextWakeAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "UPDATE workflow_instances",
			data: [][]driver.Value{
				{"stickywf-1", "test-wf", int64(1), "running", []byte(`{}`), "worker-1", nextWakeAt, "tenant-1", nil, nil, nil, int64(0), int64(0), ""},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ClaimStickyWorkflows(testCtx, "worker-1", 5)
	if err != nil {
		t.Fatalf("ClaimStickyWorkflows: %v", err)
	}
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	if wfs[0].ID != "stickywf-1" {
		t.Errorf("unexpected workflow: %+v", wfs[0])
	}
}

// ---------------------------------------------------------------------------
// ClaimWorkflow wrapper (implements WorkflowStore.ClaimWorkflow)
// ---------------------------------------------------------------------------

func TestPostgresStore_ClaimWorkflow_NilWhenEmpty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	wf, err := store.ClaimWorkflow(testCtx, "worker-1")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if wf != nil {
		t.Error("expected nil when no workflows available")
	}
}

func TestPostgresStore_ClaimWorkflow_ReturnsFirst(t *testing.T) {
	nextWakeAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "UPDATE workflow_instances",
			data: [][]driver.Value{
				{"wf-1", "test-wf", int64(1), "running", []byte(`{}`), "worker-1", nextWakeAt, nil, nil, nil, nil, int64(0), int64(0), ""},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wf, err := store.ClaimWorkflow(testCtx, "worker-1")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if wf == nil {
		t.Fatal("expected non-nil workflow")
	}
	if wf.ID != "wf-1" {
		t.Errorf("expected wf-1, got %s", wf.ID)
	}
}

// ---------------------------------------------------------------------------
// LoadEventHistory
// ---------------------------------------------------------------------------

func TestPostgresStore_LoadEventHistory_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	history, err := store.LoadEventHistory(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("expected empty, got %d", len(history))
	}
}

func TestPostgresStore_LoadEventHistory_WithEvents(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT step, event_type, service",
			data: [][]driver.Value{
				{
					int64(0),         // step
					"call",           // event_type
					"my-service",     // service (for payload population)
					"my-op",          // operation
					`{"req":"data"}`, // request
					`{"resp":"ok"}`,  // response
					"",               // error
					int64(150),       // duration_ms
					"",               // signal_names
					int64(0),         // timeout_ms
					"",               // signal_name
					"",               // signal_payload
					"",               // defer_description
					"",               // defer_id
					"",               // child_name
					"",               // child_input
					"",               // run_id
					"",               // new_input
					"",               // plugin_name
					"",               // plugin_func
					"",               // plugin_input
					"",               // plugin_output
					"",               // plugin_error
					[]byte(nil),      // payload (nil = no payload)
					"",               // promise_name
					"",               // promise_id
					"",               // promise_result
					"",               // promise_error
					int64(0),         // timestamp_ms
					nil,              // created_at
				},
				{
					int64(1), // step
					"sleep",  // event_type
					"", "", "", "", "",
					int64(5000), // duration_ms
					"", int64(0),
					"", "",
					"", "",
					"", "", "", "",
					"", "", "", "", "",
					[]byte(`{"duration_ms":5000}`), // payload
					"", "", "", "",
					int64(0), // timestamp_ms
					nil,      // created_at
				},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	history, err := store.LoadEventHistory(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 events, got %d", len(history))
	}
	if history[0].Step != 0 || history[0].EventType != "call" || history[0].Service != "my-service" {
		t.Errorf("unexpected first event: %+v", history[0])
	}
	if history[1].Step != 1 || history[1].EventType != "sleep" || history[1].DurationMs != 5000 {
		t.Errorf("unexpected second event: %+v", history[1])
	}
}

// ---------------------------------------------------------------------------
// AppendEventHistoryBatch
// ---------------------------------------------------------------------------

func TestPostgresStore_AppendEventHistoryBatch(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	recs := []EventRecord{
		{
			Step:      0,
			EventType: "call",
			Service:   "svc",
			Op:        "op",
			Request:   `{}`,
			Response:  `{}`,
		},
		{
			Step:       1,
			EventType:  "sleep",
			DurationMs: 3000,
		},
	}
	err := store.AppendEventHistoryBatch(testCtx, "wf-1", recs)
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}
}

func TestPostgresStore_AppendEventHistoryBatch_PluginCall(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	recs := []EventRecord{
		{
			Step:        0,
			EventType:   "plugin_call",
			PluginName:  "my-plugin",
			PluginFunc:  "do-thing",
			PluginInput: `{}`,
		},
	}
	err := store.AppendEventHistoryBatch(testCtx, "wf-1", recs)
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch (plugin): %v", err)
	}
}

func TestPostgresStore_AppendEventHistoryBatch_Promise(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	recs := []EventRecord{
		{
			Step:        0,
			EventType:   "create_promise",
			PromiseName: "my-promise",
			PromiseID:   "prom-uuid",
		},
	}
	err := store.AppendEventHistoryBatch(testCtx, "wf-1", recs)
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch (promise): %v", err)
	}
}

// ---------------------------------------------------------------------------
// AppendEventHistory (single event wrapper)
// ---------------------------------------------------------------------------

func TestPostgresStore_AppendEventHistory(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	rec := EventRecord{
		Step:      0,
		EventType: "call",
		Service:   "svc",
		Op:        "op",
	}
	err := store.AppendEventHistory(testCtx, "wf-1", rec)
	if err != nil {
		t.Fatalf("AppendEventHistory: %v", err)
	}
}

// ---------------------------------------------------------------------------
// StartNewRun
// ---------------------------------------------------------------------------

func TestPostgresStore_StartNewRun_NoIdempotencyKey(t *testing.T) {
	runID := "new-run-uuid"
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "INSERT INTO workflow_instances (id, def_name, def_version",
			data:  [][]driver.Value{{runID}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	id, alreadyExisted, err := store.StartNewRun(testCtx, "", "test-wf", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty run ID")
	}
	if alreadyExisted {
		t.Error("expected alreadyExisted=false")
	}
}

func TestPostgresStore_StartNewRun_WithIdempotencyKey_NewRun(t *testing.T) {
	runID := "new-run-id"
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "INSERT INTO workflow_instances",
			data:  [][]driver.Value{{runID}},
		},
	}, []mockExecResult{
		{match: "INSERT INTO idempotency_keys", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	id, alreadyExisted, err := store.StartNewRun(testCtx, "", "test-wf", 1, json.RawMessage(`{}`), "idem-key-123", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty run ID")
	}
	if alreadyExisted {
		t.Error("expected alreadyExisted=false")
	}
}

func TestPostgresStore_StartNewRun_WithIdempotencyKey_AlreadyExists(t *testing.T) {
	existingID := "existing-run-id"
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT workflow_id FROM idempotency_keys",
			data:  [][]driver.Value{{existingID}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	id, alreadyExisted, err := store.StartNewRun(testCtx, "", "test-wf", 1, json.RawMessage(`{}`), "idem-key-123", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	if id != existingID {
		t.Errorf("expected %q, got %q", existingID, id)
	}
	if !alreadyExisted {
		t.Error("expected alreadyExisted=true")
	}
}

func TestPostgresStore_StartNewRun_WithIdempotencyKey_Collision(t *testing.T) {
	// Tests the ON CONFLICT DO NOTHING path: first SELECT finds no active
	// key, INSERT returns RowsAffected=0 (another request inserted
	// simultaneously), then second SELECT returns the existing workflow ID.
	collidedID := "collided-wf-id"
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			// First SELECT: no active key found (expired or missing).
			match:   "SELECT workflow_id FROM idempotency_keys",
			data:    nil,
			consume: true,
		},
		{
			// Second SELECT after collision: return the concurrently-inserted key.
			match: "SELECT workflow_id FROM idempotency_keys",
			data:  [][]driver.Value{{collidedID}},
		},
	}, []mockExecResult{
		// INSERT ON CONFLICT DO NOTHING: RowsAffected=0 means collision.
		{match: "INSERT INTO idempotency_keys", affected: 0},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	id, alreadyExisted, err := store.StartNewRun(testCtx, "", "test-wf", 1, json.RawMessage(`{}`), "idem-key-456", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (collision): %v", err)
	}
	if id != collidedID {
		t.Errorf("expected %q, got %q", collidedID, id)
	}
	if !alreadyExisted {
		t.Error("expected alreadyExisted=true after collision")
	}
}

func TestPostgresStore_StartNewRun_WithIdempotencyKey_InsertError(t *testing.T) {
	// Tests the path where INSERT INTO idempotency_keys fails with an error.
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO idempotency_keys", err: sql.ErrConnDone},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	_, _, err := store.StartNewRun(testCtx, "", "test-wf", 1, json.RawMessage(`{}`), "idem-key-789", DefaultTenantUUID, 0)
	if err == nil {
		t.Fatal("expected error from INSERT idempotency_keys failure, got nil")
	}
}

// ---------------------------------------------------------------------------
// PollAndClaimSignal
// ---------------------------------------------------------------------------

func TestPostgresStore_PollAndClaimSignal_NotFound(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	_, found, err := store.PollAndClaimSignal(testCtx, "wf-1", "my-signal")
	if err != nil {
		t.Fatalf("PollAndClaimSignal: %v", err)
	}
	if found {
		t.Error("expected found=false")
	}
}

func TestPostgresStore_PollAndClaimSignal_Found(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "DELETE FROM workflow_signals",
			data:  [][]driver.Value{{`{"signal":"data"}`}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	payload, found, err := store.PollAndClaimSignal(testCtx, "wf-1", "my-signal")
	if err != nil {
		t.Fatalf("PollAndClaimSignal: %v", err)
	}
	if !found {
		t.Error("expected found=true")
	}
	if payload != `{"signal":"data"}` {
		t.Errorf("unexpected payload: %q", payload)
	}
}

// ---------------------------------------------------------------------------
// CompleteWorkflow and FailWorkflow (complex, with best-effort cleanup)
// ---------------------------------------------------------------------------

func TestPostgresStore_CompleteWorkflow(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompleteWorkflow(testCtx, "wf-1", "worker-1", 0, `{"result":"ok"}`, map[string]string{"key": "val"})
	if err != nil {
		t.Fatalf("CompleteWorkflow: %v", err)
	}
}

func TestPostgresStore_CompleteWorkflow_NilQueryState(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompleteWorkflow(testCtx, "wf-1", "worker-1", 0, `{}`, nil)
	if err != nil {
		t.Fatalf("CompleteWorkflow (nil qs): %v", err)
	}
}

func TestPostgresStore_CompleteWorkflow_EmptyQueryState(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompleteWorkflow(testCtx, "wf-1", "worker-1", 0, `{}`, map[string]string{})
	if err != nil {
		t.Fatalf("CompleteWorkflow (empty qs): %v", err)
	}
}

func TestPostgresStore_FailWorkflow(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.FailWorkflow(testCtx, "wf-1", "worker-1", 0, "something broke", "", "", map[string]string{"key": "val"})
	if err != nil {
		t.Fatalf("FailWorkflow: %v", err)
	}
}

func TestPostgresStore_FailWorkflow_NilQueryState(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.FailWorkflow(testCtx, "wf-1", "worker-1", 0, "error", "", "", nil)
	if err != nil {
		t.Fatalf("FailWorkflow (nil qs): %v", err)
	}
}

func TestPostgresStore_CompleteWorkflow_IdempotencyUpdateFails(t *testing.T) {
	// Idempotency UPDATE is best-effort. When it fails, the error is logged
	// but CompleteWorkflow still succeeds.
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		// Main workflow status update succeeds.
		{match: "UPDATE workflow_instances SET status = 'done'", affected: 1},
		// Idempotency update fails — logged but non-fatal.
		{match: "UPDATE idempotency_keys SET result =", err: sql.ErrConnDone},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompleteWorkflow(testCtx, "wf-1", "worker-1", 0, `{"result":"ok"}`, map[string]string{"key": "val"})
	if err != nil {
		t.Fatalf("CompleteWorkflow should succeed even when idempotency update fails: %v", err)
	}
}

func TestPostgresStore_FailWorkflow_IdempotencyUpdateFails(t *testing.T) {
	// Idempotency error UPDATE is best-effort. When it fails, the error is
	// logged but FailWorkflow still succeeds.
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		// Main workflow status update succeeds.
		{match: "UPDATE workflow_instances SET status = 'failed'", affected: 1},
		// Idempotency update fails — logged but non-fatal.
		{match: "UPDATE idempotency_keys SET error_msg =", err: sql.ErrConnDone},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.FailWorkflow(testCtx, "wf-1", "worker-1", 0, "something broke", "", "", map[string]string{"key": "val"})
	if err != nil {
		t.Fatalf("FailWorkflow should succeed even when idempotency update fails: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AcquireConcurrencyKey
// ---------------------------------------------------------------------------

func TestPostgresStore_AcquireConcurrencyKey_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "INSERT INTO concurrency_keys",
			data:  [][]driver.Value{{"wf-1"}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	acquired, err := store.AcquireConcurrencyKey(testCtx, "my-key", "wf-1", 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Error("expected acquired=true")
	}
}

func TestPostgresStore_AcquireConcurrencyKey_AlreadyHeld(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	acquired, err := store.AcquireConcurrencyKey(testCtx, "my-key", "wf-2", 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if acquired {
		t.Error("expected acquired=false when key already held")
	}
}

// ---------------------------------------------------------------------------
// CleanupMemorySamples
// ---------------------------------------------------------------------------

func TestPostgresStore_CleanupMemorySamples_NoDefs(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	n, err := store.CleanupMemorySamples(testCtx, 100)
	if err != nil {
		t.Fatalf("CleanupMemorySamples: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestPostgresStore_CleanupMemorySamples_WithDefs(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT DISTINCT def_name",
			data:  [][]driver.Value{{"wf-a"}, {"wf-b"}},
		},
	}, []mockExecResult{
		{match: "DELETE FROM workflow_memory_samples", affected: 3},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	n, err := store.CleanupMemorySamples(testCtx, 100)
	if err != nil {
		t.Fatalf("CleanupMemorySamples: %v", err)
	}
	if n != 6 {
		t.Errorf("expected 6 (3 per def * 2 defs), got %d", n)
	}
}

// ---------------------------------------------------------------------------
// PollSignal / PollCancellation wrappers
// ---------------------------------------------------------------------------

func TestPostgresStore_PollSignal(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "DELETE FROM workflow_signals",
			data:  [][]driver.Value{{`{"polled":true}`}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	payload, found, err := store.PollSignal(testCtx, "wf-1", "my-signal")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if !found || payload != `{"polled":true}` {
		t.Errorf("unexpected: found=%v, payload=%q", found, payload)
	}
}

func TestPostgresStore_PollCancellation(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT cancellation_requested",
			data:  [][]driver.Value{{true, "cancel-reason"}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	cancelled, reason, err := store.PollCancellation(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("PollCancellation: %v", err)
	}
	if !cancelled || reason != "cancel-reason" {
		t.Errorf("unexpected: cancelled=%v, reason=%q", cancelled, reason)
	}
}

// ---------------------------------------------------------------------------
// GetWorkflowByID with optional fields as NULL
// ---------------------------------------------------------------------------

func TestPostgresStore_GetWorkflowByID_NullOptionals(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT id, def_name, def_version",
			data: [][]driver.Value{{
				"wf-1",       // id
				"test-wf",    // def_name
				int64(1),     // def_version
				"running",    // status
				[]byte(`{}`), // input
				nil,          // assigned_to (NULL)
				nil,          // heartbeat_at (NULL)
				nil,          // next_wake_at (NULL)
				nil,          // completed_at (NULL)
				nil,          // result::text (NULL)
				nil,          // error_msg (NULL)
				nil,          // error_code (NULL)
				nil,          // error_op (NULL)
				int64(0),     // generation
				int64(0),     // priority
				"",           // trace_id (COALESCE)
			}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wf, err := store.GetWorkflowByID(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf == nil {
		t.Fatal("expected non-nil")
	}
	if wf.AssignedTo != "" {
		t.Errorf("expected empty assigned_to, got %q", wf.AssignedTo)
	}
}

// ---------------------------------------------------------------------------
// GetConcurrencyKeyCount
// ---------------------------------------------------------------------------

func TestPostgresStore_GetConcurrencyKeyCount_NonZero(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT COUNT(*) FROM concurrency_keys",
			data:  [][]driver.Value{{int64(3)}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	count, err := store.GetConcurrencyKeyCount(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetConcurrencyKeyCount: %v", err)
	}
	if count != 3 {
		t.Errorf("expected count=3, got %d", count)
	}
}

func TestPostgresStore_GetConcurrencyKeyCount_Zero(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT COUNT(*) FROM concurrency_keys",
			data:  [][]driver.Value{{int64(0)}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	count, err := store.GetConcurrencyKeyCount(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetConcurrencyKeyCount: %v", err)
	}
	if count != 0 {
		t.Errorf("expected count=0, got %d", count)
	}
}

func TestPostgresStore_GetConcurrencyKeyCount_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT COUNT(*) FROM concurrency_keys",
			err:   errors.New("count query failed"),
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetConcurrencyKeyCount(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected error from query failure")
	}
}

// ---------------------------------------------------------------------------
// AcquireConcurrencyKey error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_AcquireConcurrencyKey_DeleteExpiredError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{
			match: "DELETE FROM concurrency_keys",
			err:   errors.New("delete expired failed"),
		},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.AcquireConcurrencyKey(testCtx, "my-key", "wf-1", 30*time.Second)
	if err == nil {
		t.Fatal("expected error from delete expired failure")
	}
}

func TestPostgresStore_AcquireConcurrencyKey_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "INSERT INTO concurrency_keys",
			err:   errors.New("insert query failed"),
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.AcquireConcurrencyKey(testCtx, "my-key", "wf-1", 30*time.Second)
	if err == nil {
		t.Fatal("expected error from insert query failure")
	}
}

func TestPostgresStore_AcquireConcurrencyKey_CommitError(t *testing.T) {
	db := newMockDBWithErrors(t, []mockRowsResult{
		{
			match: "INSERT INTO concurrency_keys",
			data:  [][]driver.Value{{"wf-1"}},
		},
	}, nil, nil, errors.New("commit failed"))
	defer db.Close()

	store := NewPostgresStore(db)
	acquired, err := store.AcquireConcurrencyKey(testCtx, "my-key", "wf-1", 30*time.Second)
	if err == nil {
		t.Fatal("expected error from commit failure")
	}
	if !acquired {
		t.Error("expected acquired=true before commit failure")
	}
}

// ---------------------------------------------------------------------------
// ReleaseConcurrencyKey error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_ReleaseConcurrencyKey_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{
			match: "DELETE FROM concurrency_keys",
			err:   errors.New("delete concurrency key failed"),
		},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ReleaseConcurrencyKey(testCtx, "my-key")
	if err == nil {
		t.Fatal("expected error from exec failure")
	}
}

func TestPostgresStore_ReleaseConcurrencyKey_CommitError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, nil, errors.New("commit failed"))
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ReleaseConcurrencyKey(testCtx, "my-key")
	if err == nil {
		t.Fatal("expected error from commit failure")
	}
}

// ---------------------------------------------------------------------------
// ReleaseWorkflowConcurrencyKeys error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_ReleaseWorkflowConcurrencyKeys_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{
			match: "DELETE FROM concurrency_keys",
			err:   errors.New("delete workflow keys failed"),
		},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ReleaseWorkflowConcurrencyKeys(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected error from exec failure")
	}
}

func TestPostgresStore_ReleaseWorkflowConcurrencyKeys_CommitError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, nil, errors.New("commit failed"))
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ReleaseWorkflowConcurrencyKeys(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected error from commit failure")
	}
}

// ---------------------------------------------------------------------------
// ReapExpiredConcurrencyKeys error path
// ---------------------------------------------------------------------------

func TestPostgresStore_ReapExpiredConcurrencyKeys_CommitError(t *testing.T) {
	db := newMockDBWithErrors(t, []mockRowsResult{}, []mockExecResult{
		{match: "DELETE FROM concurrency_keys", affected: 5},
	}, nil, errors.New("commit failed"))
	defer db.Close()

	store := NewPostgresStore(db)
	n, err := store.ReapExpiredConcurrencyKeys(testCtx)
	if err == nil {
		t.Fatal("expected error from commit failure")
	}
	if n != 5 {
		t.Errorf("expected n=5 from RowsAffected, got %d", n)
	}
}

func TestPostgresStore_ReapExpiredConcurrencyKeys_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{
			match: "DELETE FROM concurrency_keys",
			err:   errors.New("delete expired keys failed"),
		},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ReapExpiredConcurrencyKeys(testCtx)
	if err == nil {
		t.Fatal("expected error from exec failure")
	}
}

// ---------------------------------------------------------------------------
// ReleaseWorkflow error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_ReleaseWorkflow_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{
			match: "UPDATE workflow_instances",
			err:   errors.New("update workflow failed"),
		},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ReleaseWorkflow(testCtx, "wf-1", "worker-1", 0, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected error from exec failure")
	}
}

func TestPostgresStore_ReleaseWorkflow_CommitError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, nil, errors.New("commit failed"))
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ReleaseWorkflow(testCtx, "wf-1", "worker-1", 0, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected error from commit failure")
	}
}

// ---------------------------------------------------------------------------
// CompactHistory — edge cases and error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_CompactHistory_WorkflowGone(t *testing.T) {
	// When the workflow no longer exists, CompactHistory should commit
	// and return nil (it is a no-op).
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT generation", err: sql.ErrNoRows},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompactHistory(testCtx, "nonexistent", []byte(`{"version":1}`), 100, 50)
	if err != nil {
		t.Fatalf("CompactHistory (workflow gone) should succeed: %v", err)
	}
}

func TestPostgresStore_CompactHistory_GenerationError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT generation", err: errors.New("connection lost")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompactHistory(testCtx, "wf-1", []byte(`{"version":1}`), 100, 50)
	if err == nil {
		t.Fatal("expected error from generation query failure, got nil")
	}
	if !strings.Contains(err.Error(), "compact history: get generation") {
		t.Errorf("expected error to contain 'compact history: get generation', got: %v", err)
	}
}

func TestPostgresStore_CompactHistory_DeleteError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT generation", data: [][]driver.Value{{int64(5)}}},
	}, []mockExecResult{
		{match: "DELETE FROM event_history", err: errors.New("delete failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompactHistory(testCtx, "wf-1", []byte(`{"version":1}`), 100, 50)
	if err == nil {
		t.Fatal("expected error from DELETE failure, got nil")
	}
	if !strings.Contains(err.Error(), "compact history: delete") {
		t.Errorf("expected error to contain 'compact history: delete', got: %v", err)
	}
}

func TestPostgresStore_CompactHistory_UpdateError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT generation", data: [][]driver.Value{{int64(5)}}},
	}, []mockExecResult{
		{match: "DELETE FROM event_history", affected: 10},
		{match: "SET compaction_state", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompactHistory(testCtx, "wf-1", []byte(`{"version":1}`), 100, 50)
	if err == nil {
		t.Fatal("expected error from compaction_state UPDATE failure, got nil")
	}
	if !strings.Contains(err.Error(), "compact history: update") {
		t.Errorf("expected error to contain 'compact history: update', got: %v", err)
	}
}

func TestPostgresStore_CompactHistory_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT generation", data: [][]driver.Value{{int64(5)}}},
	}, []mockExecResult{
		{match: "DELETE FROM event_history", affected: 10},
		{match: "SET compaction_state", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompactHistory(testCtx, "wf-1", []byte(`{"version":1,"compacted_step":100}`), 100, 50)
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetEventCount
// ---------------------------------------------------------------------------

func TestPostgresStore_GetEventCount_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT event_count", data: [][]driver.Value{{int64(42)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	count, err := store.GetEventCount(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetEventCount: %v", err)
	}
	if count != 42 {
		t.Errorf("expected 42, got %d", count)
	}
}

func TestPostgresStore_GetEventCount_Zero(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT event_count", data: [][]driver.Value{{int64(0)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	count, err := store.GetEventCount(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetEventCount: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestPostgresStore_GetEventCount_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT event_count", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetEventCount(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected error from query failure, got nil")
	}
	if !strings.Contains(err.Error(), "get event count for wf-1") {
		t.Errorf("expected error to contain 'get event count for wf-1', got: %v", err)
	}
}

func TestPostgresStore_GetEventCount_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetEventCount(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "get event count for wf-1: begin") {
		t.Errorf("expected error to contain 'get event count for wf-1: begin', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AcquireConcurrencyKey — additional edge cases
// ---------------------------------------------------------------------------

func TestPostgresStore_AcquireConcurrencyKey_DoubleAcquireDifferentWorkflow(t *testing.T) {
	// When the key is held by a different workflow, AcquireConcurrencyKey
	// returns false (the INSERT ON CONFLICT DO NOTHING returns no rows).
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	acquired, err := store.AcquireConcurrencyKey(testCtx, "shared-key", "other-wf", 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if acquired {
		t.Error("expected acquired=false when key is held by another workflow")
	}
}

func TestPostgresStore_AcquireConcurrencyKey_ZeroTTL(t *testing.T) {
	// TTL of 0 should still work — the interval becomes "0 seconds".
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "INSERT INTO concurrency_keys", data: [][]driver.Value{{"wf-1"}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	acquired, err := store.AcquireConcurrencyKey(testCtx, "my-key", "wf-1", 0)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey (zero TTL): %v", err)
	}
	if !acquired {
		t.Error("expected acquired=true")
	}
}

func TestPostgresStore_ReleaseConcurrencyKey_NonExistent(t *testing.T) {
	// Releasing a key that does not exist should succeed (DELETE matches 0 rows).
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "DELETE FROM concurrency_keys", affected: 0},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ReleaseConcurrencyKey(testCtx, "nonexistent-key")
	if err != nil {
		t.Fatalf("ReleaseConcurrencyKey (non-existent): %v", err)
	}
}

func TestPostgresStore_ReleaseWorkflowConcurrencyKeys_NonExistent(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "DELETE FROM concurrency_keys", affected: 0},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ReleaseWorkflowConcurrencyKeys(testCtx, "nonexistent-wf")
	if err != nil {
		t.Fatalf("ReleaseWorkflowConcurrencyKeys (non-existent): %v", err)
	}
}

// ---------------------------------------------------------------------------
// ReapExpiredConcurrencyKeys — begin error path
// ---------------------------------------------------------------------------

func TestPostgresStore_ReapExpiredConcurrencyKeys_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ReapExpiredConcurrencyKeys(testCtx)
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "reap expired concurrency keys: begin") {
		t.Errorf("expected error to contain 'reap expired concurrency keys: begin', got: %v", err)
	}
}
