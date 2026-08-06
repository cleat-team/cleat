package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"

	_ "github.com/microsoft/go-mssqldb"
)

// ---------------------------------------------------------------------------
// Test helper
// ---------------------------------------------------------------------------

// setupMSSQLIntegrationTest opens a connection to the MSSQL test database,
// creates the full schema, builds a fresh MSSQLStore, and registers a cleanup
// via t.Cleanup. Tests should call this at the top of their function.
// When CLEAT_TEST_MSSQL is not set the test is skipped.
func setupMSSQLIntegrationTest(t *testing.T) (*MSSQLStore, *sql.DB) {
	t.Helper()

	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping MSSQL integration test")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}

	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	// The stored procedures are part of the schema these tests exercise --
	// FinalizeWorkflowSegment EXECs finalize_workflow_status. Without this
	// they passed only because some *other* test had installed the
	// procedure into the same database via MSSQLBackend.Setup, and
	// CREATE PROCEDURE persists, so the dependency was invisible after the
	// first full run. On a fresh database a filtered run failed with
	// "Could not find stored procedure 'finalize_workflow_status'".
	applyMSSQLProcedures(t, db)
	testutil.CleanupMSSQLTestData(t, db)

	// Built the way production builds it. NewMSSQLStore(db, ...) on a plain
	// pool has no sp_set_session_context on any of its connections, so under
	// the shipped schema's security policies every tenant-scoped read matches
	// nothing -- the store cannot see rows it just wrote. Every non-test caller
	// goes through the factory instead (cmd/cleat-worker, cmd/cleat-bench,
	// cmd/deploy-workflow), and OpenStore is what wraps the connector.
	//
	// This is the §1.3 shape: the tests were exercising a construction nothing
	// ships, and it passed only because the hand-written test schema had no
	// policies to enforce.
	ws, closer, err := NewMSSQLStoreFactory(dsn).OpenStore(
		context.Background(), DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("open a tenant-scoped store: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	store, ok := ws.(*MSSQLStore)
	if !ok {
		t.Fatalf("OpenStore returned %T, want *MSSQLStore", ws)
	}

	t.Cleanup(func() {
		testutil.CleanupMSSQLTestData(t, db)
	})

	// Assertions in these tests read tables directly with raw SQL, and those
	// reads are subject to the same policies. They are checking what the store
	// did, across whatever tenant it did it as, which is administrative work.
	return store, testutil.MSSQLAdminDB(t, db)
}

// deployWorkflowDef is a helper that inserts a workflow_def row via the store.
func deployWorkflowDef(t *testing.T, store *MSSQLStore, name string, version int, wasm []byte) {
	t.Helper()
	def := &WorkflowDef{
		Name:       name,
		Version:    version,
		WASMBytes:  wasm,
		ABIVersion: 1,
		MinVersion: 1,
	}
	if err := store.DeployWorkflowDef(context.Background(), def); err != nil {
		t.Fatalf("DeployWorkflowDef(%s, %d): %v", name, version, err)
	}
}

// ---------------------------------------------------------------------------
// Workflow Definition Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_DeployAndGetWorkflowDef(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()

	wasm := []byte{0x00, 0x61, 0x73, 0x6d}
	def := &WorkflowDef{
		Name:       "integration-test-wf",
		Version:    1,
		WASMBytes:  wasm,
		ABIVersion: 2,
		MinVersion: 1,
	}
	if err := store.DeployWorkflowDef(ctx, def); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	// GetWorkflowDef
	got, err := store.GetWorkflowDef(ctx, "integration-test-wf", 1)
	if err != nil {
		t.Fatalf("GetWorkflowDef: %v", err)
	}
	if got == nil {
		t.Fatal("GetWorkflowDef returned nil")
	}
	if got.Name != "integration-test-wf" || got.Version != 1 {
		t.Errorf("name/version = %q/%d, want integration-test-wf/1", got.Name, got.Version)
	}
	if got.ABIVersion != 2 {
		t.Errorf("ABIVersion = %d, want 2", got.ABIVersion)
	}
	if got.MinVersion != 1 {
		t.Errorf("MinVersion = %d, want 1", got.MinVersion)
	}
	if got.Deprecated {
		t.Error("Deprecated should be false")
	}
	if len(got.PluginDeps) != 0 {
		t.Errorf("PluginDeps = %v, want empty", got.PluginDeps)
	}

	// Verify WASM bytes via direct SQL.
	var storedWASM []byte
	err = db.QueryRowContext(ctx,
		`SELECT wasm_bytes FROM workflow_defs WHERE name = @p1 AND version = @p2`,
		"integration-test-wf", 1).Scan(&storedWASM)
	if err != nil {
		t.Fatalf("query wasm_bytes: %v", err)
	}
	if string(storedWASM) != string(wasm) {
		t.Errorf("stored WASM = %v, want %v", storedWASM, wasm)
	}
}

// ---------------------------------------------------------------------------
// Workflow Lifecycle Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_StartNewRun(t *testing.T) {
	store, _ := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "lifecycle-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// Start a new run without idempotency key.
	runID, isDup, err := store.StartNewRun(ctx, "", "lifecycle-wf", 1,
		json.RawMessage(`{"key":"val"}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	if runID == "" {
		t.Fatal("expected non-empty runID")
	}
	if isDup {
		t.Fatal("unexpected duplicate")
	}

	// Start with idempotency key.
	runID2, isDup2, err := store.StartNewRun(ctx, "", "lifecycle-wf", 1,
		json.RawMessage(`{"key":"val2"}`), "idem-key-001", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun with idemkey: %v", err)
	}
	if runID2 == "" {
		t.Fatal("expected non-empty runID for idempotent start")
	}
	if isDup2 {
		t.Fatal("unexpected duplicate for new idem key")
	}

	// Reuse the same idempotency key — should return existing run.
	runID3, isDup3, err := store.StartNewRun(ctx, "", "lifecycle-wf", 1,
		json.RawMessage(`{"key":"val2"}`), "idem-key-001", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun duplicate idemkey: %v", err)
	}
	if runID3 != runID2 {
		t.Errorf("duplicate idemkey returned %s, want %s", runID3, runID2)
	}
	if !isDup3 {
		t.Fatal("expected duplicate=true for repeated idempotency key")
	}
}

func TestMSSQLIntegration_ClaimAndCompleteWorkflow(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "claim-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// Start a run directly via SQL so it's in 'ready' state with next_wake_at in the past.
	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'claim-wf', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// Claim the workflow.
	wf, err := store.ClaimWorkflow(ctx, "worker-1")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if wf == nil {
		t.Fatal("ClaimWorkflow returned nil")
	}
	if wf.ID != wfID {
		t.Errorf("ClaimWorkflow id = %s, want %s", wf.ID, wfID)
	}
	if wf.Status != "running" {
		t.Errorf("status = %s, want running", wf.Status)
	}
	if wf.AssignedTo != "worker-1" {
		t.Errorf("assigned_to = %s, want worker-1", wf.AssignedTo)
	}
	if wf.Generation <= 0 {
		t.Errorf("generation should be > 0, got %d", wf.Generation)
	}

	// Complete the workflow.
	err = store.CompleteWorkflow(ctx, wfID, "worker-1", wf.Generation, `{"result":"ok"}`, nil)
	if err != nil {
		t.Fatalf("CompleteWorkflow: %v", err)
	}

	// Verify completion.
	var status, result string
	err = db.QueryRowContext(ctx,
		`SELECT status, ISNULL(result, '') FROM workflow_instances WHERE id = @p1`, wfID).Scan(&status, &result)
	if err != nil {
		t.Fatalf("query completed status: %v", err)
	}
	if status != "done" {
		t.Errorf("status after complete = %s, want done", status)
	}
	if result != `{"result":"ok"}` {
		t.Errorf("result = %s, want {\"result\":\"ok\"}", result)
	}
}

func TestMSSQLIntegration_ClaimWorkflowsBatch(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "batch-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// Insert two ready workflows with past next_wake_at.
	ids := []string{uuid.New().String(), uuid.New().String()}
	for _, id := range ids {
		_, err := db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
			VALUES (@p1, 'batch-wf', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default')
		`, id)
		if err != nil {
			t.Fatalf("insert workflow %s: %v", id, err)
		}
	}

	// Claim up to 10.
	claimed, err := store.ClaimWorkflows(ctx, "batch-worker", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("ClaimWorkflows returned %d workflows, want 2", len(claimed))
	}

	claimedIDs := map[string]bool{}
	for _, wf := range claimed {
		claimedIDs[wf.ID] = true
		if wf.Status != "running" {
			t.Errorf("workflow %s status = %s, want running", wf.ID, wf.Status)
		}
		if wf.AssignedTo != "batch-worker" {
			t.Errorf("workflow %s assigned_to = %s, want batch-worker", wf.ID, wf.AssignedTo)
		}
	}
	if !claimedIDs[ids[0]] || !claimedIDs[ids[1]] {
		t.Error("not all workflows were claimed")
	}

	// Second claim should return nothing (all already claimed).
	remaining, err := store.ClaimWorkflows(ctx, "batch-worker", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows second call: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("second claim returned %d workflows, want 0", len(remaining))
	}
}

