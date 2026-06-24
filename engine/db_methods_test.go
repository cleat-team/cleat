package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
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













// ---- Schedule methods ----





// ---- DeliverSignal ----


// ---- ReleaseWorkflow ----


// ---- RequestCancellation ----


// ---- RecordWorkflowMemorySample ----


// ---- CompactHistory ----


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


// ---------------------------------------------------------------------------
// ClaimWorkflows (complex UPDATE ... RETURNING)
// ---------------------------------------------------------------------------


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




// ---------------------------------------------------------------------------
// AppendEventHistory (single event wrapper)
// ---------------------------------------------------------------------------


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


// ---------------------------------------------------------------------------
// CleanupMemorySamples
// ---------------------------------------------------------------------------


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

// ---------------------------------------------------------------------------
// decryptField and decryptAndRedactEventRecord tests
// ---------------------------------------------------------------------------

func TestPostgresStore_DecryptField_DecryptError(t *testing.T) {
	pe, err := NewPayloadEncryption(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	store := NewPostgresStore(nil)
	store.encryption = pe
	store.encryptSensitivePayloads = true

	result := store.decryptField("not-valid-ciphertext", "Request", "wf-1", 0, false)
	if result != "[DECRYPTION_FAILED]" {
		t.Errorf("expected [DECRYPTION_FAILED], got %q", result)
	}
}

func TestPostgresStore_DecryptField_NilKey(t *testing.T) {
	store := NewPostgresStore(nil)
	store.encryptSensitivePayloads = true
	store.disableReadRedaction = true

	rec := &EventRecord{
		Step:      0,
		EventType: "call",
		Request:   `{"hello":"world"}`,
		Response:  `{"ok":true}`,
	}
	store.decryptAndRedactEventRecord(rec, "wf-1")
	if rec.Request != `{"hello":"world"}` {
		t.Errorf("expected original request, got %q", rec.Request)
	}
	if rec.Response != `{"ok":true}` {
		t.Errorf("expected original response, got %q", rec.Response)
	}
}

// ---------------------------------------------------------------------------
// appendEventsInTx tests
// ---------------------------------------------------------------------------

func TestPostgresStore_AppendEventsInTx_Empty(t *testing.T) {
	store := NewPostgresStore(nil)
	err := store.appendEventsInTx(context.Background(), nil, "wf-1", []EventRecord{})
	if err != nil {
		t.Fatalf("expected nil for empty records, got: %v", err)
	}
}

func TestPostgresStore_AppendEventsInTx_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", err: errors.New("insert failed")},
	})
	defer db.Close()

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	store := NewPostgresStore(db)
	recs := []EventRecord{{Step: 0, EventType: "call", Request: `{}`, Response: `{}`}}
	err = store.appendEventsInTx(context.Background(), tx, "wf-1", recs)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "append one event: exec step 0") {
		t.Errorf("expected exec step error, got: %v", err)
	}
}

func TestPostgresStore_AppendEventsInTx_EventCountError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", affected: 1},
		{match: "event_count = event_count", err: errors.New("event count update failed")},
	})
	defer db.Close()

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	store := NewPostgresStore(db)
	recs := []EventRecord{{Step: 0, EventType: "call", Request: `{}`, Response: `{}`}}
	err = store.appendEventsInTx(context.Background(), tx, "wf-1", recs)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "append events in tx: increment event_count") {
		t.Errorf("expected event_count error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ResolveVersionByTag
// ---------------------------------------------------------------------------

func TestPostgresStore_ResolveVersionByTag_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT version FROM workflow_tags", data: [][]driver.Value{{int64(3)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	version, err := store.ResolveVersionByTag(testCtx, "test-wf", "stable")
	if err != nil {
		t.Fatalf("ResolveVersionByTag: %v", err)
	}
	if version != 3 {
		t.Errorf("expected 3, got %d", version)
	}
}

func TestPostgresStore_ResolveVersionByTag_Latest(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT COALESCE", data: [][]driver.Value{{int64(5)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	version, err := store.ResolveVersionByTag(testCtx, "test-wf", "latest")
	if err != nil {
		t.Fatalf("ResolveVersionByTag(latest): %v", err)
	}
	if version != 5 {
		t.Errorf("expected 5, got %d", version)
	}
}


func TestPostgresStore_ResolveVersionByTag_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT version FROM workflow_tags", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ResolveVersionByTag(testCtx, "test-wf", "stable")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "resolve version by tag") {
		t.Errorf("expected 'resolve version by tag' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// SetWorkflowTag (upsert)
// ---------------------------------------------------------------------------

func TestPostgresStore_SetWorkflowTag_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_tags", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.SetWorkflowTag(testCtx, "test-wf", 1, "stable")
	if err != nil {
		t.Fatalf("SetWorkflowTag: %v", err)
	}
}

func TestPostgresStore_SetWorkflowTag_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.SetWorkflowTag(testCtx, "test-wf", 1, "stable")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "set workflow tag: begin") {
		t.Errorf("expected 'set workflow tag: begin' error, got: %v", err)
	}
}

func TestPostgresStore_SetWorkflowTag_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_tags", err: errors.New("exec failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.SetWorkflowTag(testCtx, "test-wf", 1, "stable")
	if err == nil {
		t.Fatal("expected error from exec failure")
	}
	if !strings.Contains(err.Error(), "set workflow tag") {
		t.Errorf("expected 'set workflow tag' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// RemoveWorkflowTag (DELETE)
// ---------------------------------------------------------------------------

func TestPostgresStore_RemoveWorkflowTag_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "DELETE FROM workflow_tags", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RemoveWorkflowTag(testCtx, "test-wf", "stable")
	if err != nil {
		t.Fatalf("RemoveWorkflowTag: %v", err)
	}
}

func TestPostgresStore_RemoveWorkflowTag_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RemoveWorkflowTag(testCtx, "test-wf", "stable")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "remove workflow tag: begin") {
		t.Errorf("expected 'remove workflow tag: begin' error, got: %v", err)
	}
}

func TestPostgresStore_RemoveWorkflowTag_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "DELETE FROM workflow_tags", err: errors.New("delete failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RemoveWorkflowTag(testCtx, "test-wf", "stable")
	if err == nil {
		t.Fatal("expected error from delete failure")
	}
	if !strings.Contains(err.Error(), "remove workflow tag") {
		t.Errorf("expected 'remove workflow tag' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetWorkflowTag (single row)
// ---------------------------------------------------------------------------

func TestPostgresStore_GetWorkflowTag_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT version FROM workflow_tags", data: [][]driver.Value{{int64(2)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	version, err := store.GetWorkflowTag(testCtx, "test-wf", "stable")
	if err != nil {
		t.Fatalf("GetWorkflowTag: %v", err)
	}
	if version != 2 {
		t.Errorf("expected 2, got %d", version)
	}
}


func TestPostgresStore_GetWorkflowTag_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetWorkflowTag(testCtx, "test-wf", "stable")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "get workflow tag: begin") {
		t.Errorf("expected 'get workflow tag: begin' error, got: %v", err)
	}
}

func TestPostgresStore_GetWorkflowTag_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT version FROM workflow_tags", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetWorkflowTag(testCtx, "test-wf", "stable")
	if err == nil {
		t.Fatal("expected error from query failure")
	}
	if !strings.Contains(err.Error(), "get workflow tag") {
		t.Errorf("expected 'get workflow tag' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetWorkflowTags (multi-row)
// ---------------------------------------------------------------------------

func TestPostgresStore_GetWorkflowTags_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT tag, version FROM workflow_tags",
			data: [][]driver.Value{
				{"stable", int64(2)},
				{"canary", int64(3)},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	tags, err := store.GetWorkflowTags(testCtx, "test-wf")
	if err != nil {
		t.Fatalf("GetWorkflowTags: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("expected 2 tags, got %d", len(tags))
	}
	if tags["stable"] != 2 || tags["canary"] != 3 {
		t.Errorf("unexpected tags: %v", tags)
	}
}


func TestPostgresStore_GetWorkflowTags_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetWorkflowTags(testCtx, "test-wf")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "get workflow tags: begin") {
		t.Errorf("expected 'get workflow tags: begin' error, got: %v", err)
	}
}

func TestPostgresStore_GetWorkflowTags_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT tag, version FROM workflow_tags", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetWorkflowTags(testCtx, "test-wf")
	if err == nil {
		t.Fatal("expected error from query failure")
	}
	if !strings.Contains(err.Error(), "get workflow tags") {
		t.Errorf("expected 'get workflow tags' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Routing rules
// ---------------------------------------------------------------------------

func TestPostgresStore_SetRoutingRule_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_routing", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.SetRoutingRule(testCtx, "test-wf", 2, 0.5)
	if err != nil {
		t.Fatalf("SetRoutingRule: %v", err)
	}
}

func TestPostgresStore_SetRoutingRule_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.SetRoutingRule(testCtx, "test-wf", 2, 0.5)
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
}

func TestPostgresStore_SetRoutingRule_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_routing", err: errors.New("insert failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.SetRoutingRule(testCtx, "test-wf", 2, 0.5)
	if err == nil {
		t.Fatal("expected error from exec failure")
	}
}