func TestMSSQLIntegration_ClaimStickyWorkflows(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "sticky-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// Insert a workflow sticky to worker-A.
	sfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, sticky_worker_id)
		VALUES (@p1, 'sticky-wf', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', 'worker-A')
	`, sfID)
	if err != nil {
		t.Fatalf("insert sticky workflow: %v", err)
	}

	// Claim sticky for worker-A.
	claimed, err := store.ClaimStickyWorkflows(ctx, "worker-A", 10)
	if err != nil {
		t.Fatalf("ClaimStickyWorkflows: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 sticky workflow, got %d", len(claimed))
	}
	if claimed[0].ID != sfID {
		t.Errorf("claimed id = %s, want %s", claimed[0].ID, sfID)
	}

	// No sticky workflows for worker-B.
	claimedB, err := store.ClaimStickyWorkflows(ctx, "worker-B", 10)
	if err != nil {
		t.Fatalf("ClaimStickyWorkflows worker-B: %v", err)
	}
	if len(claimedB) != 0 {
		t.Errorf("expected 0 for worker-B, got %d", len(claimedB))
	}
}

func TestMSSQLIntegration_FailWorkflow(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "fail-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'fail-wf', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// Claim then fail.
	wf, err := store.ClaimWorkflow(ctx, "worker-fail")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	err = store.FailWorkflow(ctx, wfID, "worker-fail", wf.Generation, "something went wrong", "ERR_CODE", "op_foo", nil)
	if err != nil {
		t.Fatalf("FailWorkflow: %v", err)
	}

	var status, errorMsg, errorCode, errorOp string
	err = db.QueryRowContext(ctx,
		`SELECT status, ISNULL(error_msg, ''), ISNULL(error_code, ''), ISNULL(error_op, '') FROM workflow_instances WHERE id = @p1`, wfID).
		Scan(&status, &errorMsg, &errorCode, &errorOp)
	if err != nil {
		t.Fatalf("query fail state: %v", err)
	}
	if status != "failed" {
		t.Errorf("status = %s, want failed", status)
	}
	if errorMsg != "something went wrong" {
		t.Errorf("error_msg = %s", errorMsg)
	}
	if errorCode != "ERR_CODE" {
		t.Errorf("error_code = %s", errorCode)
	}
	if errorOp != "op_foo" {
		t.Errorf("error_op = %s", errorOp)
	}
}

func TestMSSQLIntegration_DeadLetterAndRetry(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "dlq-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'dlq-wf', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	wf, err := store.ClaimWorkflow(ctx, "worker-dlq")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}

	// Move to dead letter queue.
	err = store.MoveToDeadLetterQueue(ctx, wfID, "worker-dlq", wf.Generation, "DLQ reason", "DLQ_CODE", "op_dlq")
	if err != nil {
		t.Fatalf("MoveToDeadLetterQueue: %v", err)
	}

	var status string
	err = db.QueryRowContext(ctx,
		`SELECT status FROM workflow_instances WHERE id = @p1`, wfID).Scan(&status)
	if err != nil {
		t.Fatalf("query dlq status: %v", err)
	}
	if status != "dead_lettered" {
		t.Errorf("status = %s, want dead_lettered", status)
	}

	// Retry the workflow.
	if err := store.RetryWorkflow(ctx, wfID); err != nil {
		t.Fatalf("RetryWorkflow: %v", err)
	}

	err = db.QueryRowContext(ctx,
		`SELECT status FROM workflow_instances WHERE id = @p1`, wfID).Scan(&status)
	if err != nil {
		t.Fatalf("query retry status: %v", err)
	}
	if status != "ready" {
		t.Errorf("status after retry = %s, want ready", status)
	}
}

func TestMSSQLIntegration_Heartbeat(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "hb-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'hb-wf', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	wf, err := store.ClaimWorkflow(ctx, "worker-hb")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}

	// Heartbeat with correct generation.
	ok, err := store.Heartbeat(ctx, wfID, "worker-hb", wf.Generation)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !ok {
		t.Fatal("Heartbeat returned false, want true")
	}

	// Heartbeat with wrong generation should return false.
	ok, err = store.Heartbeat(ctx, wfID, "worker-hb", 9999)
	if err != nil {
		t.Fatalf("Heartbeat wrong gen: %v", err)
	}
	if ok {
		t.Fatal("Heartbeat with wrong generation returned true, want false")
	}

	// Heartbeat with wrong worker should return false.
	ok, err = store.Heartbeat(ctx, wfID, "wrong-worker", wf.Generation)
	if err != nil {
		t.Fatalf("Heartbeat wrong worker: %v", err)
	}
	if ok {
		t.Fatal("Heartbeat with wrong worker returned true, want false")
	}
}

func TestMSSQLIntegration_BatchHeartbeat(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "bhb-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// Insert two ready workflows and claim them.
	ids := []string{uuid.New().String(), uuid.New().String()}
	for _, id := range ids {
		_, err := db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
			VALUES (@p1, 'bhb-wf', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default')
		`, id)
		if err != nil {
			t.Fatalf("insert workflow_instance: %v", err)
		}
	}

	// Claim them all.
	claimed, err := store.ClaimWorkflows(ctx, "batch-hb-worker", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("expected 2 claimed, got %d", len(claimed))
	}

	// Batch heartbeat.
	n, err := store.BatchHeartbeat(ctx, "batch-hb-worker")
	if err != nil {
		t.Fatalf("BatchHeartbeat: %v", err)
	}
	if n != 2 {
		t.Errorf("BatchHeartbeat affected %d rows, want 2", n)
	}

	// Unknown worker has no running workflows.
	n, err = store.BatchHeartbeat(ctx, "unknown-worker")
	if err != nil {
		t.Fatalf("BatchHeartbeat unknown: %v", err)
	}
	if n != 0 {
		t.Errorf("BatchHeartbeat for unknown worker affected %d rows, want 0", n)
	}
}

func TestMSSQLIntegration_ReleaseWorkflow(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "release-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'release-wf', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	wf, err := store.ClaimWorkflow(ctx, "worker-rel")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}

	futureWake := time.Now().Add(30 * time.Second)
	err = store.ReleaseWorkflow(ctx, wfID, "worker-rel", wf.Generation, futureWake)
	if err != nil {
		t.Fatalf("ReleaseWorkflow: %v", err)
	}

	var status string
	var nextWakeAt time.Time
	err = db.QueryRowContext(ctx,
		`SELECT status, next_wake_at FROM workflow_instances WHERE id = @p1`, wfID).Scan(&status, &nextWakeAt)
	if err != nil {
		t.Fatalf("query release state: %v", err)
	}
	if status != "ready" {
		t.Errorf("status = %s, want ready", status)
	}
	if nextWakeAt.Before(time.Now()) {
		t.Error("next_wake_at should be in the future")
	}
}

// ---------------------------------------------------------------------------
// Event History Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_EventHistory(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "evt-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, 'evt-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default', '00000000-0000-0000-0000-000000000000')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// AppendEventHistory.
	rec := EventRecord{
		Step:      0,
		EventType: "call",
		Service:   "my-svc",
		Op:        "my-op",
		Request:   `{"req":1}`,
		Response:  `{"res":2}`,
	}
	if err := store.AppendEventHistory(ctx, wfID, rec); err != nil {
		t.Fatalf("AppendEventHistory: %v", err)
	}

	// AppendEventHistoryBatch with additional records.
	recs := []EventRecord{
		{Step: 1, EventType: "call", Service: "svc2", Op: "op2", Request: `{"r":1}`, Response: `{}`},
		{Step: 2, EventType: "sleep", DurationMs: 5000},
	}
	if err := store.AppendEventHistoryBatch(ctx, wfID, recs); err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}

	// LoadEventHistory.
	history, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("LoadEventHistory returned %d events, want 3", len(history))
	}
	if history[0].Step != 0 || history[0].Service != "my-svc" {
		t.Errorf("event 0: step=%d service=%s", history[0].Step, history[0].Service)
	}

	// LoadEventHistoryPaginated.
	page, err := store.LoadEventHistoryPaginated(ctx, wfID, 1, 1)
	if err != nil {
		t.Fatalf("LoadEventHistoryPaginated: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("expected 1 event in page, got %d", len(page))
	}
	if page[0].Step != 1 {
		t.Errorf("expected step 1, got step %d", page[0].Step)
	}

	// CountEventHistory.
	count, err := store.CountEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("CountEventHistory: %v", err)
	}
	if count != 3 {
		t.Errorf("CountEventHistory = %d, want 3", count)
	}

	// Count for unknown workflow.
	unknownCount, err := store.CountEventHistory(ctx, "no-such-workflow")
	if err != nil {
		t.Fatalf("CountEventHistory unknown: %v", err)
	}
	if unknownCount != 0 {
		t.Errorf("unknown workflow count = %d, want 0", unknownCount)
	}
}

func TestMSSQLIntegration_VerifyWorkflowEvents(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "verify-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, 'verify-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default', '00000000-0000-0000-0000-000000000000')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	rec := EventRecord{Step: 0, EventType: "call", Service: "s", Op: "o"}
	if err := store.AppendEventHistory(ctx, wfID, rec); err != nil {
		t.Fatalf("AppendEventHistory: %v", err)
	}

	// Verify should succeed (checksum may be nil in pre-migration state).
	err = store.VerifyWorkflowEvents(ctx, wfID)
	if err != nil {
		t.Fatalf("VerifyWorkflowEvents: %v", err)
	}

	// Non-existent workflow returns nil (no checksums stored).
	err = store.VerifyWorkflowEvents(ctx, "no-such-workflow")
	if err != nil {
		t.Fatalf("VerifyWorkflowEvents no-such: %v", err)
	}
}

func TestMSSQLIntegration_StreamEventHistory(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "stream-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, 'stream-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default', '00000000-0000-0000-0000-000000000000')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	recs := []EventRecord{
		{Step: 0, EventType: "call", Service: "a", Op: "1"},
		{Step: 1, EventType: "call", Service: "b", Op: "2"},
		{Step: 2, EventType: "call", Service: "c", Op: "3"},
	}
	if err := store.AppendEventHistoryBatch(ctx, wfID, recs); err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}

	eventCh, errCh := store.StreamEventHistory(ctx, wfID, 2)
	var streamed []EventRecord
	for ev := range eventCh {
		streamed = append(streamed, ev)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("StreamEventHistory error: %v", err)
	}
	if len(streamed) != 3 {
		t.Fatalf("StreamEventHistory returned %d events, want 3", len(streamed))
	}
}

// ---------------------------------------------------------------------------
// Signal Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_Signals(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "sig-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'sig-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// Deliver a signal.
	if err := store.DeliverSignal(ctx, wfID, "my-signal", `{"hello":"world"}`); err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	// Poll the signal.
	payload, found, err := store.PollSignal(ctx, wfID, "my-signal")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if !found {
		t.Fatal("PollSignal: signal not found")
	}
	if payload != `{"hello":"world"}` {
		t.Errorf("PollSignal payload = %s, want {\"hello\":\"world\"}", payload)
	}

	// PollAndClaim (atomically claims the signal).
	payload2, found2, err := store.PollAndClaimSignal(ctx, wfID, "my-signal")
	if err != nil {
		t.Fatalf("PollAndClaimSignal: %v", err)
	}
	if !found2 {
		t.Fatal("PollAndClaimSignal: signal not found")
	}
	if payload2 != `{"hello":"world"}` {
		t.Errorf("PollAndClaimSignal payload = %s", payload2)
	}

	// Second Poll should NOT find it (PollAndClaimSignal deletes the signal).
	_, found3, err := store.PollSignal(ctx, wfID, "my-signal")
	if err != nil {
		t.Fatalf("PollSignal 2nd: %v", err)
	}
	if found3 {
		t.Fatal("PollSignal 2nd: expected signal not found after PollAndClaim")
	}

	// Poll for non-existent signal.
	_, found4, err := store.PollSignal(ctx, wfID, "no-such-signal")
	if err != nil {
		t.Fatalf("PollSignal no-such: %v", err)
	}
	if found4 {
		t.Fatal("PollSignal returned found for non-existent signal")
	}
}

// ---------------------------------------------------------------------------
// Cancellation Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_Cancellation(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "cancel-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'cancel-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// Initially not cancelled.
	cancelled, reason, err := store.CheckCancellation(ctx, wfID)
	if err != nil {
		t.Fatalf("CheckCancellation initial: %v", err)
	}
	if cancelled {
		t.Fatal("expected not cancelled initially")
	}
	if reason != "" {
		t.Errorf("expected empty reason, got %s", reason)
	}

	// Request cancellation.
	if err := store.RequestCancellation(ctx, wfID, "user requested"); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}

	// Now cancelled.
	cancelled, reason, err = store.CheckCancellation(ctx, wfID)
	if err != nil {
		t.Fatalf("CheckCancellation after: %v", err)
	}
	if !cancelled {
		t.Fatal("expected cancelled after RequestCancellation")
	}
	if reason != "user requested" {
		t.Errorf("reason = %s, want 'user requested'", reason)
	}

	// PollCancellation should also detect it.
	cancelled2, reason2, err := store.PollCancellation(ctx, wfID)
	if err != nil {
		t.Fatalf("PollCancellation: %v", err)
	}
	if !cancelled2 {
		t.Fatal("PollCancellation returned false")
	}
	if reason2 != "user requested" {
		t.Errorf("PollCancellation reason = %s", reason2)
	}
}

// ---------------------------------------------------------------------------
// Schedule Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_Schedules(t *testing.T) {
	store, _ := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "sched-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// Create a schedule.
	sch := Schedule{
		Name:           "test-schedule-1",
		DefName:        "sched-wf",
		EntryPoint:     "main",
		CronExpression: "*/5 * * * *",
		Input:          json.RawMessage(`{"type":"test"}`),
		Enabled:        true,
		NextRunAt:      time.Now().Add(-1 * time.Hour),
	}
	if err := store.CreateSchedule(ctx, sch); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	// List schedules.
	schedules, err := store.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(schedules) != 1 {
		t.Fatalf("ListSchedules returned %d, want 1", len(schedules))
	}
	if schedules[0].Name != "test-schedule-1" {
		t.Errorf("name = %s", schedules[0].Name)
	}
	if schedules[0].CronExpression != "*/5 * * * *" {
		t.Errorf("cron = %s", schedules[0].CronExpression)
	}
	if !schedules[0].Enabled {
		t.Error("schedule should be enabled")
	}

	// GetDueSchedules.
	due, err := store.GetDueSchedules(ctx)
	if err != nil {
		t.Fatalf("GetDueSchedules: %v", err)
	}
	if len(due) < 1 {
		t.Fatal("expected at least 1 due schedule")
	}

	// UpdateScheduleNextRun.
	futureRun := time.Now().Add(1 * time.Hour)
	if err := store.UpdateScheduleNextRun(ctx, "test-schedule-1", futureRun); err != nil {
		t.Fatalf("UpdateScheduleNextRun: %v", err)
	}

	// Schedule should no longer be due.
	due2, err := store.GetDueSchedules(ctx)
	if err != nil {
		t.Fatalf("GetDueSchedules 2nd: %v", err)
	}
	for _, s := range due2 {
		if s.Name == "test-schedule-1" {
			t.Error("schedule should not be due after next_run update")
		}
	}

	// SetScheduleEnabled to false.
	if err := store.SetScheduleEnabled(ctx, "test-schedule-1", false); err != nil {
		t.Fatalf("SetScheduleEnabled: %v", err)
	}

	schedules2, err := store.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("ListSchedules after disable: %v", err)
	}
	if len(schedules2) != 1 {
		t.Fatalf("expected 1 schedule, got %d", len(schedules2))
	}
	if schedules2[0].Enabled {
		t.Error("schedule should be disabled")
	}

	// DeleteSchedule.
	if err := store.DeleteSchedule(ctx, "test-schedule-1"); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}

	schedules3, err := store.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("ListSchedules after delete: %v", err)
	}
	if len(schedules3) != 0 {
		t.Errorf("expected 0 schedules after delete, got %d", len(schedules3))
	}
}

// ---------------------------------------------------------------------------
// Promise Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_Promises(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "prom-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'prom-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// Create a promise.
	if err := store.CreatePromise(ctx, wfID, "my-promise", "promise-001"); err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}

	// GetPromise.
	status, result, errMsg, err := store.GetPromise(ctx, wfID, "promise-001")
	if err != nil {
		t.Fatalf("GetPromise: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %s, want pending", status)
	}
	if result != "" {
		t.Errorf("result = %s, want empty", result)
	}
	if errMsg != "" {
		t.Errorf("errMsg = %s, want empty", errMsg)
	}

	// ResolvePromise.
	if err := store.ResolvePromise(ctx, wfID, "promise-001", `{"resolved":true}`); err != nil {
		t.Fatalf("ResolvePromise: %v", err)
	}

	status2, result2, _, err := store.GetPromise(ctx, wfID, "promise-001")
	if err != nil {
		t.Fatalf("GetPromise after resolve: %v", err)
	}
	if status2 != "resolved" {
		t.Errorf("status = %s, want resolved", status2)
	}
	if result2 != `{"resolved":true}` {
		t.Errorf("result = %s", result2)
	}

	// ListPromises.
	promises, err := store.ListPromises(ctx, wfID)
	if err != nil {
		t.Fatalf("ListPromises: %v", err)
	}
	if len(promises) != 1 {
		t.Fatalf("expected 1 promise, got %d", len(promises))
	}
	if promises[0].PromiseID != "promise-001" {
		t.Errorf("PromiseID = %s", promises[0].PromiseID)
	}
	if promises[0].PromiseName != "my-promise" {
		t.Errorf("PromiseName = %s", promises[0].PromiseName)
	}

	// Create another promise and reject it.
	promiseID2 := "promise-002"
	if err := store.CreatePromise(ctx, wfID, "reject-promise", promiseID2); err != nil {
		t.Fatalf("CreatePromise 2nd: %v", err)
	}
	if err := store.RejectPromise(ctx, wfID, promiseID2, "rejected because"); err != nil {
		t.Fatalf("RejectPromise: %v", err)
	}

	status3, _, errMsg3, err := store.GetPromise(ctx, wfID, promiseID2)
	if err != nil {
		t.Fatalf("GetPromise rejected: %v", err)
	}
	if status3 != "rejected" {
		t.Errorf("status = %s, want rejected", status3)
	}
	if errMsg3 != "rejected because" {
		t.Errorf("errMsg = %s", errMsg3)
	}
}