func TestPostgresStore_RemoveRoutingRule_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "DELETE FROM workflow_routing", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RemoveRoutingRule(testCtx, "rule-uuid")
	if err != nil {
		t.Fatalf("RemoveRoutingRule: %v", err)
	}
}

func TestPostgresStore_RemoveRoutingRule_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RemoveRoutingRule(testCtx, "rule-uuid")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
}

func TestPostgresStore_RemoveRoutingRule_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "DELETE FROM workflow_routing", err: errors.New("delete failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RemoveRoutingRule(testCtx, "rule-uuid")
	if err == nil {
		t.Fatal("expected error from delete failure")
	}
}

func TestPostgresStore_GetRoutingRules_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT id, workflow_name, target_version, weight",
			data: [][]driver.Value{
				{"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "test-wf", int64(2), float64(0.5)},
				{"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "test-wf", int64(3), float64(0.5)},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	rules, err := store.GetRoutingRules(testCtx, "test-wf")
	if err != nil {
		t.Fatalf("GetRoutingRules: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
}


func TestPostgresStore_GetRoutingRules_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetRoutingRules(testCtx, "test-wf")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
}

func TestPostgresStore_GetRoutingRules_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT id, workflow_name, target_version, weight", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetRoutingRules(testCtx, "test-wf")
	if err == nil {
		t.Fatal("expected error from query failure")
	}
}

// ---------------------------------------------------------------------------
// Schedule operation error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_CreateSchedule_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CreateSchedule(testCtx, Schedule{Name: "daily", DefName: "wf"})
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "create schedule: begin") {
		t.Errorf("expected 'create schedule: begin' error, got: %v", err)
	}
}

func TestPostgresStore_CreateSchedule_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_schedules", err: errors.New("insert failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CreateSchedule(testCtx, Schedule{Name: "daily", DefName: "wf"})
	if err == nil {
		t.Fatal("expected error from exec failure")
	}
}

func TestPostgresStore_CreateSchedule_CommitError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, nil, errors.New("commit failed"))
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CreateSchedule(testCtx, Schedule{Name: "daily", DefName: "wf"})
	if err == nil {
		t.Fatal("expected error from commit failure")
	}
}

func TestPostgresStore_DeleteSchedule_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "DELETE FROM workflow_schedules", err: errors.New("delete failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.DeleteSchedule(testCtx, "daily")
	if err == nil {
		t.Fatal("expected error from delete failure")
	}
}

func TestPostgresStore_DeleteSchedule_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.DeleteSchedule(testCtx, "daily")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
}

func TestPostgresStore_SetScheduleEnabled_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.SetScheduleEnabled(testCtx, "daily", false)
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "set schedule enabled: begin") {
		t.Errorf("expected 'set schedule enabled: begin' error, got: %v", err)
	}
}

func TestPostgresStore_SetScheduleEnabled_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_schedules SET enabled", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.SetScheduleEnabled(testCtx, "daily", false)
	if err == nil {
		t.Fatal("expected error from update failure")
	}
}

func TestPostgresStore_UpdateScheduleNextRun_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_schedules SET next_run_at", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.UpdateScheduleNextRun(testCtx, "daily", time.Now())
	if err == nil {
		t.Fatal("expected error from update failure")
	}
}

func TestPostgresStore_UpdateScheduleNextRun_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.UpdateScheduleNextRun(testCtx, "daily", time.Now())
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
}

func TestPostgresStore_GetDueSchedules_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetDueSchedules(testCtx)
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "get due schedules: begin") {
		t.Errorf("expected 'get due schedules: begin' error, got: %v", err)
	}
}

func TestPostgresStore_GetDueSchedules_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT name, def_name, entry_point", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetDueSchedules(testCtx)
	if err == nil {
		t.Fatal("expected error from query failure")
	}
}

// ---------------------------------------------------------------------------
// FinalizeWorkflowSegment
// ---------------------------------------------------------------------------

func TestPostgresStore_FinalizeWorkflowSegment_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT generation", data: [][]driver.Value{{int64(1)}}},
	}, []mockExecResult{
		{match: "INSERT INTO event_history", affected: 1},
		{match: "SET status = 'done'", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.FinalizeWorkflowSegment(testCtx, "wf-1", "worker-1", 1, nil, "done", `{"result":"ok"}`, "", "", nil, time.Time{})
	if err != nil {
		t.Fatalf("FinalizeWorkflowSegment: %v", err)
	}
}

func TestPostgresStore_FinalizeWorkflowSegment_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.FinalizeWorkflowSegment(testCtx, "wf-1", "worker-1", 1, nil, "done", `{}`, "", "", nil, time.Time{})
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "finalize workflow: begin tx") {
		t.Errorf("expected 'begin tx' error, got: %v", err)
	}
}

func TestPostgresStore_FinalizeWorkflowSegment_Suspend(t *testing.T) {
	nextWake := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT generation", data: [][]driver.Value{{int64(1)}}},
	}, []mockExecResult{
		{match: "SET status = 'ready'", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.FinalizeWorkflowSegment(testCtx, "wf-1", "worker-1", 1, nil, "ready", "", "", "", nil, nextWake)
	if err != nil {
		t.Fatalf("FinalizeWorkflowSegment(suspend): %v", err)
	}
}


// ---------------------------------------------------------------------------
// StreamEventHistory
// ---------------------------------------------------------------------------

func TestPostgresStore_StreamEventHistory_Error(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT step, event_type", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	eventCh, errCh := store.StreamEventHistory(context.Background(), "wf-1", 10)

	err := <-errCh
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// eventCh should be closed after error
	_, ok := <-eventCh
	if ok {
		t.Error("expected eventCh to be closed after error")
	}
}

func TestPostgresStore_StreamEventHistory_Empty(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT step, event_type", data: [][]driver.Value{}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	eventCh, errCh := store.StreamEventHistory(context.Background(), "wf-1", 10)

	// Should receive no events and no error
	select {
	case rec, ok := <-eventCh:
		if ok {
			t.Errorf("unexpected event: %+v", rec)
		}
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func TestPostgresStore_StreamEventHistory_SuccessWithPageSizeZero(t *testing.T) {
	// pageSize <= 0 should default to 1000
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT step, event_type", data: [][]driver.Value{
			{int64(0), "call", "", "", `{"req":"data"}`, `{"resp":"ok"}`, "", int64(0), "", int64(0), "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", nil, int64(0)},
		}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	eventCh, errCh := store.StreamEventHistory(context.Background(), "wf-1", 0)

	select {
	case rec := <-eventCh:
		if rec.Step != 0 || rec.EventType != "call" {
			t.Errorf("unexpected event: %+v", rec)
		}
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// ContinueAsNew (Postgres variant)
// ---------------------------------------------------------------------------

func TestPostgresStore_ContinueAsNew_SuccessMock(t *testing.T) {
	newRunID := "new-run-uuid"
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "INSERT INTO workflow_instances", data: [][]driver.Value{{newRunID}}},
	}, []mockExecResult{
		{match: "INSERT INTO event_history", affected: 1},
		{match: "SET status = 'done'", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	events := []EventRecord{{Step: 0, EventType: "call"}}
	id, err := store.ContinueAsNew(testCtx, "current-run", "worker-1", 1, "test-wf", 2, json.RawMessage(`{}`), events, `{"result":"ok"}`, nil, 0)
	if err != nil {
		t.Fatalf("ContinueAsNew: %v", err)
	}
	if id != newRunID {
		t.Errorf("expected %q, got %q", newRunID, id)
	}
}

func TestPostgresStore_ContinueAsNew_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ContinueAsNew(testCtx, "current-run", "worker-1", 1, "test-wf", 2, json.RawMessage(`{}`), nil, `{}`, nil, 0)
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "continue as new: begin") {
		t.Errorf("expected 'continue as new: begin' error, got: %v", err)
	}
}

func TestPostgresStore_ContinueAsNew_AppendEventsError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", err: errors.New("insert failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	events := []EventRecord{{Step: 0, EventType: "call", Request: `{}`, Response: `{}`}}
	_, err := store.ContinueAsNew(testCtx, "current-run", "worker-1", 1, "test-wf", 2, json.RawMessage(`{}`), events, `{}`, nil, 0)
	if err == nil {
		t.Fatal("expected error from append events failure")
	}
	if !strings.Contains(err.Error(), "continue as new: append events") {
		t.Errorf("expected 'append events' error, got: %v", err)
	}
}

func TestPostgresStore_ContinueAsNew_NewRunError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	events := []EventRecord{{Step: 0, EventType: "call"}}
	_, err := store.ContinueAsNew(testCtx, "current-run", "worker-1", 1, "test-wf", 2, json.RawMessage(`{}`), events, `{}`, nil, 0)
	if err == nil {
		t.Fatal("expected error from new run insert failure")
	}
}

// ---------------------------------------------------------------------------
// RecordWorkflowMemorySample — error path
// ---------------------------------------------------------------------------

func TestPostgresStore_RecordWorkflowMemorySample_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RecordWorkflowMemorySample(testCtx, "wf-def", 4096)
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "record memory sample: begin") {
		t.Errorf("expected 'record memory sample: begin' error, got: %v", err)
	}
}

func TestPostgresStore_RecordWorkflowMemorySample_InsertError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_memory_samples", err: errors.New("insert failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RecordWorkflowMemorySample(testCtx, "wf-def", 4096)
	if err == nil {
		t.Fatal("expected error from insert failure")
	}
	if !strings.Contains(err.Error(), "record memory sample: insert sample") {
		t.Errorf("expected 'insert sample' error, got: %v", err)
	}
}

func TestPostgresStore_RecordWorkflowMemorySample_UpsertError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_memory_samples", affected: 1},
		{match: "INSERT INTO workflow_memory_stats", err: errors.New("upsert failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RecordWorkflowMemorySample(testCtx, "wf-def", 4096)
	if err == nil {
		t.Fatal("expected error from upsert failure")
	}
	if !strings.Contains(err.Error(), "record memory sample: upsert stats") {
		t.Errorf("expected 'upsert stats' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// LoadMemoryEstimates — error path
// ---------------------------------------------------------------------------

func TestPostgresStore_LoadMemoryEstimates_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT def_name, mean_bytes", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.LoadMemoryEstimates(testCtx)
	if err == nil {
		t.Fatal("expected error from query failure")
	}
	if !strings.Contains(err.Error(), "load memory estimates") {
		t.Errorf("expected 'load memory estimates' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// LoadMemoryStats — error path
// ---------------------------------------------------------------------------

func TestPostgresStore_LoadMemoryStats_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT def_name", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.LoadMemoryStats(testCtx)
	if err == nil {
		t.Fatal("expected error from query failure")
	}
	if !strings.Contains(err.Error(), "load memory stats") {
		t.Errorf("expected 'load memory stats' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CleanupMemorySamples — error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_CleanupMemorySamples_DefQueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT DISTINCT def_name", err: errors.New("list defs failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.CleanupMemorySamples(testCtx, 100)
	if err == nil {
		t.Fatal("expected error from list defs failure")
	}
	if !strings.Contains(err.Error(), "cleanup memory samples: list defs") {
		t.Errorf("expected 'list defs' error, got: %v", err)
	}
}

func TestPostgresStore_CleanupMemorySamples_DeleteError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT DISTINCT def_name", data: [][]driver.Value{{"wf-a"}}},
	}, []mockExecResult{
		{match: "DELETE FROM workflow_memory_samples", err: errors.New("delete failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.CleanupMemorySamples(testCtx, 100)
	if err == nil {
		t.Fatal("expected error from delete failure")
	}
}

// ---------------------------------------------------------------------------
// ListSchedules — error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_ListSchedules_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT name, def_name, entry_point", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ListSchedules(testCtx)
	if err == nil {
		t.Fatal("expected error from query failure")
	}
}

func TestPostgresStore_ListSchedules_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ListSchedules(testCtx)
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
}

// ---------------------------------------------------------------------------
// StartChildWorkflowAtomic (Postgres variant)
// ---------------------------------------------------------------------------

func TestPostgresStore_StartChildWorkflowAtomic_Success(t *testing.T) {
	childID := "child-uuid"
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "INSERT INTO workflow_instances", data: [][]driver.Value{{childID}}},
	}, []mockExecResult{
		{match: "SELECT set_config"}, // from beginTxWithRLS
		{match: "INSERT INTO event_history", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	event := EventRecord{
		Step:       0,
		EventType:  "child_workflow",
		ChildName:  "child-wf",
		ChildInput: `{}`,
	}
	id, err := store.StartChildWorkflowAtomic(testCtx, "", "parent-1", "child-wf", `{}`, 1, "ABANDON", event, 0)
	if err != nil {
		t.Fatalf("StartChildWorkflowAtomic: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty child ID")
	}
}

func TestPostgresStore_StartChildWorkflowAtomic_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.StartChildWorkflowAtomic(testCtx, "child-id", "parent-1", "child-wf", `{}`, 1, "ABANDON", EventRecord{}, 0)
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
}

func TestPostgresStore_StartChildWorkflowAtomic_InsertChildError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_instances", err: errors.New("insert child failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.StartChildWorkflowAtomic(testCtx, "child-id", "parent-1", "child-wf", `{}`, 1, "ABANDON", EventRecord{}, 0)
	if err == nil {
		t.Fatal("expected error from insert child failure")
	}
}

// ---------------------------------------------------------------------------
// CompactHistory — begin error path
// ---------------------------------------------------------------------------

func TestPostgresStore_CompactHistory_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompactHistory(testCtx, "wf-1", []byte(`{}`), 100, 50)
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "compact history: begin") {
		t.Errorf("expected 'compact history: begin' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// StartChildWorkflow (Postgres variant) — error path
// ---------------------------------------------------------------------------

func TestPostgresStore_StartChildWorkflow_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "INSERT INTO workflow_instances", err: errors.New("insert failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.StartChildWorkflow(testCtx, "parent-1", "child-wf", `{}`, 1, "ABANDON", 0)
	if err == nil {
		t.Fatal("expected error from insert failure")
	}
}

// ---------------------------------------------------------------------------
// PickVersionByRouting
// ---------------------------------------------------------------------------


func TestPostgresStore_PickVersionByRouting_WithRules(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "SELECT id, workflow_name, target_version, weight",
			data: [][]driver.Value{
				{"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "test-wf", int64(2), float64(1.0)},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	version, err := store.PickVersionByRouting(testCtx, "test-wf")
	if err != nil {
		t.Fatalf("PickVersionByRouting: %v", err)
	}
	if version != 2 {
		t.Errorf("expected version 2, got %d", version)
	}
}

// ---------------------------------------------------------------------------
// GetChildResult (Postgres variant) — error path
// ---------------------------------------------------------------------------

func TestPostgresStore_GetChildResult_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT COALESCE", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, _, err := store.GetChildResult(testCtx, "child-1")
	if err == nil {
		t.Fatal("expected error from query failure")
	}
}

// ---------------------------------------------------------------------------
// CheckCancellation (Postgres variant) — error path
// ---------------------------------------------------------------------------

func TestPostgresStore_CheckCancellation_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT cancellation_requested", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, _, err := store.CheckCancellation(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected error from query failure")
	}
}

// ---------------------------------------------------------------------------
// DeliverSignal (Postgres variant) — error path
// ---------------------------------------------------------------------------

func TestPostgresStore_DeliverSignal_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_signals", err: errors.New("insert failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.DeliverSignal(testCtx, "wf-1", "sig", `{}`)
	if err == nil {
		t.Fatal("expected error from insert failure")
	}
}

// ---------------------------------------------------------------------------
// PollAndClaimSignal (Postgres variant) — error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_PollAndClaimSignal_DeleteError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "DELETE FROM workflow_signals", err: errors.New("delete failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, _, err := store.PollAndClaimSignal(testCtx, "wf-1", "sig")
	if err == nil {
		t.Fatal("expected error from delete failure")
	}
}

func TestPostgresStore_PollAndClaimSignal_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, _, err := store.PollAndClaimSignal(testCtx, "wf-1", "sig")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
}

// ---------------------------------------------------------------------------
// MoveToDeadLetterQueue — error path
// ---------------------------------------------------------------------------

func TestPostgresStore_MoveToDeadLetterQueue_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.MoveToDeadLetterQueue(testCtx, "wf-1", "worker-1", 1, "err", "ERR", "op")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
}


// ---------------------------------------------------------------------------
// BatchHeartbeat (Postgres variant) — error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_BatchHeartbeat_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances", affected: 3},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	n, err := store.BatchHeartbeat(testCtx, "worker-1")
	if err != nil {
		t.Fatalf("BatchHeartbeat: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3, got %d", n)
	}
}

func TestPostgresStore_BatchHeartbeat_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET heartbeat_at", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.BatchHeartbeat(testCtx, "worker-1")
	if err == nil {
		t.Fatal("expected error from update failure")
	}
}

// ---------------------------------------------------------------------------
// Heartbeat (Postgres variant) — error path
// ---------------------------------------------------------------------------

func TestPostgresStore_Heartbeat_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.Heartbeat(testCtx, "wf-1", "worker-1", 0)
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "heartbeat: begin") {
		t.Errorf("expected 'heartbeat: begin' error, got: %v", err)
	}
}

func TestPostgresStore_Heartbeat_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET heartbeat_at", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.Heartbeat(testCtx, "wf-1", "worker-1", 0)
	if err == nil {
		t.Fatal("expected error from update failure")
	}
}

// ---------------------------------------------------------------------------
// CompleteWorkflow (Postgres variant) — error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_CompleteWorkflow_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompleteWorkflow(testCtx, "wf-1", "worker-1", 0, `{}`, nil)
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "complete workflow: begin") {
		t.Errorf("expected 'complete workflow: begin' error, got: %v", err)
	}
}