// ---------------------------------------------------------------------------
// Concurrency Key Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_ConcurrencyKeys(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "ck-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'ck-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// Acquire a concurrency key.
	acquired, err := store.AcquireConcurrencyKey(ctx, "resource-a", wfID, 5*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Fatal("expected to acquire key")
	}

	// Second acquire of same key should fail.
	acquired2, err := store.AcquireConcurrencyKey(ctx, "resource-a", wfID, 5*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey 2nd: %v", err)
	}
	if acquired2 {
		t.Fatal("expected second acquire to return false")
	}

	// GetConcurrencyKeyCount.
	count, err := store.GetConcurrencyKeyCount(ctx, wfID)
	if err != nil {
		t.Fatalf("GetConcurrencyKeyCount: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}

	// Acquire a second key.
	acquired3, err := store.AcquireConcurrencyKey(ctx, "resource-b", wfID, 5*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey b: %v", err)
	}
	if !acquired3 {
		t.Fatal("expected to acquire key b")
	}

	count2, err := store.GetConcurrencyKeyCount(ctx, wfID)
	if err != nil {
		t.Fatalf("GetConcurrencyKeyCount b: %v", err)
	}
	if count2 != 2 {
		t.Errorf("count after second key = %d, want 2", count2)
	}

	// Release one key.
	if err := store.ReleaseConcurrencyKey(ctx, "resource-a"); err != nil {
		t.Fatalf("ReleaseConcurrencyKey: %v", err)
	}

	// Now resource-a can be acquired again.
	acquired4, err := store.AcquireConcurrencyKey(ctx, "resource-a", wfID, 5*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey after release: %v", err)
	}
	if !acquired4 {
		t.Fatal("expected to acquire key after release")
	}

	// Release all keys for the workflow.
	if err := store.ReleaseWorkflowConcurrencyKeys(ctx, wfID); err != nil {
		t.Fatalf("ReleaseWorkflowConcurrencyKeys: %v", err)
	}

	count3, err := store.GetConcurrencyKeyCount(ctx, wfID)
	if err != nil {
		t.Fatalf("GetConcurrencyKeyCount after release all: %v", err)
	}
	if count3 != 0 {
		t.Errorf("count after release all = %d, want 0", count3)
	}
}

func TestMSSQLIntegration_ReapExpiredConcurrencyKeys(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "reap-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, 'reap-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default', '00000000-0000-0000-0000-000000000000')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// Insert an already-expired concurrency key via SQL (negative TTL).
	hash := sha256Of("expired-key")
	_, err = db.ExecContext(ctx, `
		INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
		VALUES (@p1, 'expired-key', @p2, DATEADD(HOUR, -1, SYSUTCDATETIME()), '00000000-0000-0000-0000-000000000000')
	`, hash, wfID)
	if err != nil {
		t.Fatalf("insert expired concurrency key: %v", err)
	}

	// Reap should delete the expired key.
	n, err := store.ReapExpiredConcurrencyKeys(ctx)
	if err != nil {
		t.Fatalf("ReapExpiredConcurrencyKeys: %v", err)
	}
	if n != 1 {
		t.Errorf("reaped %d keys, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Child Workflow Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_ChildWorkflows(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "child-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	parentID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'child-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, parentID)
	if err != nil {
		t.Fatalf("insert parent workflow: %v", err)
	}

	// Start child workflow.
	childID, err := store.StartChildWorkflow(ctx, parentID, "child-wf", `{"child":true}`, 1, "ABANDON", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}
	if childID == "" {
		t.Fatal("expected non-empty child ID")
	}

	// GetChildCount.
	count, err := store.GetChildCount(ctx, parentID)
	if err != nil {
		t.Fatalf("GetChildCount: %v", err)
	}
	if count != 1 {
		t.Errorf("child count = %d, want 1", count)
	}

	// GetChildResult should not be completed yet.
	_, completed, err := store.GetChildResult(ctx, childID)
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if completed {
		t.Fatal("child should not be completed yet")
	}

	// Complete the child via SQL and verify GetChildResult.
	_, err = db.ExecContext(ctx, `
		UPDATE workflow_instances SET status = 'done', result = @p2, completed_at = SYSUTCDATETIME()
		WHERE id = @p1
	`, childID, `{"result":"child-done"}`)
	if err != nil {
		t.Fatalf("complete child via SQL: %v", err)
	}

	result, completed, err := store.GetChildResult(ctx, childID)
	if err != nil {
		t.Fatalf("GetChildResult after complete: %v", err)
	}
	if !completed {
		t.Fatal("child should be completed now")
	}
	if result != `{"result":"child-done"}` {
		t.Errorf("child result = %s", result)
	}
}

func TestMSSQLIntegration_StartChildWorkflowAtomic(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "atomic-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	parentID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'atomic-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, parentID)
	if err != nil {
		t.Fatalf("insert parent workflow: %v", err)
	}

	childID := uuid.New().String()
	event := EventRecord{
		Step:       0,
		EventType:  "child_workflow",
		ChildName:  "atomic-wf",
		ChildInput: `{"child":true}`,
		RunID:      childID,
	}

	returnedID, err := store.StartChildWorkflowAtomic(ctx, childID, parentID, "atomic-wf", `{"child":true}`, 1, "ABANDON", event, 0)
	if err != nil {
		t.Fatalf("StartChildWorkflowAtomic: %v", err)
	}
	if returnedID != childID {
		t.Errorf("returnedID = %s, want %s", returnedID, childID)
	}

	// Verify the child workflow exists.
	var childStatus string
	err = db.QueryRowContext(ctx,
		`SELECT status FROM workflow_instances WHERE id = @p1`, childID).Scan(&childStatus)
	if err != nil {
		t.Fatalf("query child status: %v", err)
	}
	if childStatus != "ready" {
		t.Errorf("child status = %s, want ready", childStatus)
	}

	// Verify parent-child link.
	var parentWFID string
	err = db.QueryRowContext(ctx,
		`SELECT ISNULL(parent_workflow_id, '') FROM workflow_instances WHERE id = @p1`, childID).Scan(&parentWFID)
	if err != nil {
		t.Fatalf("query parent_workflow_id: %v", err)
	}
	if parentWFID != parentID {
		t.Errorf("parent_workflow_id = %s, want %s", parentWFID, parentID)
	}

	// Verify event was appended.
	var eventCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_history WHERE workflow_id = @p1`, parentID).Scan(&eventCount)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 1 {
		t.Errorf("event count = %d, want 1", eventCount)
	}
}

// ---------------------------------------------------------------------------
// Sticky Worker Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_StickyWorker(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "sticky-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'sticky-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// UpdateStickyWorker.
	if err := store.UpdateStickyWorker(ctx, wfID, "sticky-worker-A"); err != nil {
		t.Fatalf("UpdateStickyWorker: %v", err)
	}

	var workerID string
	err = db.QueryRowContext(ctx,
		`SELECT ISNULL(sticky_worker_id, '') FROM workflow_instances WHERE id = @p1`, wfID).Scan(&workerID)
	if err != nil {
		t.Fatalf("query sticky_worker_id: %v", err)
	}
	if workerID != "sticky-worker-A" {
		t.Errorf("sticky_worker_id = %s", workerID)
	}

	// ClearStickyWorker.
	if err := store.ClearStickyWorker(ctx, wfID); err != nil {
		t.Fatalf("ClearStickyWorker: %v", err)
	}

	err = db.QueryRowContext(ctx,
		`SELECT ISNULL(sticky_worker_id, '') FROM workflow_instances WHERE id = @p1`, wfID).Scan(&workerID)
	if err != nil {
		t.Fatalf("query cleared sticky_worker_id: %v", err)
	}
	if workerID != "" {
		t.Errorf("sticky_worker_id after clear = %s, want empty", workerID)
	}
}

// ---------------------------------------------------------------------------
// Workflow Query Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_ListAndGetWorkflows(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "list-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// Insert two workflow instances in different states.
	wfID1 := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, priority, tenant_id)
		VALUES (@p1, 'list-wf', 1, 'ready', SYSUTCDATETIME(), '{"k":"v1"}', 'default', 1, '00000000-0000-0000-0000-000000000000')
	`, wfID1)
	if err != nil {
		t.Fatalf("insert wf1: %v", err)
	}

	wfID2 := uuid.New().String()
	_, err = db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, priority, tenant_id)
		VALUES (@p1, 'list-wf', 1, 'running', SYSUTCDATETIME(), '{"k":"v2"}', 'default', 0, '00000000-0000-0000-0000-000000000000')
	`, wfID2)
	if err != nil {
		t.Fatalf("insert wf2: %v", err)
	}

	// ListWorkflows with no filter.
	all, err := store.ListWorkflows(ctx, WorkflowFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListWorkflows returned %d, want 2", len(all))
	}

	// Filter by status.
	running, err := store.ListWorkflows(ctx, WorkflowFilter{Status: "running", Limit: 100})
	if err != nil {
		t.Fatalf("ListWorkflows running: %v", err)
	}
	if len(running) != 1 {
		t.Fatalf("expected 1 running, got %d", len(running))
	}
	if running[0].ID != wfID2 {
		t.Errorf("running wf id = %s", running[0].ID)
	}

	// GetWorkflowByID.
	wf, err := store.GetWorkflowByID(ctx, wfID1)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf == nil {
		t.Fatal("GetWorkflowByID returned nil")
	}
	if wf.ID != wfID1 {
		t.Errorf("GetWorkflowByID id = %s", wf.ID)
	}
	if wf.Status != "ready" {
		t.Errorf("status = %s, want ready", wf.Status)
	}

	// GetWorkflowByID with non-existent ID.
	missing, err := store.GetWorkflowByID(ctx, "no-such-workflow")
	if err != nil {
		t.Fatalf("GetWorkflowByID missing: %v", err)
	}
	if missing != nil {
		t.Fatal("expected nil for missing workflow")
	}
}

func TestMSSQLIntegration_GetQueryState(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "qs-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	qsJSON := `{"mykey":"myvalue"}`
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, query_state)
		VALUES (@p1, 'qs-wf', 1, 'running', '{}', 'default', @p2)
	`, wfID, qsJSON)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// Get query state.
	val, err := store.GetQueryState(ctx, wfID, "mykey")
	if err != nil {
		t.Fatalf("GetQueryState: %v", err)
	}
	if val != "myvalue" {
		t.Errorf("query state = %s, want myvalue", val)
	}

	// Non-existent key returns empty string (JSON_VALUE yields SQL NULL).
	val, err = store.GetQueryState(ctx, wfID, "nosuchkey")
	if err != nil {
		t.Fatalf("GetQueryState (non-existent key): %v", err)
	}
	if val != "" {
		t.Errorf("expected empty string for non-existent key, got %q", val)
	}
}