func TestPostgresStore_CompleteWorkflow_CommitError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances SET status = 'done'", affected: 1},
	}, nil, errors.New("commit failed"))
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompleteWorkflow(testCtx, "wf-1", "worker-1", 0, `{}`, nil)
	if err == nil {
		t.Fatal("expected error from commit failure")
	}
}

// ---------------------------------------------------------------------------
// FailWorkflow (Postgres variant) — error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_FailWorkflow_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.FailWorkflow(testCtx, "wf-1", "worker-1", 0, "err", "", "", nil)
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "fail workflow: begin") {
		t.Errorf("expected 'fail workflow: begin' error, got: %v", err)
	}
}

func TestPostgresStore_FailWorkflow_CommitError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances SET status = 'failed'", affected: 1},
	}, nil, errors.New("commit failed"))
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.FailWorkflow(testCtx, "wf-1", "worker-1", 0, "err", "", "", nil)
	if err == nil {
		t.Fatal("expected error from commit failure")
	}
}

// ---------------------------------------------------------------------------
// AppendEventHistoryBatch (Postgres variant)
// ---------------------------------------------------------------------------

func TestPostgresStore_AppendEventHistoryBatch_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", affected: 1},
		{match: "event_count", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	recs := []EventRecord{{
		Step: 0, EventType: "call", Service: "svc", Op: "op",
		Request: `{}`, Response: `{}`,
	}}
	err := store.AppendEventHistoryBatch(testCtx, "wf-1", recs)
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}
}

func TestPostgresStore_AppendEventHistoryBatch_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.AppendEventHistoryBatch(testCtx, "wf-1", nil)
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch (nil): %v", err)
	}
	err = store.AppendEventHistoryBatch(testCtx, "wf-1", []EventRecord{})
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch (empty): %v", err)
	}
}

func TestPostgresStore_AppendEventHistoryBatch_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("connection refused"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	recs := []EventRecord{{Step: 0, EventType: "call", Service: "svc"}}
	err := store.AppendEventHistoryBatch(testCtx, "wf-1", recs)
	if err == nil {
		t.Fatal("expected error from begin tx failure, got nil")
	}
	if !strings.Contains(err.Error(), "append history batch: begin tx") {
		t.Errorf("expected error to contain 'append history batch: begin tx', got: %v", err)
	}
}

func TestPostgresStore_AppendEventHistoryBatch_InsertError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", err: errors.New("insert failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	recs := []EventRecord{{Step: 0, EventType: "call", Service: "svc"}}
	err := store.AppendEventHistoryBatch(testCtx, "wf-1", recs)
	if err == nil {
		t.Fatal("expected error from INSERT failure, got nil")
	}
	if !strings.Contains(err.Error(), "append one event") {
		t.Errorf("expected error to contain 'append one event', got: %v", err)
	}
}

func TestPostgresStore_AppendEventHistoryBatch_RLSError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "set_config", err: errors.New("rls error")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	recs := []EventRecord{{Step: 0, EventType: "call", Service: "svc"}}
	err := store.AppendEventHistoryBatch(testCtx, "wf-1", recs)
	if err == nil {
		t.Fatal("expected error from set_config failure, got nil")
	}
	if !strings.Contains(err.Error(), "append history batch: set rls") {
		t.Errorf("expected error to contain 'append history batch: set rls', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AppendEventHistory (single event wrapper)
// ---------------------------------------------------------------------------

func TestPostgresStore_AppendEventHistory_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO event_history", affected: 1},
		{match: "event_count", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	rec := EventRecord{Step: 0, EventType: "call", Service: "svc", Op: "op"}
	err := store.AppendEventHistory(testCtx, "wf-1", rec)
	if err != nil {
		t.Fatalf("AppendEventHistory: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TraceWorkflow
// ---------------------------------------------------------------------------

func TestPostgresStore_TraceWorkflow_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances SET trace_id", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.TraceWorkflow(testCtx, "wf-1", "trace-1")
	if err != nil {
		t.Fatalf("TraceWorkflow: %v", err)
	}
}

func TestPostgresStore_TraceWorkflow_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances SET trace_id", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.TraceWorkflow(testCtx, "wf-1", "trace-1")
	if err == nil {
		t.Fatal("expected error from UPDATE failure")
	}
}

func TestPostgresStore_TraceWorkflow_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.TraceWorkflow(testCtx, "wf-1", "trace-1")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "trace workflow: begin") {
		t.Errorf("expected 'trace workflow: begin' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// RequestCancellation
// ---------------------------------------------------------------------------

func TestPostgresStore_RequestCancellation_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET cancellation_requested", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RequestCancellation(testCtx, "wf-1", "reason")
	if err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}
}

func TestPostgresStore_RequestCancellation_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET cancellation_requested", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RequestCancellation(testCtx, "wf-1", "reason")
	if err == nil {
		t.Fatal("expected error from UPDATE failure")
	}
}

func TestPostgresStore_RequestCancellation_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RequestCancellation(testCtx, "wf-1", "reason")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "request cancellation: begin") {
		t.Errorf("expected 'request cancellation: begin' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreatePromise
// ---------------------------------------------------------------------------

func TestPostgresStore_CreatePromise_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_promises", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CreatePromise(testCtx, "wf-1", "my-promise", "promise-1")
	if err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}
}

func TestPostgresStore_CreatePromise_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_promises", err: errors.New("insert failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CreatePromise(testCtx, "wf-1", "my-promise", "promise-1")
	if err == nil {
		t.Fatal("expected error from INSERT failure")
	}
}

func TestPostgresStore_CreatePromise_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CreatePromise(testCtx, "wf-1", "my-promise", "promise-1")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "create promise: begin") {
		t.Errorf("expected 'create promise: begin' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ResolvePromise
// ---------------------------------------------------------------------------

func TestPostgresStore_ResolvePromise_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_promises SET status", affected: 1},
		{match: "UPDATE workflow_instances SET next_wake_at", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ResolvePromise(testCtx, "wf-1", "promise-1", `{"result":"ok"}`)
	if err != nil {
		t.Fatalf("ResolvePromise: %v", err)
	}
}

func TestPostgresStore_ResolvePromise_PromiseUpdateError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_promises SET status", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ResolvePromise(testCtx, "wf-1", "promise-1", `{}`)
	if err == nil {
		t.Fatal("expected error from promise update failure")
	}
}

func TestPostgresStore_ResolvePromise_WakeUpdateError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_promises SET status", affected: 1},
		{match: "UPDATE workflow_instances SET next_wake_at", err: errors.New("wake update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ResolvePromise(testCtx, "wf-1", "promise-1", `{}`)
	if err == nil {
		t.Fatal("expected error from wake update failure")
	}
}

func TestPostgresStore_ResolvePromise_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.ResolvePromise(testCtx, "wf-1", "promise-1", `{}`)
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "resolve promise: begin") {
		t.Errorf("expected 'resolve promise: begin' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// RejectPromise
// ---------------------------------------------------------------------------

func TestPostgresStore_RejectPromise_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_promises SET status", affected: 1},
		{match: "UPDATE workflow_instances SET next_wake_at", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RejectPromise(testCtx, "wf-1", "promise-1", "error msg")
	if err != nil {
		t.Fatalf("RejectPromise: %v", err)
	}
}

func TestPostgresStore_RejectPromise_PromiseUpdateError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_promises SET status", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RejectPromise(testCtx, "wf-1", "promise-1", "error")
	if err == nil {
		t.Fatal("expected error from promise update failure")
	}
}

func TestPostgresStore_RejectPromise_WakeUpdateError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_promises SET status", affected: 1},
		{match: "UPDATE workflow_instances SET next_wake_at", err: errors.New("wake update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RejectPromise(testCtx, "wf-1", "promise-1", "error")
	if err == nil {
		t.Fatal("expected error from wake update failure")
	}
}

func TestPostgresStore_RejectPromise_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.RejectPromise(testCtx, "wf-1", "promise-1", "error")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "reject promise: begin") {
		t.Errorf("expected 'reject promise: begin' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// UpdateStickyWorker
// ---------------------------------------------------------------------------

func TestPostgresStore_UpdateStickyWorker_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances SET sticky_worker_id", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.UpdateStickyWorker(testCtx, "wf-1", "worker-1")
	if err != nil {
		t.Fatalf("UpdateStickyWorker: %v", err)
	}
}

func TestPostgresStore_UpdateStickyWorker_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances SET sticky_worker_id", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.UpdateStickyWorker(testCtx, "wf-1", "worker-1")
	if err == nil {
		t.Fatal("expected error from UPDATE failure")
	}
	if !strings.Contains(err.Error(), "update sticky worker") {
		t.Errorf("expected error to contain 'update sticky worker', got: %v", err)
	}
}

func TestPostgresStore_UpdateStickyWorker_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.UpdateStickyWorker(testCtx, "wf-1", "worker-1")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "update sticky worker: begin") {
		t.Errorf("expected 'update sticky worker: begin' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreateUpdateRequest
// ---------------------------------------------------------------------------

func TestPostgresStore_CreateUpdateRequest_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_update_requests", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CreateUpdateRequest(testCtx, "wf-1", "update-1", `{"key":"val"}`, "promise-1")
	if err != nil {
		t.Fatalf("CreateUpdateRequest: %v", err)
	}
}

func TestPostgresStore_CreateUpdateRequest_NonJSONPayload(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_update_requests", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CreateUpdateRequest(testCtx, "wf-1", "update-1", "raw-string", "promise-1")
	if err != nil {
		t.Fatalf("CreateUpdateRequest: %v", err)
	}
}

func TestPostgresStore_CreateUpdateRequest_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_update_requests", err: errors.New("insert failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CreateUpdateRequest(testCtx, "wf-1", "update-1", `{}`, "promise-1")
	if err == nil {
		t.Fatal("expected error from INSERT failure")
	}
}

func TestPostgresStore_CreateUpdateRequest_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CreateUpdateRequest(testCtx, "wf-1", "update-1", `{}`, "promise-1")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "create update request: begin") {
		t.Errorf("expected 'create update request: begin' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CompleteUpdateRequest
// ---------------------------------------------------------------------------

func TestPostgresStore_CompleteUpdateRequest_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_update_requests", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompleteUpdateRequest(testCtx, "wf-1", "update-1", `{"result":"ok"}`, "")
	if err != nil {
		t.Fatalf("CompleteUpdateRequest: %v", err)
	}
}

func TestPostgresStore_CompleteUpdateRequest_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_update_requests", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompleteUpdateRequest(testCtx, "wf-1", "update-1", `{}`, "")
	if err == nil {
		t.Fatal("expected error from UPDATE failure")
	}
}