// ---------------------------------------------------------------------------
// Update Request Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_UpdateRequests(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "upd-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'upd-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// Create an update request.
	if err := store.CreateUpdateRequest(ctx, wfID, "update-name-1", `{"data":"payload"}`, "promise-u1"); err != nil {
		t.Fatalf("CreateUpdateRequest: %v", err)
	}

	// GetPendingUpdateRequests.
	pending, err := store.GetPendingUpdateRequests(ctx, wfID)
	if err != nil {
		t.Fatalf("GetPendingUpdateRequests: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending update, got %d", len(pending))
	}
	if pending[0].UpdateName != "update-name-1" {
		t.Errorf("UpdateName = %s", pending[0].UpdateName)
	}
	if pending[0].Payload != `{"data":"payload"}` {
		t.Errorf("Payload = %s", pending[0].Payload)
	}
	if pending[0].Status != "pending" {
		t.Errorf("Status = %s, want pending", pending[0].Status)
	}
	if pending[0].PromiseID != "promise-u1" {
		t.Errorf("PromiseID = %s", pending[0].PromiseID)
	}

	// CompleteUpdateRequest with result.
	if err := store.CompleteUpdateRequest(ctx, wfID, "update-name-1", `{"completed":true}`, ""); err != nil {
		t.Fatalf("CompleteUpdateRequest: %v", err)
	}

	// No more pending.
	pending2, err := store.GetPendingUpdateRequests(ctx, wfID)
	if err != nil {
		t.Fatalf("GetPendingUpdateRequests 2nd: %v", err)
	}
	if len(pending2) != 0 {
		t.Errorf("expected 0 pending after completion, got %d", len(pending2))
	}
}

// ---------------------------------------------------------------------------
// Version Management Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_VersionManagement(t *testing.T) {
	store, _ := setupMSSQLIntegrationTest(t)
	ctx := context.Background()

	// Deploy multiple versions of the same workflow.
	wasmV1 := []byte{0x00, 0x61, 0x73, 0x6d}
	wasmV2 := []byte{0x00, 0x61, 0x73, 0x6d, 0x01}

	deployWorkflowDef(t, store, "ver-wf", 1, wasmV1)
	deployWorkflowDef(t, store, "ver-wf", 2, wasmV2)

	// ListVersions.
	versions, err := store.ListVersions(ctx, "ver-wf")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}

	// ResolveLatestVersion.
	latest, err := store.ResolveLatestVersion(ctx, "ver-wf")
	if err != nil {
		t.Fatalf("ResolveLatestVersion: %v", err)
	}
	if latest != 2 {
		t.Errorf("latest version = %d, want 2", latest)
	}

	// ValidateVersion.
	valid, err := store.ValidateVersion(ctx, "ver-wf", 1)
	if err != nil {
		t.Fatalf("ValidateVersion: %v", err)
	}
	if !valid {
		t.Fatal("version 1 should be valid")
	}

	valid2, err := store.ValidateVersion(ctx, "ver-wf", 99)
	if err != nil {
		t.Fatalf("ValidateVersion 99: %v", err)
	}
	if valid2 {
		t.Fatal("version 99 should not be valid")
	}

	// CountActiveInstances (should be 0).
	count, err := store.CountActiveInstances(ctx, "ver-wf", 1)
	if err != nil {
		t.Fatalf("CountActiveInstances: %v", err)
	}
	if count != 0 {
		t.Errorf("active instances = %d, want 0", count)
	}

	// MarkVersionDeprecated.
	if err := store.MarkVersionDeprecated(ctx, "ver-wf", 1, true); err != nil {
		t.Fatalf("MarkVersionDeprecated: %v", err)
	}

	// ValidateVersion for deprecated version.
	valid3, err := store.ValidateVersion(ctx, "ver-wf", 1)
	if err != nil {
		t.Fatalf("ValidateVersion deprecated: %v", err)
	}
	if valid3 {
		t.Fatal("deprecated version should not be valid")
	}

	// ListWorkflowDefs.
	defs, err := store.ListWorkflowDefs(ctx, "ver-wf")
	if err != nil {
		t.Fatalf("ListWorkflowDefs: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected 2 defs, got %d", len(defs))
	}
}

// ---------------------------------------------------------------------------
// WASM / Config Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_WASMAndConfig(t *testing.T) {
	store, _ := setupMSSQLIntegrationTest(t)
	ctx := context.Background()

	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x02, 0x03, 0x04}
	deployWorkflowDef(t, store, "wasm-wf", 1, wasmBytes)

	// LoadWASM.
	loaded, err := store.LoadWASM(ctx, "wasm-wf", 1)
	if err != nil {
		t.Fatalf("LoadWASM: %v", err)
	}
	if string(loaded) != string(wasmBytes) {
		t.Errorf("loaded wasm = %v, want %v", loaded, wasmBytes)
	}

	// GetWASMLength.
	length, err := store.GetWASMLength(ctx, "wasm-wf", 1)
	if err != nil {
		t.Fatalf("GetWASMLength: %v", err)
	}
	if length != int64(len(wasmBytes)) {
		t.Errorf("wasm length = %d, want %d", length, len(wasmBytes))
	}

	// LoadWASM for non-existent def.
	_, err = store.LoadWASM(ctx, "no-such-def", 1)
	if err == nil {
		t.Fatal("expected error for non-existent WASM")
	}

	// LoadWorkflowConfig (default max_history_length = 0).
	maxLen, err := store.LoadWorkflowConfig(ctx, "wasm-wf", 1)
	if err != nil {
		t.Fatalf("LoadWorkflowConfig: %v", err)
	}
	if maxLen != 0 {
		t.Errorf("max_history_length = %d, want 0", maxLen)
	}

	// LoadDAGSpec (default nil).
	dagSpec, err := store.LoadDAGSpec(ctx, "wasm-wf", 1)
	if err != nil {
		t.Fatalf("LoadDAGSpec: %v", err)
	}
	if dagSpec != nil {
		t.Errorf("dag_spec = %v, want nil", dagSpec)
	}
}

// ---------------------------------------------------------------------------
// Operational Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_TraceWorkflow(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "trace-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'trace-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	if err := store.TraceWorkflow(ctx, wfID, "00-abc123-00"); err != nil {
		t.Fatalf("TraceWorkflow: %v", err)
	}

	var traceID string
	err = db.QueryRowContext(ctx,
		`SELECT ISNULL(trace_id, '') FROM workflow_instances WHERE id = @p1`, wfID).Scan(&traceID)
	if err != nil {
		t.Fatalf("query trace_id: %v", err)
	}
	if traceID != "00-abc123-00" {
		t.Errorf("trace_id = %s", traceID)
	}
}

func TestMSSQLIntegration_QueueDepth(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "qd-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// One ready workflow with past wake time.
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'qd-wf', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default')
	`, uuid.New().String())
	if err != nil {
		t.Fatalf("insert workflow 1: %v", err)
	}

	// A running workflow should not be counted.
	_, err = db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'qd-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, uuid.New().String())
	if err != nil {
		t.Fatalf("insert workflow 2: %v", err)
	}

	depth, err := store.QueueDepth(ctx)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 1 {
		t.Errorf("QueueDepth = %d, want 1", depth)
	}
}

func TestMSSQLIntegration_EventCount(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "ec-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'ec-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// Append two events.
	rec1 := EventRecord{Step: 0, EventType: "call", Service: "s", Op: "o"}
	rec2 := EventRecord{Step: 1, EventType: "call", Service: "s", Op: "o2"}
	if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{rec1, rec2}); err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}

	count, err := store.GetEventCount(ctx, wfID)
	if err != nil {
		t.Fatalf("GetEventCount: %v", err)
	}
	if count != 2 {
		t.Errorf("event count = %d, want 2", count)
	}
}

func TestMSSQLIntegration_GetAllowedSignalCallers(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "asc-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'asc-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// Initially no allowed signals.
	callers, err := store.GetAllowedSignalCallers(ctx, wfID)
	if err != nil {
		t.Fatalf("GetAllowedSignalCallers: %v", err)
	}
	if callers != nil {
		t.Errorf("callers = %v, want nil", callers)
	}

	// Set allowed_signals via SQL and test again.
	_, err = db.ExecContext(ctx,
		`UPDATE workflow_instances SET allowed_signals = @p2 WHERE id = @p1`,
		wfID, `["signal-a","signal-b"]`)
	if err != nil {
		t.Fatalf("set allowed_signals: %v", err)
	}

	callers2, err := store.GetAllowedSignalCallers(ctx, wfID)
	if err != nil {
		t.Fatalf("GetAllowedSignalCallers after set: %v", err)
	}
	if len(callers2) != 2 {
		t.Fatalf("expected 2 callers, got %d", len(callers2))
	}
	if callers2[0] != "signal-a" || callers2[1] != "signal-b" {
		t.Errorf("callers = %v", callers2)
	}
}

// ---------------------------------------------------------------------------
// Cleanup and Reaping Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_ReapStaleInstances(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "reap-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// Insert a stale running workflow (heartbeat in the past).
	staleID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, heartbeat_at, next_wake_at, input, assigned_to, task_queue, tenant_id)
		VALUES (@p1, 'reap-wf', 1, 'running', DATEADD(HOUR, -2, SYSUTCDATETIME()), SYSUTCDATETIME(), '{}', 'stale-worker', 'default', '00000000-0000-0000-0000-000000000000')
	`, staleID)
	if err != nil {
		t.Fatalf("insert stale instance: %v", err)
	}

	// Insert a non-stale running workflow (recent heartbeat).
	freshID := uuid.New().String()
	_, err = db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, heartbeat_at, next_wake_at, input, assigned_to, task_queue, tenant_id)
		VALUES (@p1, 'reap-wf', 1, 'running', SYSUTCDATETIME(), SYSUTCDATETIME(), '{}', 'fresh-worker', 'default', '00000000-0000-0000-0000-000000000000')
	`, freshID)
	if err != nil {
		t.Fatalf("insert fresh instance: %v", err)
	}

	// Reap with 1-hour timeout should catch the stale one.
	reaped, err := store.ReapStaleInstances(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("ReapStaleInstances: %v", err)
	}
	if reaped != 1 {
		t.Errorf("reaped %d, want 1", reaped)
	}
}

func TestMSSQLIntegration_DeleteExpiredEvents(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "exp-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// Create a completed workflow with events.
	doneID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, completed_at, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, 'exp-wf', 1, 'done', DATEADD(DAY, -10, SYSUTCDATETIME()), SYSUTCDATETIME(), '{}', 'default', '00000000-0000-0000-0000-000000000000')
	`, doneID)
	if err != nil {
		t.Fatalf("insert done instance: %v", err)
	}

	// Insert events for this workflow.
	for i := 0; i < 3; i++ {
		_, err = db.ExecContext(ctx, `
			INSERT INTO event_history (workflow_id, step, event_type, service, operation)
			VALUES (@p1, @p2, 'call', 'svc', 'op')
		`, doneID, i)
		if err != nil {
			t.Fatalf("insert event %d: %v", i, err)
		}
	}

	// Also create a non-terminal workflow with events (should NOT be deleted).
	runningID := uuid.New().String()
	_, err = db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'exp-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, runningID)
	if err != nil {
		t.Fatalf("insert running instance: %v", err)
	}
	for i := 0; i < 2; i++ {
		_, err = db.ExecContext(ctx, `
			INSERT INTO event_history (workflow_id, step, event_type, service, operation)
			VALUES (@p1, @p2, 'call', 'svc', 'op')
		`, runningID, i)
		if err != nil {
			t.Fatalf("insert running event %d: %v", i, err)
		}
	}

	// Delete events older than 1 day.
	deleted, err := store.DeleteExpiredEvents(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteExpiredEvents: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted %d events, want 3", deleted)
	}

	// Verify done workflow events are gone.
	var doneEventCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_history WHERE workflow_id = @p1`, doneID).Scan(&doneEventCount)
	if err != nil {
		t.Fatalf("count done events: %v", err)
	}
	if doneEventCount != 0 {
		t.Errorf("done workflow still has %d events", doneEventCount)
	}

	// Verify running workflow events remain.
	var runningEventCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_history WHERE workflow_id = @p1`, runningID).Scan(&runningEventCount)
	if err != nil {
		t.Fatalf("count running events: %v", err)
	}
	if runningEventCount != 2 {
		t.Errorf("running workflow has %d events, want 2", runningEventCount)
	}
}

func TestMSSQLIntegration_DeleteDeadLetteredWorkflows(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "dlq-del-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// Insert a dead_lettered workflow from 30 days ago.
	oldID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, completed_at, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, 'dlq-del-wf', 1, 'dead_lettered', DATEADD(DAY, -30, SYSUTCDATETIME()), SYSUTCDATETIME(), '{}', 'default', '00000000-0000-0000-0000-000000000000')
	`, oldID)
	if err != nil {
		t.Fatalf("insert old dead_lettered: %v", err)
	}

	// Insert a recent dead_lettered workflow (should NOT be deleted).
	recentID := uuid.New().String()
	_, err = db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, completed_at, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, 'dlq-del-wf', 1, 'dead_lettered', SYSUTCDATETIME(), SYSUTCDATETIME(), '{}', 'default', '00000000-0000-0000-0000-000000000000')
	`, recentID)
	if err != nil {
		t.Fatalf("insert recent dead_lettered: %v", err)
	}

	// Delete dead lettered workflows older than 7 days.
	deleted, err := store.DeleteDeadLetteredWorkflows(ctx, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteDeadLetteredWorkflows: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted %d workflows, want 1", deleted)
	}

	// Verify old one is gone.
	var oldExists int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_instances WHERE id = @p1`, oldID).Scan(&oldExists)
	if err != nil {
		t.Fatalf("count old instance: %v", err)
	}
	if oldExists != 0 {
		t.Error("old dead_lettered workflow should have been deleted")
	}

	// Verify recent one remains.
	var recentExists int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_instances WHERE id = @p1`, recentID).Scan(&recentExists)
	if err != nil {
		t.Fatalf("count recent instance: %v", err)
	}
	if recentExists != 1 {
		t.Error("recent dead_lettered workflow should remain")
	}
}

func TestMSSQLIntegration_TerminateWorkflow(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "term-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'term-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	if err := store.TerminateWorkflow(ctx, wfID, "forced termination"); err != nil {
		t.Fatalf("TerminateWorkflow: %v", err)
	}

	var status, reason string
	err = db.QueryRowContext(ctx,
		`SELECT status, ISNULL(error_msg, '') FROM workflow_instances WHERE id = @p1`, wfID).
		Scan(&status, &reason)
	if err != nil {
		t.Fatalf("query terminated: %v", err)
	}
	if status != "terminated" {
		t.Errorf("status = %s, want terminated", status)
	}
	if reason != "forced termination" {
		t.Errorf("reason = %s", reason)
	}
}