func TestPostgresStore_CompleteUpdateRequest_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompleteUpdateRequest(testCtx, "wf-1", "update-1", `{}`, "")
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "complete update request: begin") {
		t.Errorf("expected 'complete update request: begin' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// OpenStore / Close
// ---------------------------------------------------------------------------

func TestPostgresStore_OpenStore_PublicSchema(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	defer db.Close()

	factory := NewPostgresStoreFactory(db, "public")
	store, closer, err := factory.OpenStore(testCtx, "tenant-1")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store")
	}
	pgStore := store.(*PostgresStore)
	if pgStore.tenantID != "tenant-1" {
		t.Errorf("expected tenant-1, got %q", pgStore.tenantID)
	}
	if err := closer.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestPostgresStore_OpenStore_CustomSchema(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "CREATE SCHEMA", affected: 0},
	})
	defer db.Close()

	factory := NewPostgresStoreFactory(db, "my_schema")
	store, closer, err := factory.OpenStore(testCtx, "tenant-1")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store")
	}
	if err := closer.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestPostgresStore_OpenStore_SchemaError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "CREATE SCHEMA", err: errors.New("permission denied")},
	})
	defer db.Close()

	factory := NewPostgresStoreFactory(db, "my_schema")
	_, _, err := factory.OpenStore(testCtx, "tenant-1")
	if err == nil {
		t.Fatal("expected error from schema creation failure")
	}
	if !strings.Contains(err.Error(), "create schema") {
		t.Errorf("expected 'create schema' error, got: %v", err)
	}
}

func TestPostgresStore_OpenStore_DefaultSchema(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	defer db.Close()

	factory := NewPostgresStoreFactory(db, "")
	store, closer, err := factory.OpenStore(testCtx, "tenant-1")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store")
	}
	if err := closer.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestPostgresStore_Close(t *testing.T) {
	var c nopCloser
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// computeEventChecksum tests
// ---------------------------------------------------------------------------

func TestComputeEventChecksum_NoPrevious(t *testing.T) {
	rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "s", Op: "o"}
	checksum := computeEventChecksum(rec, "")
	if checksum == "" {
		t.Error("expected non-empty checksum")
	}
	if len(checksum) != 16 {
		t.Errorf("expected 16 hex chars, got %d", len(checksum))
	}
}

func TestComputeEventChecksum_WithPrevious(t *testing.T) {
	rec := EventRecord{Step: 1, EventType: EventTypeSleep, DurationMs: 5000}
	checksum := computeEventChecksum(rec, "abc123")
	if checksum == "" {
		t.Error("expected non-empty checksum")
	}
}

func TestComputeEventChecksum_Deterministic(t *testing.T) {
	rec := EventRecord{Step: 0, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "f"}
	c1 := computeEventChecksum(rec, "")
	c2 := computeEventChecksum(rec, "")
	if c1 != c2 {
		t.Error("checksum should be deterministic for same input")
	}
}

// ---------------------------------------------------------------------------
// eventRecordToPayload tests
// ---------------------------------------------------------------------------

func TestEventRecordToPayload_Call(t *testing.T) {
	rec := EventRecord{
		Step: 0, EventType: "call",
		Service: "s", Op: "o", Request: `{"key":"val"}`,
		Response: `{"result":"ok"}`, DurationMs: 100,
	}
	data, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["service"] != "s" || m["operation"] != "o" {
		t.Errorf("unexpected payload: %v", m)
	}
	if m["duration_ms"] != float64(100) {
		t.Errorf("expected duration_ms 100, got %v", m["duration_ms"])
	}
}

func TestEventRecordToPayload_PluginCall(t *testing.T) {
	rec := EventRecord{
		Step: 0, EventType: "plugin_call",
		PluginName: "p", PluginFunc: "f",
		PluginInput: `{"in":"put"}`, PluginOutput: `{"out":"put"}`,
		PluginError: "some error",
	}
	data, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["plugin_name"] != "p" || m["plugin_error"] != "some error" {
		t.Errorf("unexpected payload: %v", m)
	}
}

func TestEventRecordToPayload_EmptyEvent(t *testing.T) {
	rec := EventRecord{Step: 0, EventType: "unknown_type"}
	data, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	if string(data) != "{}" {
		t.Errorf("expected empty object, got %s", data)
	}
}

func TestEventRecordToPayload_Sleep(t *testing.T) {
	rec := EventRecord{Step: 0, EventType: "sleep", DurationMs: 3000}
	data, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["duration_ms"] != float64(3000) {
		t.Errorf("expected duration_ms 3000, got %v", m["duration_ms"])
	}
}

func TestEventRecordToPayload_Fetch(t *testing.T) {
	rec := EventRecord{
		Step: 0, EventType: "fetch",
		FetchMethod: "GET", FetchURL: "http://example.com",
		FetchHeaders: `{"Accept":"text/plain"}`,
		FetchBody: `{"q":"search"}`, FetchResponse: `{"status":"ok"}`,
	}
	data, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["fetch_method"] != "GET" || m["fetch_url"] != "http://example.com" {
		t.Errorf("unexpected payload: %v", m)
	}
}

func TestEventRecordToPayload_AcquireLock(t *testing.T) {
	rec := EventRecord{
		Step: 0, EventType: "acquire_lock",
		LockKey: "my-lock", LockTTLMs: 60000, LockAcquired: 1,
	}
	data, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["lock_key"] != "my-lock" || m["lock_acquired"] != float64(1) {
		t.Errorf("unexpected payload: %v", m)
	}
}

// ---------------------------------------------------------------------------
// populateFromPayload tests
// ---------------------------------------------------------------------------

func TestPopulateFromPayload_InvalidJSON(t *testing.T) {
	rec := &EventRecord{Step: 0, EventType: "call"}
	populateFromPayload(rec, []byte("{invalid json"))
	if rec.Service != "" {
		t.Errorf("expected no change, got Service=%q", rec.Service)
	}
}

func TestPopulateFromPayload_CallFields(t *testing.T) {
	payload := `{"service":"my-svc","operation":"my-op","request_b64":"` +
		base64.StdEncoding.EncodeToString([]byte(`{"key":"val"}`)) +
		`","response_b64":"` +
		base64.StdEncoding.EncodeToString([]byte(`{"result":"ok"}`)) +
		`","error":"","duration_ms":200}`
	rec := &EventRecord{Step: 0, EventType: "call"}
	populateFromPayload(rec, []byte(payload))
	if rec.Service != "my-svc" || rec.Op != "my-op" {
		t.Errorf("unexpected: Service=%q Op=%q", rec.Service, rec.Op)
	}
	if rec.Request != `{"key":"val"}` {
		t.Errorf("unexpected Request=%q", rec.Request)
	}
	if rec.Response != `{"result":"ok"}` {
		t.Errorf("unexpected Response=%q", rec.Response)
	}
	if rec.DurationMs != 200 {
		t.Errorf("unexpected DurationMs=%d", rec.DurationMs)
	}
}

func TestPopulateFromPayload_FallbackToRawFields(t *testing.T) {
	payload := `{"service":"s","operation":"o","request":"raw-req","response":"raw-resp"}`
	rec := &EventRecord{Step: 0, EventType: "call"}
	populateFromPayload(rec, []byte(payload))
	if rec.Request != "raw-req" || rec.Response != "raw-resp" {
		t.Errorf("unexpected: Request=%q Response=%q", rec.Request, rec.Response)
	}
}