// ---------------------------------------------------------------------------
// Compaction Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_Compaction(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "comp-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'comp-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// Append some events.
	for i := 0; i < 5; i++ {
		rec := EventRecord{Step: i, EventType: "call", Service: "s", Op: "op"}
		if err := store.AppendEventHistory(ctx, wfID, rec); err != nil {
			t.Fatalf("AppendEventHistory step %d: %v", i, err)
		}
	}

	// GetCompactionCandidates with threshold=1 should find this workflow.
	candidates, err := store.GetCompactionCandidates(ctx, 1, 100)
	if err != nil {
		t.Fatalf("GetCompactionCandidates: %v", err)
	}
	if len(candidates) < 1 {
		t.Fatal("expected at least 1 compaction candidate")
	}
	found := false
	for _, c := range candidates {
		if c == wfID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("workflow %s not found in compaction candidates %v", wfID, candidates)
	}

	// CompactHistory.
	compactionState := []byte(`{"version":1,"compacted_step":3,"events":[]}`)
	if err := store.CompactHistory(ctx, wfID, compactionState, 3, 5); err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}

	// LoadCompactionState.
	state, err := store.LoadCompactionState(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadCompactionState: %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil compaction state")
	}
	if state.Version != 1 {
		t.Errorf("compaction version = %d, want 1", state.Version)
	}
	if state.CompactedStep != 3 {
		t.Errorf("compacted_step = %d, want 3", state.CompactedStep)
	}

	// Events with step < keepStep (5) should be gone.
	var remaining int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_history WHERE workflow_id = @p1`, wfID).Scan(&remaining)
	if err != nil {
		t.Fatalf("count remaining events: %v", err)
	}
	if remaining != 0 {
		t.Errorf("expected 0 events after compact with keepStep=5, got %d", remaining)
	}

	// Compact with keepStep=2 should leave steps 2,3,4.
	wfID2 := uuid.New().String()
	_, err = db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'comp-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default')
	`, wfID2)
	if err != nil {
		t.Fatalf("insert workflow_instance 2: %v", err)
	}
	for i := 0; i < 5; i++ {
		rec := EventRecord{Step: i, EventType: "call", Service: "s", Op: "op"}
		if err := store.AppendEventHistory(ctx, wfID2, rec); err != nil {
			t.Fatalf("AppendEventHistory step %d: %v", i, err)
		}
	}
	if err := store.CompactHistory(ctx, wfID2, compactionState, 2, 2); err != nil {
		t.Fatalf("CompactHistory keepStep=2: %v", err)
	}
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_history WHERE workflow_id = @p1`, wfID2).Scan(&remaining)
	if err != nil {
		t.Fatalf("count remaining events 2: %v", err)
	}
	if remaining != 3 {
		t.Errorf("expected 3 events after compact with keepStep=2, got %d", remaining)
	}
}

// ---------------------------------------------------------------------------
// Memory Stats Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_MemoryStats(t *testing.T) {
	store, _ := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "mem-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// RecordMemorySample.
	if err := store.RecordWorkflowMemorySample(ctx, "mem-wf", 1024); err != nil {
		t.Fatalf("RecordWorkflowMemorySample: %v", err)
	}
	if err := store.RecordWorkflowMemorySample(ctx, "mem-wf", 2048); err != nil {
		t.Fatalf("RecordWorkflowMemorySample 2: %v", err)
	}

	// LoadMemoryEstimates.
	estimates, err := store.LoadMemoryEstimates(ctx)
	if err != nil {
		t.Fatalf("LoadMemoryEstimates: %v", err)
	}
	if len(estimates) == 0 {
		t.Fatal("expected non-empty memory estimates")
	}
	mean, ok := estimates["mem-wf"]
	if !ok {
		t.Fatal("mem-wf not in estimates")
	}
	if mean <= 0 {
		t.Errorf("mean bytes = %f, want > 0", mean)
	}

	// LoadMemoryStats.
	stats, err := store.LoadMemoryStats(ctx)
	if err != nil {
		t.Fatalf("LoadMemoryStats: %v", err)
	}
	if len(stats) == 0 {
		t.Fatal("expected non-empty memory stats")
	}
	found := false
	for _, s := range stats {
		if s.DefName == "mem-wf" {
			found = true
			if s.SampleCount < 1 {
				t.Errorf("sample_count = %d, want >= 1", s.SampleCount)
			}
			if s.AvgBytes <= 0 {
				t.Errorf("avg_bytes = %f, want > 0", s.AvgBytes)
			}
			break
		}
	}
	if !found {
		t.Error("mem-wf not found in memory stats")
	}

	// CleanupMemorySamples.
	cleaned, err := store.CleanupMemorySamples(ctx, 1)
	if err != nil {
		t.Fatalf("CleanupMemorySamples: %v", err)
	}
	if cleaned < 1 {
		t.Errorf("cleaned %d samples, want >= 1", cleaned)
	}
}

// ---------------------------------------------------------------------------
// ContinueAsNew and FinalizeWorkflowSegment Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_ContinueAsNew(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "can-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'can-wf', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	wf, err := store.ClaimWorkflow(ctx, "can-worker")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}

	newInput := json.RawMessage(`{"continued":true}`)
	newEvents := []EventRecord{
		{Step: 0, EventType: "continue_as_new", NewInput: `{"continued":true}`},
	}

	newRunID, err := store.ContinueAsNew(ctx, wfID, "can-worker", wf.Generation, "can-wf", 1, newInput, newEvents, `{"prev":"done"}`, nil, 0)
	if err != nil {
		t.Fatalf("ContinueAsNew: %v", err)
	}
	if newRunID == "" {
		t.Fatal("expected non-empty new run ID")
	}

	// Old workflow should be done.
	var oldStatus string
	err = db.QueryRowContext(ctx,
		`SELECT status FROM workflow_instances WHERE id = @p1`, wfID).Scan(&oldStatus)
	if err != nil {
		t.Fatalf("query old status: %v", err)
	}
	if oldStatus != "done" {
		t.Errorf("old status = %s, want done", oldStatus)
	}

	// New workflow should exist with the continued input.
	var newInputStr string
	err = db.QueryRowContext(ctx,
		`SELECT CAST(ISNULL(input, '') AS VARCHAR(MAX)) FROM workflow_instances WHERE id = @p1`, newRunID).Scan(&newInputStr)
	if err != nil {
		t.Fatalf("query new input: %v", err)
	}
	if newInputStr != `{"continued":true}` {
		t.Errorf("new input = %s", newInputStr)
	}

	// Event should have been recorded.
	var eventCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_history WHERE workflow_id = @p1`, wfID).Scan(&eventCount)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 1 {
		t.Errorf("expected 1 event, got %d", eventCount)
	}
}