func TestPopulateFromPayload_Sleep(t *testing.T) {
	payload := `{"duration_ms":3000}`
	rec := &EventRecord{Step: 0, EventType: "sleep"}
	populateFromPayload(rec, []byte(payload))
	if rec.DurationMs != 3000 {
		t.Errorf("expected DurationMs 3000, got %d", rec.DurationMs)
	}
}

func TestPopulateFromPayload_AwaitSignals(t *testing.T) {
	payload := `{"signal_names":"[\"sig1\"]","timeout_ms":5000}`
	rec := &EventRecord{Step: 0, EventType: "await_signals"}
	populateFromPayload(rec, []byte(payload))
	if rec.SignalNames != `["sig1"]` {
		t.Errorf("expected SignalNames %q, got %q", `["sig1"]`, rec.SignalNames)
	}
	if rec.TimeoutMs != 5000 {
		t.Errorf("expected TimeoutMs 5000, got %d", rec.TimeoutMs)
	}
}

// ---------------------------------------------------------------------------
// DeliverSignal tests — error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_DeliverSignal_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.DeliverSignal(testCtx, "wf-1", "sig", `{}`)
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "deliver signal: begin") {
		t.Errorf("expected 'deliver signal: begin' error, got: %v", err)
	}
}

func TestPostgresStore_DeliverSignal_InsertError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_signals", err: errors.New("insert failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.DeliverSignal(testCtx, "wf-1", "sig", `{}`)
	if err == nil {
		t.Fatal("expected error from insert failure")
	}
}

func TestPostgresStore_DeliverSignal_UpdateError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_signals", affected: 1},
		{match: "UPDATE workflow_instances", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.DeliverSignal(testCtx, "wf-1", "sig", `{}`)
	if err == nil {
		t.Fatal("expected error from update failure")
	}
}

func TestPostgresStore_DeliverSignal_InvalidPayload(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_signals", affected: 1},
		{match: "UPDATE workflow_instances", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.DeliverSignal(testCtx, "wf-1", "sig", "plain text")
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}
}

func TestPostgresStore_DeliverSignal_CommitError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_signals", affected: 1},
		{match: "UPDATE workflow_instances", affected: 1},
	}, nil, errors.New("commit failed"))
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.DeliverSignal(testCtx, "wf-1", "sig", `{}`)
	if err == nil {
		t.Fatal("expected error from commit failure")
	}
}

// ---------------------------------------------------------------------------
// decryptPayloadJSON tests
// ---------------------------------------------------------------------------

func TestDecryptPayloadJSON_NoEncryption(t *testing.T) {
	store := NewPostgresStore(nil)
	result := store.decryptPayloadJSON(`{"plain":"text"}`)
	if result != `{"plain":"text"}` {
		t.Errorf("expected original payload, got %q", result)
	}
}

func TestDecryptPayloadJSON_EmptyPayload(t *testing.T) {
	enc := newTestPayloadEncryption(t)
	store := NewPostgresStore(nil).WithEncryption(enc, true)
	result := store.decryptPayloadJSON("")
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestDecryptPayloadJSON_DecryptionFailure(t *testing.T) {
	enc := newTestPayloadEncryption(t)
	store := NewPostgresStore(nil).WithEncryption(enc, true)
	result := store.decryptPayloadJSON(`"corrupted-base64-data"`)
	if result != `"corrupted-base64-data"` {
		t.Errorf("expected original payload on decryption failure, got %q", result)
	}
}

func TestDecryptPayloadJSON_Success(t *testing.T) {
	enc := newTestPayloadEncryption(t)
	store := NewPostgresStore(nil).WithEncryption(enc, true)
	encrypted, err := enc.EncryptJSON([]byte(`{"secret":"data"}`))
	if err != nil {
		t.Fatalf("EncryptJSON: %v", err)
	}
	result := store.decryptPayloadJSON(string(encrypted))
	if result != `{"secret":"data"}` {
		t.Errorf("expected decrypted payload, got %q", result)
	}
}

func newTestPayloadEncryption(t *testing.T) *PayloadEncryption {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	enc, err := NewPayloadEncryption(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	return enc
}

// ---------------------------------------------------------------------------
// decryptAndRedactEventRecord tests
// ---------------------------------------------------------------------------

func TestDecryptAndRedactEventRecord_NoEncryption(t *testing.T) {
	store := NewPostgresStore(nil)
	rec := &EventRecord{Step: 0, Request: "hello", Response: "world"}
	store.decryptAndRedactEventRecord(rec, "wf-1")
	if rec.Request == "" {
		t.Error("expected Request to be preserved (redacted or not)")
	}
}

func TestDecryptAndRedactEventRecord_InvalidEncryptedData(t *testing.T) {
	enc := newTestPayloadEncryption(t)
	store := NewPostgresStore(nil).WithEncryption(enc, true)
	rec := &EventRecord{
		Step: 0, Request: "tampered-data", Response: "bad-data",
		Err: "invalid-ciphertext",
	}
	store.decryptAndRedactEventRecord(rec, "wf-1")
	if rec.Request != "[DECRYPTION_FAILED]" {
		t.Errorf("expected [DECRYPTION_FAILED] for Request, got %q", rec.Request)
	}
	if rec.Response != "[DECRYPTION_FAILED]" {
		t.Errorf("expected [DECRYPTION_FAILED] for Response, got %q", rec.Response)
	}
	if rec.Err != "[DECRYPTION_FAILED]" {
		t.Errorf("expected [DECRYPTION_FAILED] for Err, got %q", rec.Err)
	}
}

// ---------------------------------------------------------------------------
// ListWorkflows tests
// ---------------------------------------------------------------------------

func TestPostgresStore_ListWorkflows_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ListWorkflows(testCtx, WorkflowFilter{})
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
}

func TestPostgresStore_ListWorkflows_EmptyResult(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_instances", data: nil},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ListWorkflows(testCtx, WorkflowFilter{})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 0 {
		t.Errorf("expected 0 workflows, got %d", len(wfs))
	}
}

func TestPostgresStore_ListWorkflows_StatusFilter(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "status =", data: [][]driver.Value{
			{"wf-1", "my-wf", int64(1), "running", []byte(`{}`), "worker-1", nil, "", "", nil, nil, int64(0), int64(0), ""},
		}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ListWorkflows(testCtx, WorkflowFilter{Status: "running"})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 1 {
		t.Errorf("expected 1 workflow, got %d", len(wfs))
	}
	if wfs[0].ID != "wf-1" {
		t.Errorf("expected ID 'wf-1', got %q", wfs[0].ID)
	}
}

func TestPostgresStore_ListWorkflows_SearchFilter(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "OR", data: [][]driver.Value{
			{"wf-2", "my-wf", int64(1), "done", []byte(`{}`), "", nil, "", "", nil, nil, int64(0), int64(0), ""},
		}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ListWorkflows(testCtx, WorkflowFilter{Search: "search-term"})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 1 {
		t.Errorf("expected 1 workflow, got %d", len(wfs))
	}
}

func TestPostgresStore_ListWorkflows_InputContainsFilter(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "ILIKE", data: [][]driver.Value{
			{"wf-3", "my-wf", int64(1), "running", []byte(`{}`), "", nil, "", "", nil, nil, int64(0), int64(0), ""},
		}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ListWorkflows(testCtx, WorkflowFilter{InputContains: "search-term"})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 1 {
		t.Errorf("expected 1 workflow, got %d", len(wfs))
	}
}

func TestPostgresStore_ListWorkflows_LimitClamping(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "ORDER BY", data: nil},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ListWorkflows(testCtx, WorkflowFilter{Limit: 5000})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
}

func TestPostgresStore_ListWorkflows_DefaultLimit(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "ORDER BY", data: nil},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ListWorkflows(testCtx, WorkflowFilter{Limit: 0})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ClaimWorkflows error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_ClaimWorkflows_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ClaimWorkflows(testCtx, "worker-1", 1)
	if err == nil {
		t.Fatal("expected error from begin failure")
	}
}

func TestPostgresStore_ClaimWorkflows_NoRows(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "UPDATE workflow_instances", data: nil},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ClaimWorkflows(testCtx, "worker-1", 1)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	if len(wfs) != 0 {
		t.Errorf("expected 0 workflows, got %d", len(wfs))
	}
}

// ---------------------------------------------------------------------------
// claimWorkflowImpl tests
// ---------------------------------------------------------------------------

func TestClaimWorkflowImpl_ReturnsNil(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "UPDATE workflow_instances", data: nil},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wf, err := store.claimWorkflowImpl(testCtx, "worker-1")
	if err != nil {
		t.Fatalf("claimWorkflowImpl: %v", err)
	}
	if wf != nil {
		t.Errorf("expected nil workflow, got %+v", wf)
	}
}

// ---------------------------------------------------------------------------
// Encryption edge cases
// ---------------------------------------------------------------------------

func TestDecryptAndRedactEventRecord_WithRedactionDisabled(t *testing.T) {
	store := NewPostgresStore(nil).WithReadRedactionDisabled(true)
	rec := &EventRecord{Step: 0, Request: "sensitive-data"}
	store.decryptAndRedactEventRecord(rec, "wf-1")
	if rec.Request != "sensitive-data" {
		t.Errorf("expected original data when redaction disabled, got %q", rec.Request)
	}
}

func TestNewPayloadEncryption_BadKey(t *testing.T) {
	_, err := NewPayloadEncryption("not-valid-base64!!!")
	if err == nil {
		t.Error("expected error for invalid base64 key")
	}
}

func TestNewPayloadEncryption_WrongKeyLength(t *testing.T) {
	shortKey := base64.StdEncoding.EncodeToString([]byte("short"))
	_, err := NewPayloadEncryption(shortKey)
	if err == nil {
		t.Error("expected error for wrong key length")
	}
	if err != nil && !strings.Contains(err.Error(), "key must be exactly 32 bytes") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// computeEventChecksum -- additional event types
// ---------------------------------------------------------------------------

func TestComputeEventChecksum_SideEffect(t *testing.T) {
	rec := EventRecord{Step: 0, EventType: EventTypeSideEffect, SideEffectResult: "result-data"}
	c1 := computeEventChecksum(rec, "")
	c2 := computeEventChecksum(rec, "")
	if c1 == "" || len(c1) != 16 {
		t.Error("expected non-empty 16-char hex checksum")
	}
	if c1 != c2 {
		t.Error("checksum should be deterministic for same input")
	}
}

func TestComputeEventChecksum_StateMutation(t *testing.T) {
	rec := EventRecord{Step: 0, EventType: EventTypeStateMutation, StateKey: "k", StateValue: "v", StateDelta: 42}
	c1 := computeEventChecksum(rec, "")
	c2 := computeEventChecksum(rec, "")
	if c1 != c2 {
		t.Error("checksum should be deterministic")
	}
	if len(c1) != 16 {
		t.Error("expected 16 hex chars")
	}
}

func TestComputeEventChecksum_RunDetached(t *testing.T) {
	rec := EventRecord{Step: 0, EventType: EventTypeRunDetached, DetachedName: "child", DetachedInput: `{"x":1}`}
	c := computeEventChecksum(rec, "")
	if c == "" || len(c) != 16 {
		t.Error("expected non-empty 16-char hex checksum")
	}
}

// ---------------------------------------------------------------------------
// populateFromPayload -- additional event types
// ---------------------------------------------------------------------------

func TestPopulateFromPayload_SignalReceived(t *testing.T) {
	payload := `{"signal_name":"my-signal","signal_payload":"{\"data\":1}"}`
	rec := &EventRecord{Step: 0, EventType: "signal_received"}
	populateFromPayload(rec, []byte(payload))
	if rec.SignalName != "my-signal" {
		t.Errorf("expected SignalName 'my-signal', got %q", rec.SignalName)
	}
	if rec.SignalPayload != `{"data":1}` {
		t.Errorf("expected SignalPayload %q, got %q", `{"data":1}`, rec.SignalPayload)
	}
}

func TestPopulateFromPayload_ChildWorkflow(t *testing.T) {
	payload := `{"child_name":"child-wf","child_input":"{}","run_id":"run-123"}`
	rec := &EventRecord{Step: 0, EventType: "child_workflow"}
	populateFromPayload(rec, []byte(payload))
	if rec.ChildName != "child-wf" || rec.RunID != "run-123" {
		t.Errorf("unexpected: ChildName=%q RunID=%q", rec.ChildName, rec.RunID)
	}
	if rec.ChildInput != "{}" {
		t.Errorf("unexpected ChildInput=%q", rec.ChildInput)
	}
}

func TestPopulateFromPayload_Promise(t *testing.T) {
	// create_promise populates promise_name and promise_id
	payload := `{"promise_name":"p-name","promise_id":"p-id"}`
	rec := &EventRecord{Step: 0, EventType: "create_promise"}
	populateFromPayload(rec, []byte(payload))
	if rec.PromiseName != "p-name" || rec.PromiseID != "p-id" {
		t.Errorf("unexpected: PromiseName=%q PromiseID=%q", rec.PromiseName, rec.PromiseID)
	}
	// promise_resolved populates promise_result
	payload2 := `{"promise_id":"p-id","promise_result":"ok"}`
	rec2 := &EventRecord{Step: 1, EventType: "promise_resolved"}
	populateFromPayload(rec2, []byte(payload2))
	if rec2.PromiseID != "p-id" || rec2.PromiseResult != "ok" {
		t.Errorf("unexpected resolved: PromiseID=%q PromiseResult=%q", rec2.PromiseID, rec2.PromiseResult)
	}
	// promise_rejected with error
	payload3 := `{"promise_id":"p-id-2","promise_error":"err msg"}`
	rec3 := &EventRecord{Step: 2, EventType: "promise_rejected"}
	populateFromPayload(rec3, []byte(payload3))
	if rec3.PromiseID != "p-id-2" || rec3.PromiseError != "err msg" {
		t.Errorf("unexpected rejected: PromiseID=%q PromiseError=%q", rec3.PromiseID, rec3.PromiseError)
	}
}

func TestPopulateFromPayload_AcquireReleaseLock(t *testing.T) {
	// acquire_lock
	payload := `{"lock_key":"my-lock","lock_ttl_ms":30000,"lock_acquired":1}`
	rec := &EventRecord{Step: 0, EventType: "acquire_lock"}
	populateFromPayload(rec, []byte(payload))
	if rec.LockKey != "my-lock" || rec.LockTTLMs != 30000 || rec.LockAcquired != 1 {
		t.Errorf("unexpected: LockKey=%q LockTTLMs=%d LockAcquired=%d", rec.LockKey, rec.LockTTLMs, rec.LockAcquired)
	}
	// release_lock
	payload2 := `{"lock_key":"released-lock"}`
	rec2 := &EventRecord{Step: 1, EventType: "release_lock"}
	populateFromPayload(rec2, []byte(payload2))
	if rec2.LockKey != "released-lock" {
		t.Errorf("expected LockKey 'released-lock', got %q", rec2.LockKey)
	}
}

func TestPopulateFromPayload_Fetch(t *testing.T) {
	payload := `{"fetch_method":"POST","fetch_url":"http://example.com","fetch_headers":"{}","fetch_body":"body","fetch_response_b64":"` +
		base64.StdEncoding.EncodeToString([]byte(`{"ok":true}`)) +
		`","error":""}`
	rec := &EventRecord{Step: 0, EventType: "fetch"}
	populateFromPayload(rec, []byte(payload))
	if rec.FetchMethod != "POST" || rec.FetchURL != "http://example.com" {
		t.Errorf("unexpected: Method=%q URL=%q", rec.FetchMethod, rec.FetchURL)
	}
	if rec.FetchResponse != `{"ok":true}` {
		t.Errorf("unexpected FetchResponse=%q", rec.FetchResponse)
	}
}

// ---------------------------------------------------------------------------
// eventRecordToPayload -- additional event types
// ---------------------------------------------------------------------------

func TestEventRecordToPayload_SignalReceived(t *testing.T) {
	rec := EventRecord{
		Step: 0, EventType: "signal_received",
		SignalName: "my-signal", SignalPayload: `{"key":"val"}`,
	}
	data, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["signal_name"] != "my-signal" {
		t.Errorf("expected signal_name 'my-signal', got %v", m["signal_name"])
	}
	if m["signal_payload"] != `{"key":"val"}` {
		t.Errorf("expected signal_payload %q, got %v", `{"key":"val"}`, m["signal_payload"])
	}
}

func TestEventRecordToPayload_AwaitSignals(t *testing.T) {
	rec := EventRecord{
		Step: 0, EventType: "await_signals",
		SignalNames: `["sig1","sig2"]`, TimeoutMs: 10000,
	}
	data, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["signal_names"] != `["sig1","sig2"]` {
		t.Errorf("expected signal_names %q, got %v", `["sig1","sig2"]`, m["signal_names"])
	}
	if m["timeout_ms"] != float64(10000) {
		t.Errorf("expected timeout_ms 10000, got %v", m["timeout_ms"])
	}
}

func TestEventRecordToPayload_ChildWorkflow(t *testing.T) {
	rec := EventRecord{
		Step: 0, EventType: "child_workflow",
		ChildName: "child-wf", ChildInput: `{"x":1}`, RunID: "run-123",
	}
	data, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["child_name"] != "child-wf" || m["run_id"] != "run-123" {
		t.Errorf("unexpected payload: %v", m)
	}
}

func TestEventRecordToPayload_ReleaseLock(t *testing.T) {
	rec := EventRecord{
		Step: 0, EventType: "release_lock",
		LockKey: "my-lock",
	}
	data, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["lock_key"] != "my-lock" {
		t.Errorf("expected lock_key 'my-lock', got %v", m["lock_key"])
	}
}

func TestEventRecordToPayload_SideEffect(t *testing.T) {
	rec := EventRecord{
		Step: 0, EventType: "side_effect",
		SideEffectResult: "result-data",
	}
	data, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["side_effect_result"] != "result-data" {
		t.Errorf("expected side_effect_result 'result-data', got %v", m["side_effect_result"])
	}
}

func TestEventRecordToPayload_ErrorEvent(t *testing.T) {
	rec := EventRecord{
		Step: 0, EventType: "call",
		Service: "svc", Op: "op", Request: `{}`,
		Err: "something went wrong",
	}
	data, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["error"] != "something went wrong" {
		t.Errorf("expected error field, got %v", m["error"])
	}
}

// ---------------------------------------------------------------------------
// LoadWASM / LoadWorkflowConfig / LoadDAGSpec error paths
// ---------------------------------------------------------------------------

func TestPostgresStore_LoadWASM_NotFound(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT wasm_bytes", data: nil},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.LoadWASM(testCtx, "nonexistent", 1)
	if err == nil {
		t.Fatal("expected error for not found")
	}
	if !strings.Contains(err.Error(), "wasm not found") {
		t.Errorf("expected 'wasm not found' error, got: %v", err)
	}
}

func TestPostgresStore_LoadWorkflowConfig_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT max_history_length", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.LoadWorkflowConfig(testCtx, "test-wf", 1)
	if err == nil {
		t.Fatal("expected error from query failure")
	}
	if !strings.Contains(err.Error(), "load workflow config") {
		t.Errorf("expected 'load workflow config' error, got: %v", err)
	}
}