func TestMSSQLIntegration_FinalizeWorkflowSegment_Done(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "fws-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'fws-wf', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	wf, err := store.ClaimWorkflow(ctx, "fws-worker")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}

	events := []EventRecord{
		{Step: 0, EventType: "call", Service: "svc", Op: "op", Request: "{}", Response: `{"res":1}`},
	}

	err = store.FinalizeWorkflowSegment(ctx, wfID, "fws-worker", wf.Generation, events, "done", `{"result":"final"}`, "", "", nil, time.Time{})
	if err != nil {
		t.Fatalf("FinalizeWorkflowSegment done: %v", err)
	}

	var status, result string
	err = db.QueryRowContext(ctx,
		`SELECT status, ISNULL(result, '') FROM workflow_instances WHERE id = @p1`, wfID).Scan(&status, &result)
	if err != nil {
		t.Fatalf("query done state: %v", err)
	}
	if status != "done" {
		t.Errorf("status = %s, want done", status)
	}
	if result != `{"result":"final"}` {
		t.Errorf("result = %s", result)
	}

	// Events are cleaned up on terminal status by the stored procedure.
	var eventCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_history WHERE workflow_id = @p1`, wfID).Scan(&eventCount)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 0 {
		t.Errorf("expected 0 events after terminal finalize, got %d", eventCount)
	}
}

func TestMSSQLIntegration_FinalizeWorkflowSegment_Suspend(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "fws2-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'fws2-wf', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	wf, err := store.ClaimWorkflow(ctx, "fws2-worker")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}

	// Suspend (return to ready with next_wake_at in future).
	futureWake := time.Now().Add(5 * time.Minute)
	events := []EventRecord{
		{Step: 0, EventType: "sleep", DurationMs: 300000},
	}
	err = store.FinalizeWorkflowSegment(ctx, wfID, "fws2-worker", wf.Generation, events, "ready", "", "", "", nil, futureWake)
	if err != nil {
		t.Fatalf("FinalizeWorkflowSegment suspend: %v", err)
	}

	var status string
	var nextWakeAt time.Time
	err = db.QueryRowContext(ctx,
		`SELECT status, next_wake_at FROM workflow_instances WHERE id = @p1`, wfID).Scan(&status, &nextWakeAt)
	if err != nil {
		t.Fatalf("query suspend state: %v", err)
	}
	if status != "ready" {
		t.Errorf("status = %s, want ready", status)
	}
	if nextWakeAt.Before(time.Now()) {
		t.Error("next_wake_at should be in the future after suspend")
	}
}

// ---------------------------------------------------------------------------
// PurgeWorkflowDef Tests
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_PurgeWorkflowDef(t *testing.T) {
	store, _ := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "purge-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// Retrieve the def first.
	def, err := store.GetWorkflowDef(ctx, "purge-wf", 1)
	if err != nil {
		t.Fatalf("GetWorkflowDef: %v", err)
	}
	if def == nil {
		t.Fatal("expected def before purge")
	}

	// Purge the def.
	if err := store.PurgeWorkflowDef(ctx, "purge-wf", 1); err != nil {
		t.Fatalf("PurgeWorkflowDef: %v", err)
	}

	// Verify it's gone.
	gone, err := store.GetWorkflowDef(ctx, "purge-wf", 1)
	if err != nil {
		t.Fatalf("GetWorkflowDef after purge: %v", err)
	}
	if gone != nil {
		t.Fatal("expected nil after purge")
	}
}

// ---------------------------------------------------------------------------
// GetActiveInstanceCountsByVersion Integration Test
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_GetActiveInstanceCountsByVersion(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "counts-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// Insert two ready instances.
	for i := 0; i < 2; i++ {
		_, err := db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
			VALUES (@p1, 'counts-wf', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default')
		`, uuid.New().String())
		if err != nil {
			t.Fatalf("insert ready instance %d: %v", i, err)
		}
	}

	// Insert one done instance (should not be counted).
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, completed_at, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, 'counts-wf', 1, 'done', SYSUTCDATETIME(), SYSUTCDATETIME(), '{}', 'default', '00000000-0000-0000-0000-000000000000')
	`, uuid.New().String())
	if err != nil {
		t.Fatalf("insert done instance: %v", err)
	}

	counts, err := store.GetActiveInstanceCountsByVersion(ctx)
	if err != nil {
		t.Fatalf("GetActiveInstanceCountsByVersion: %v", err)
	}
	if len(counts) == 0 {
		t.Fatal("expected non-empty counts")
	}
	if counts["counts-wf:1"] != 2 {
		t.Errorf("counts-wf:1 = %d, want 2", counts["counts-wf:1"])
	}
}

// ---------------------------------------------------------------------------
// ResolveTenantFromAPIKey Integration Test
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_ResolveTenantFromAPIKey(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()

	// The tenant first, then its key.
	//
	// The shipped schema has fk_api_keys_tenant, so an API key for a tenant
	// that does not exist cannot be inserted -- which is the right constraint
	// and is how a real deployment works. This test used to insert only the
	// key, and passed because engine/testutil's hand-written MSSQL schema
	// declared no such foreign key (IMPROVEMENT-PLAN 1.9, 2.71).
	//
	// dbo.tenants, not admin.tenants. The shipped schema defines BOTH pairs --
	// admin.tenants/admin.tenant_api_keys and dbo.tenants/dbo.tenant_api_keys --
	// and ResolveTenantFromAPIKey (engine/mssql_deployment.go:111) queries
	// `tenant_api_keys` unqualified, which resolves to dbo for a dbo-default
	// principal. Seeding admin.tenants therefore satisfied nothing: the insert
	// below hits dbo.tenant_api_keys, whose fk_api_keys_tenant references
	// dbo.tenants (001_schema.sql:343-344).
	tenantUUID := uuid.New()
	if _, err := db.ExecContext(ctx, `
		IF NOT EXISTS (SELECT 1 FROM dbo.tenants WHERE tenant_id = @p1)
		INSERT INTO dbo.tenants (tenant_id, name) VALUES (@p1, @p2)
	`, tenantUUID.String(), "tenant-"+tenantUUID.String()); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	keyHash := sha256Of("my-api-key")
	_, err := db.ExecContext(ctx, `
		INSERT INTO tenant_api_keys (key_hash, tenant_id, description)
		VALUES (@p1, @p2, 'test-key')
	`, keyHash, tenantUUID.String())
	if err != nil {
		t.Fatalf("insert tenant_api_key: %v", err)
	}

	got, err := store.ResolveTenantFromAPIKey(ctx, keyHash)
	if err != nil {
		t.Fatalf("ResolveTenantFromAPIKey: %v", err)
	}
	if got != tenantUUID {
		t.Errorf("tenant UUID = %v, want %v", got, tenantUUID)
	}

	// Unknown key.
	unknownHash := sha256Of("unknown-key")
	_, err = store.ResolveTenantFromAPIKey(ctx, unknownHash)
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

// ---------------------------------------------------------------------------
// ResolveLatestVersion Integration Test
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_ResolveLatestVersion_NotFound(t *testing.T) {
	store, _ := setupMSSQLIntegrationTest(t)
	ctx := context.Background()

	_, err := store.ResolveLatestVersion(ctx, "non-existent")
	if err == nil {
		t.Fatal("expected error for non-existent def")
	}
}

// ---------------------------------------------------------------------------
// GetDueSchedules Edge Cases
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_GetDueSchedules_Empty(t *testing.T) {
	store, _ := setupMSSQLIntegrationTest(t)
	ctx := context.Background()

	due, err := store.GetDueSchedules(ctx)
	if err != nil {
		t.Fatalf("GetDueSchedules empty: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("expected 0 due schedules, got %d", len(due))
	}
}

// ---------------------------------------------------------------------------
// GetChildCount for non-existent parent
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_GetChildCount_Zero(t *testing.T) {
	store, _ := setupMSSQLIntegrationTest(t)
	ctx := context.Background()

	count, err := store.GetChildCount(ctx, "non-existent-parent")
	if err != nil {
		t.Fatalf("GetChildCount non-existent: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

// ---------------------------------------------------------------------------
// GetConcurrencyKeyCount for non-existent workflow
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_GetConcurrencyKeyCount_Zero(t *testing.T) {
	store, _ := setupMSSQLIntegrationTest(t)
	ctx := context.Background()

	count, err := store.GetConcurrencyKeyCount(ctx, "non-existent")
	if err != nil {
		t.Fatalf("GetConcurrencyKeyCount: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

// ---------------------------------------------------------------------------
// ListWorkflows with various filters
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_ListWorkflows_Filters(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "filter-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	// Insert workflows with various inputs.
	wfID1 := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, 'filter-wf', 1, 'failed', SYSUTCDATETIME(), '{"data":"important"}', 'default', '00000000-0000-0000-0000-000000000000')
	`, wfID1)
	if err != nil {
		t.Fatalf("insert wf1: %v", err)
	}
	// Set error on wf1.
	_, err = db.ExecContext(ctx,
		`UPDATE workflow_instances SET error_msg = @p2 WHERE id = @p1`,
		wfID1, "critical failure")
	if err != nil {
		t.Fatalf("set error on wf1: %v", err)
	}

	wfID2 := uuid.New().String()
	_, err = db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, 'filter-wf', 1, 'done', SYSUTCDATETIME(), '{"data":"normal"}', 'default', '00000000-0000-0000-0000-000000000000')
	`, wfID2)
	if err != nil {
		t.Fatalf("insert wf2: %v", err)
	}

	// Filter by status.
	failed, err := store.ListWorkflows(ctx, WorkflowFilter{Status: "failed", Limit: 100})
	if err != nil {
		t.Fatalf("ListWorkflows failed: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed, got %d", len(failed))
	}
	if failed[0].ID != wfID1 {
		t.Errorf("failed workflow id = %s", failed[0].ID)
	}
	if failed[0].Error != "critical failure" {
		t.Errorf("failed workflow error = %s", failed[0].Error)
	}

	// Filter by ErrorContains.
	errFilter, err := store.ListWorkflows(ctx, WorkflowFilter{ErrorContains: "critical", Limit: 100})
	if err != nil {
		t.Fatalf("ListWorkflows error filter: %v", err)
	}
	if len(errFilter) != 1 {
		t.Fatalf("expected 1 workflow matching error, got %d", len(errFilter))
	}

	// Filter with limit.
	limited, err := store.ListWorkflows(ctx, WorkflowFilter{Limit: 1})
	if err != nil {
		t.Fatalf("ListWorkflows limit: %v", err)
	}
	if len(limited) > 1 {
		t.Errorf("expected at most 1 workflow with limit=1, got %d", len(limited))
	}
}

// ---------------------------------------------------------------------------
// GetEventCount for non-existent workflow
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_GetEventCount_Zero(t *testing.T) {
	store, _ := setupMSSQLIntegrationTest(t)
	ctx := context.Background()

	count, err := store.GetEventCount(ctx, "no-such-workflow")
	if err != nil {
		t.Fatalf("GetEventCount: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

// ---------------------------------------------------------------------------
// ReleaseWorkflow with wrong generation
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_ReleaseWorkflow_WrongGeneration(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "relwg-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	wfID := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue)
		VALUES (@p1, 'relwg-wf', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default')
	`, wfID)
	if err != nil {
		t.Fatalf("insert workflow_instance: %v", err)
	}

	// Release with generation that doesn't match should fail or return no error but not update.
	// The method uses beginTxWithContext so it does RLS context set.
	err = store.ReleaseWorkflow(ctx, wfID, "wrong-worker", 9999, time.Now().Add(time.Hour))
	if err == nil {
		t.Fatal("expected error for wrong generation/worker")
	}
}

// ---------------------------------------------------------------------------
// GetWorkflowByID with empty tenant filter
// ---------------------------------------------------------------------------

func TestMSSQLIntegration_GetWorkflowByID_TenantScoped(t *testing.T) {
	store, db := setupMSSQLIntegrationTest(t)
	ctx := context.Background()
	deployWorkflowDef(t, store, "ten-wf", 1, []byte{0x00, 0x61, 0x73, 0x6d})

	tenantA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	tenantB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	// Insert workflows for tenant A and tenant B.
	wfA := uuid.New().String()
	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, 'ten-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default', @p2)
	`, wfA, tenantA)
	if err != nil {
		t.Fatalf("insert wfA: %v", err)
	}

	wfB := uuid.New().String()
	_, err = db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, 'ten-wf', 1, 'running', SYSUTCDATETIME(), '{}', 'default', @p2)
	`, wfB, tenantB)
	if err != nil {
		t.Fatalf("insert wfB: %v", err)
	}

	// With default store (no tenant), GetWorkflowByID should NOT find wfA
	// since wfA belongs to tenantA (tenant-scoped filtering).
	storeDefault := store.WithTenant(DefaultTenantUUID)
	wfResult, err := storeDefault.GetWorkflowByID(ctx, wfA)
	if err != nil {
		t.Fatalf("GetWorkflowByID default tenant: %v", err)
	}
	if wfResult != nil {
		t.Fatal("expected default tenant NOT to see tenantA's workflow")
	}

	// With tenant A tenant, GetWorkflowByID should find wfA.
	storeA := store.WithTenant(tenantA)
	wfAResult, err := storeA.GetWorkflowByID(ctx, wfA)
	if err != nil {
		t.Fatalf("GetWorkflowByID tenantA: %v", err)
	}
	if wfAResult == nil {
		t.Fatal("expected to find wfA with tenantA")
	}
}

// ---------------------------------------------------------------------------
// Helper: sha256Of computes SHA-256 hash of a string as a byte slice.
// ---------------------------------------------------------------------------

func sha256Of(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}