func TestPostgresStore_LoadDAGSpec_NotFound(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT dag_spec", data: nil},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.LoadDAGSpec(testCtx, "nonexistent", 1)
	if err == nil {
		t.Fatal("expected error for not found")
	}
	if !strings.Contains(err.Error(), "workflow def not found") {
		t.Errorf("expected 'workflow def not found' error, got: %v", err)
	}
}

func TestPostgresStore_LoadDAGSpec_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT dag_spec", err: errors.New("db error")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.LoadDAGSpec(testCtx, "test-wf", 1)
	if err == nil {
		t.Fatal("expected error from query failure")
	}
	if !strings.Contains(err.Error(), "load dag_spec") {
		t.Errorf("expected 'load dag_spec' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// beginTxWithRLS -- set_config failure
// ---------------------------------------------------------------------------

func TestBeginTxWithRLS_SetConfigError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "set_config", err: errors.New("set_config failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.LoadWASM(testCtx, "test-wf", 1)
	if err == nil {
		t.Fatal("expected error from set_config failure")
	}
	if !strings.Contains(err.Error(), "set row-level security") {
		t.Errorf("expected 'set row-level security' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ListWorkflows -- additional filter combinations
// ---------------------------------------------------------------------------

func TestPostgresStore_ListWorkflows_ErrorContainsFilter(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "error_msg", data: [][]driver.Value{
			{"wf-1", "my-wf", int64(1), "failed", []byte(`{}`), "worker-1", nil, "ERR001", "some-op", "something failed", nil, int64(0), int64(0), ""},
		}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ListWorkflows(testCtx, WorkflowFilter{ErrorContains: "failed"})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	if wfs[0].Error != "something failed" {
		t.Errorf("expected Error 'something failed', got %q", wfs[0].Error)
	}
}

func TestPostgresStore_ListWorkflows_WithOffset(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "OFFSET", data: [][]driver.Value{
			{"wf-1", "my-wf", int64(1), "running", []byte(`{}`), "", nil, "", "", nil, nil, int64(0), int64(0), ""},
		}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	wfs, err := store.ListWorkflows(testCtx, WorkflowFilter{Limit: 10, Offset: 5})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 1 {
		t.Errorf("expected 1 workflow, got %d", len(wfs))
	}
}

// ---------------------------------------------------------------------------
// ClaimWorkflows -- scan error
// ---------------------------------------------------------------------------

func TestPostgresStore_ClaimWorkflows_ScanError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "UPDATE workflow_instances",
			data: [][]driver.Value{
				// Wrong type for def_version (string instead of int64) causes scan error
				{"wf-1", "test-wf", "not-an-int", "running", []byte(`{}`), "worker-1", nil, nil, nil, nil, nil, int64(0), int64(0), ""},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.ClaimWorkflows(testCtx, "worker-1", 1)
	if err == nil {
		t.Fatal("expected scan error")
	}
	if !strings.Contains(err.Error(), "claim workflows scan") {
		t.Errorf("expected 'claim workflows scan' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// decryptAndRedactEventRecord — successful decryption of all fields
// ---------------------------------------------------------------------------

func TestDecryptAndRedactEventRecord_SuccessfulDecryption(t *testing.T) {
	enc := newTestPayloadEncryption(t)
	store := NewPostgresStore(nil).WithEncryption(enc, true).WithReadRedactionDisabled(true)

	// Request and Response are stored as raw ciphertext bytes (already
	// base64-decoded by tryDecodeBase64), so decryptField uses
	// encryption.Decrypt (useBytesDecrypt=true).
	plainReq := `{"hello":"world"}`
	rawReq, err := enc.Encrypt([]byte(plainReq))
	if err != nil {
		t.Fatalf("Encrypt request: %v", err)
	}
	plainResp := `{"ok":true}`
	rawResp, err := enc.Encrypt([]byte(plainResp))
	if err != nil {
		t.Fatalf("Encrypt response: %v", err)
	}

	// Err is stored as a base64-encoded ciphertext, so decryptField uses
	// encryption.DecryptString (useBytesDecrypt=false).
	plainErr := "operation failed"
	encodedErr, err := enc.EncryptString(plainErr)
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}

	rec := &EventRecord{
		Step:     0,
		Request:  string(rawReq),
		Response: string(rawResp),
		Err:      encodedErr,
	}
	store.decryptAndRedactEventRecord(rec, "wf-1")

	if rec.Request != plainReq {
		t.Errorf("expected Request=%q, got %q", plainReq, rec.Request)
	}
	if rec.Response != plainResp {
		t.Errorf("expected Response=%q, got %q", plainResp, rec.Response)
	}
	if rec.Err != plainErr {
		t.Errorf("expected Err=%q, got %q", plainErr, rec.Err)
	}
}

// ---------------------------------------------------------------------------
// DeliverSignal — valid JSON payload
// ---------------------------------------------------------------------------

func TestPostgresStore_DeliverSignal_ValidJSON(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_signals", affected: 1},
		{match: "UPDATE workflow_instances", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	// Valid JSON payload — the code should insert it as-is (no quoting).
	err := store.DeliverSignal(testCtx, "wf-1", "sig", `{"key":"value"}`)
	if err != nil {
		t.Fatalf("DeliverSignal with valid JSON: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetWorkflowByID — query error path
// ---------------------------------------------------------------------------

func TestPostgresStore_GetWorkflowByID_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT id, def_name, def_version", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.GetWorkflowByID(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected error from query failure")
	}
	if !strings.Contains(err.Error(), "get workflow") {
		t.Errorf("expected 'get workflow' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Heartbeat — RowsAffected greater than 1
// ---------------------------------------------------------------------------

func TestPostgresStore_Heartbeat_MultipleRows(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET heartbeat_at", affected: 3},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	owned, err := store.Heartbeat(testCtx, "wf-1", "worker-1", 0)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !owned {
		t.Error("expected owned=true when RowsAffected=3")
	}
}

// ---------------------------------------------------------------------------
// CompleteWorkflow — UPDATE affects 0 rows (code ignores RowsAffected)
// ---------------------------------------------------------------------------

func TestPostgresStore_CompleteWorkflow_ZeroRowsAffected(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances SET status = 'done'", affected: 0},
		{match: "UPDATE idempotency_keys SET result =", affected: 0},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.CompleteWorkflow(testCtx, "wf-1", "worker-1", 0, `{}`, nil)
	if err != nil {
		t.Fatalf("CompleteWorkflow should succeed even with 0 rows affected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// FailWorkflow — exec error path
// ---------------------------------------------------------------------------

func TestPostgresStore_FailWorkflow_ExecError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances", err: errors.New("update failed")},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.FailWorkflow(testCtx, "wf-1", "worker-1", 0, "err", "", "", nil)
	if err == nil {
		t.Fatal("expected error from exec failure")
	}
}

// ---------------------------------------------------------------------------
// LoadEventHistory — query error path
// ---------------------------------------------------------------------------

func TestPostgresStore_LoadEventHistory_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT step, event_type, service", err: errors.New("select failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.LoadEventHistory(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected error from query failure")
	}
	if !strings.Contains(err.Error(), "load history") {
		t.Errorf("expected 'load history' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetChildResult — scan error path
// ---------------------------------------------------------------------------

func TestPostgresStore_GetChildResult_ScanError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		// struct{} is not a supported driver.Value type for *string Scan target.
		{match: "SELECT COALESCE", data: [][]driver.Value{{`{}`, struct{}{}}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, _, err := store.GetChildResult(testCtx, "child-1")
	if err == nil {
		t.Fatal("expected scan error")
	}
	if !strings.Contains(err.Error(), "get child result") {
		t.Errorf("expected 'get child result' error, got: %v", err)
	}
}

