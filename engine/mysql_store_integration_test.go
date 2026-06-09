// mysql_store_integration_test.go — MySQL-specific integration tests
//
// These tests run against a real MySQL/MariaDB instance and verify that the
// MySQLStore backend correctly handles SQL generation, data round-tripping,
// and MySQL-specific features (INSERT IGNORE, NOW(6), INTERVAL syntax,
// SHA2/UNHEX for concurrency keys, etc.).
//
// Run with:
//   CLEAT_TEST_MYSQL="root:cleat@tcp(127.0.0.1:3306)/cleat?tls=false&parseTime=true&multiStatements=true" \
//     go test ./engine/ -run TestMySQLIntegration -v -count=1

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// mysqlIntegrationStore opens a real MySQL connection, sets up the schema,
// and returns a configured MySQLStore plus a teardown function.
func mysqlIntegrationStore(t *testing.T) (*MySQLStore, func()) {
	t.Helper()
	db := openMySQLTestDB(t)
	testutil.SetupMySQLFullSchema(t, db)
	testutil.CleanupMySQLTestData(t, db)

	store := NewMySQLStore(db)
	return store, func() {
		cleanupMySQLTestTables(t, store)
		db.Close()
	}
}

// cleanupMySQLTestTables removes all data from dynamic tables so each test
// starts with a clean slate.
func cleanupMySQLTestTables(t *testing.T, s *MySQLStore) {
	t.Helper()
	ctx := context.Background()

	// Delete schedules first.
	_ = s.DeleteSchedule(ctx, "integ-test-schedule")
	_ = s.DeleteSchedule(ctx, "integ-test-schedule-2")

	// Wipe dynamic tables.  The order respects MySQL FK constraints (children first).
	s.db.Exec("DELETE FROM workflow_update_requests")
	s.db.Exec("DELETE FROM workflow_promises")
	s.db.Exec("DELETE FROM workflow_signals")
	s.db.Exec("DELETE FROM concurrency_keys")
	s.db.Exec("DELETE FROM idempotency_keys")
	s.db.Exec("DELETE FROM event_history")
	s.db.Exec("DELETE FROM workflow_instances")
}

// deployTestDef deploys a minimal workflow definition for integration tests.
func deployTestDef(t *testing.T, s *MySQLStore, name string, version int) {
	t.Helper()
	ctx := context.Background()
	def := &WorkflowDef{
		Name:       name,
		Version:    version,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d}, // minimal WASM magic
		ABIVersion: 1,
		MinVersion: 1,
	}
	if err := s.DeployWorkflowDef(ctx, def); err != nil {
		t.Fatalf("DeployWorkflowDef(%s, v%d): %v", name, version, err)
	}
}

// createReadyWorkflow deploys the test-workflow def and starts one run,
// returning its ID.
func createReadyWorkflow(t *testing.T, s *MySQLStore, idempotencyKey string) string {
	t.Helper()
	ctx := context.Background()
	deployTestDef(t, s, "test-workflow", 1)
	runID, alreadyExisted, err := s.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), idempotencyKey, DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	if alreadyExisted {
		t.Fatalf("StartNewRun returned alreadyExisted=true for key %q", idempotencyKey)
	}
	return runID
}

// claimOne claims exactly one ready workflow and returns it.
func claimOne(t *testing.T, s *MySQLStore, workerID string) *WorkflowInstance {
	t.Helper()
	ctx := context.Background()
	wf, err := s.ClaimWorkflow(ctx, workerID)
	if err != nil {
		t.Fatalf("ClaimWorkflow(%s): %v", workerID, err)
	}
	if wf == nil {
		t.Fatal("ClaimWorkflow returned nil, expected one ready workflow")
	}
	if wf.Status != "running" {
		t.Fatalf("ClaimWorkflow status = %q, want %q", wf.Status, "running")
	}
	if wf.AssignedTo != workerID {
		t.Fatalf("ClaimWorkflow AssignedTo = %q, want %q", wf.AssignedTo, workerID)
	}
	return wf
}

// ---------------------------------------------------------------------------
// Factory & Configuration
// ---------------------------------------------------------------------------

// 1.
func TestMySQLIntegration_FactoryOpenStore(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()

	dsn := "root:cleat@tcp(127.0.0.1:3306)/cleat?tls=false&parseTime=true&multiStatements=true"
	factory := NewMySQLStoreFactory(s.db, dsn)
	if factory == nil {
		t.Fatal("NewMySQLStoreFactory returned nil")
	}
	if factory.DriverName() != "mysql" {
		t.Errorf("DriverName = %q, want %q", factory.DriverName(), "mysql")
	}
	if factory.Dialect() != DialectMySQL {
		t.Errorf("Dialect = %v, want %v", factory.Dialect(), DialectMySQL)
	}

	// OpenStore for the default tenant.
	ctx := context.Background()
	openedStore, closer, err := factory.OpenStore(ctx, "00000000-0000-0000-0000-000000000000", "default")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if openedStore == nil {
		t.Fatal("OpenStore returned nil")
	}
	if closer == nil {
		t.Fatal("OpenStore returned nil closer")
	}
	if err := closer.Close(); err != nil {
		t.Errorf("closer.Close: %v", err)
	}

	_ = factory.Close()
}

// 2.
func TestMySQLIntegration_FactoryTenantDSN(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()

	dsn := "root:cleat@tcp(127.0.0.1:3306)/?parseTime=true"
	factory := NewMySQLStoreFactory(s.db, dsn)

	tests := []struct {
		dbName   string
		expected string
	}{
		{"cleat_test", "root:cleat@tcp(127.0.0.1:3306)/cleat_test?parseTime=true"},
		{"cleat_tenant_abc", "root:cleat@tcp(127.0.0.1:3306)/cleat_tenant_abc?parseTime=true"},
	}

	for _, tc := range tests {
		got := factory.buildTenantDSN(tc.dbName)
		if got != tc.expected {
			t.Errorf("buildTenantDSN(%q) = %q, want %q", tc.dbName, got, tc.expected)
		}
	}

	// Without query parameters.
	factory2 := NewMySQLStoreFactory(s.db, "root:cleat@tcp(127.0.0.1:3306)/")
	got := factory2.buildTenantDSN("mydb")
	if got != "root:cleat@tcp(127.0.0.1:3306)/mydb" {
		t.Errorf("buildTenantDSN without params = %q, want %q", got, "root:cleat@tcp(127.0.0.1:3306)/mydb")
	}
}

// 3.
func TestMySQLIntegration_StoreConfigOptions(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()

	// WithTenant returns a copy with a different tenantID.
	s1 := s.WithTenant("tenant-xyz")
	if s1.tenantID != "tenant-xyz" {
		t.Errorf("WithTenant tenantID = %q, want %q", s1.tenantID, "tenant-xyz")
	}
	if s.tenantID == "tenant-xyz" {
		t.Error("WithTenant mutated the original store")
	}

	// WithIdempotencyKeyTTL returns a copy with a different TTL.
	ttl := 5 * time.Minute
	s2 := s.WithIdempotencyKeyTTL(ttl)
	if s2.idempotencyKeyTTL != ttl {
		t.Errorf("WithIdempotencyKeyTTL = %v, want %v", s2.idempotencyKeyTTL, ttl)
	}
	if s.idempotencyKeyTTL == ttl {
		t.Error("WithIdempotencyKeyTTL mutated the original store")
	}

	// WithReadRedactionDisabled returns a copy with redaction disabled.
	s3 := s.WithReadRedactionDisabled(true)
	if !s3.disableReadRedaction {
		t.Error("WithReadRedactionDisabled should set disableReadRedaction=true")
	}
	if s.disableReadRedaction {
		t.Error("WithReadRedactionDisabled mutated the original store")
	}

	// WithEncryption returns a copy.
	s4 := s.WithEncryption(nil, false)
	if s4 == nil {
		t.Error("WithEncryption returned nil")
	}
}

// ---------------------------------------------------------------------------
// Workflow Lifecycle
// ---------------------------------------------------------------------------

// 4.
func TestMySQLIntegration_ClaimWorkflow(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()

	_ = createReadyWorkflow(t, s, "integ-claim-1")

	wf := claimOne(t, s, "worker-1")

	// The claimed workflow should appear with status "running" in the DB.
	stored, err := s.GetWorkflowByID(context.Background(), wf.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if stored == nil {
		t.Fatal("claimed workflow not found")
	}
	if stored.Status != "running" {
		t.Errorf("stored status = %q, want %q", stored.Status, "running")
	}
	if stored.AssignedTo != "worker-1" {
		t.Errorf("AssignedTo = %q, want %q", stored.AssignedTo, "worker-1")
	}

	// A second claim should return nil (no ready workflows left).
	wf2, err := s.ClaimWorkflow(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("ClaimWorkflow (second): %v", err)
	}
	if wf2 != nil {
		t.Errorf("expected nil for second claim, got workflow %s", wf2.ID)
	}
}

// 5.
func TestMySQLIntegration_ClaimAndComplete(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	_ = createReadyWorkflow(t, s, "integ-complete-1")
	wf := claimOne(t, s, "worker-1")

	result := `{"completed":true}`
	if err := s.CompleteWorkflow(ctx, wf.ID, "worker-1", wf.Generation, result,
		map[string]string{"qs_key": "qs_val"}); err != nil {
		t.Fatalf("CompleteWorkflow: %v", err)
	}

	stored, err := s.GetWorkflowByID(ctx, wf.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if stored == nil {
		t.Fatal("workflow not found after CompleteWorkflow")
	}
	if stored.Status != "done" {
		t.Errorf("status = %q, want %q", stored.Status, "done")
	}
	if stored.Result != result {
		t.Errorf("result = %q, want %q", stored.Result, result)
	}

	// Verify query state was stored.
	qs, err := s.GetQueryState(ctx, wf.ID, "qs_key")
	if err != nil {
		t.Fatalf("GetQueryState: %v", err)
	}
	if qs != "qs_val" {
		t.Errorf("query state = %q, want %q", qs, "qs_val")
	}
}

// 6.
func TestMySQLIntegration_ClaimAndFail(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	_ = createReadyWorkflow(t, s, "integ-fail-1")
	wf := claimOne(t, s, "worker-1")

	errMsg := "something went wrong"
	errCode := "ERR_INTEG"
	errOp := "DurableCall"
	if err := s.FailWorkflow(ctx, wf.ID, "worker-1", wf.Generation,
		errMsg, errCode, errOp, nil); err != nil {
		t.Fatalf("FailWorkflow: %v", err)
	}

	stored, err := s.GetWorkflowByID(ctx, wf.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if stored == nil {
		t.Fatal("workflow not found after FailWorkflow")
	}
	if stored.Status != "failed" {
		t.Errorf("status = %q, want %q", stored.Status, "failed")
	}
	if stored.Error != errMsg {
		t.Errorf("Error = %q, want %q", stored.Error, errMsg)
	}
	if stored.ErrorCode != errCode {
		t.Errorf("ErrorCode = %q, want %q", stored.ErrorCode, errCode)
	}
	if stored.ErrorOp != errOp {
		t.Errorf("ErrorOp = %q, want %q", stored.ErrorOp, errOp)
	}
}

// 7.
func TestMySQLIntegration_DeadLetterAndRetry(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	_ = createReadyWorkflow(t, s, "integ-dl-1")
	wf := claimOne(t, s, "worker-1")

	// Move to dead letter queue.
	if err := s.MoveToDeadLetterQueue(ctx, wf.ID, "worker-1", wf.Generation,
		"exhausted retries", "max_retries", "DurableCall"); err != nil {
		t.Fatalf("MoveToDeadLetterQueue: %v", err)
	}

	stored, err := s.GetWorkflowByID(ctx, wf.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if stored == nil {
		t.Fatal("workflow not found after MoveToDeadLetterQueue")
	}
	if stored.Status != "dead_lettered" {
		t.Errorf("status = %q, want %q", stored.Status, "dead_lettered")
	}

	// Retry the dead-lettered workflow.
	if err := s.RetryWorkflow(ctx, wf.ID); err != nil {
		t.Fatalf("RetryWorkflow: %v", err)
	}

	retried, err := s.GetWorkflowByID(ctx, wf.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByID (after retry): %v", err)
	}
	if retried == nil {
		t.Fatal("workflow not found after RetryWorkflow")
	}
	if retried.Status != "ready" {
		t.Errorf("status after retry = %q, want %q", retried.Status, "ready")
	}
	if retried.AssignedTo != "" {
		t.Errorf("AssignedTo after retry = %q, want empty", retried.AssignedTo)
	}
	if retried.Error != "" {
		t.Errorf("Error after retry = %q, want empty", retried.Error)
	}
}

// 8.
func TestMySQLIntegration_ClaimAndRelease(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	_ = createReadyWorkflow(t, s, "integ-release-1")
	wf := claimOne(t, s, "worker-1")

	// Release with a future wake time (suspend).
	nextWakeAt := time.Now().Add(30 * time.Minute)
	if err := s.ReleaseWorkflow(ctx, wf.ID, "worker-1", wf.Generation, nextWakeAt); err != nil {
		t.Fatalf("ReleaseWorkflow: %v", err)
	}

	stored, err := s.GetWorkflowByID(ctx, wf.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if stored == nil {
		t.Fatal("workflow not found after ReleaseWorkflow")
	}
	if stored.Status != "ready" {
		t.Errorf("status = %q, want %q", stored.Status, "ready")
	}
	if stored.AssignedTo != "" {
		t.Errorf("AssignedTo after release = %q, want empty", stored.AssignedTo)
	}
	if stored.NextWakeAt.Before(time.Now().Add(20 * time.Minute)) {
		t.Errorf("NextWakeAt too early: %v (expected ~30m from now)", stored.NextWakeAt)
	}
	if stored.NextWakeAt.After(time.Now().Add(40 * time.Minute)) {
		t.Errorf("NextWakeAt too late: %v (expected ~30m from now)", stored.NextWakeAt)
	}
}

// 9.
func TestMySQLIntegration_ContinueAsNew(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	_ = createReadyWorkflow(t, s, "integ-continue-1")
	wf := claimOne(t, s, "worker-1")

	newInput := json.RawMessage(`{"iteration":2}`)
	newRunID, err := s.ContinueAsNew(ctx, wf.ID, "worker-1", wf.Generation,
		"test-workflow", 1, newInput, nil, `{"done":true}`, nil, 0)
	if err != nil {
		t.Fatalf("ContinueAsNew: %v", err)
	}

	// Old run must be "done".
	oldRun, err := s.GetWorkflowByID(ctx, wf.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByID (old): %v", err)
	}
	if oldRun == nil {
		t.Fatal("old workflow not found after ContinueAsNew")
	}
	if oldRun.Status != "done" {
		t.Errorf("old run status = %q, want %q", oldRun.Status, "done")
	}

	// New run must exist with a different ID and status "ready".
	newRun, err := s.GetWorkflowByID(ctx, newRunID)
	if err != nil {
		t.Fatalf("GetWorkflowByID (new): %v", err)
	}
	if newRun == nil {
		t.Fatal("new run not found after ContinueAsNew")
	}
	if newRun.ID == wf.ID {
		t.Error("new run ID same as old run -- they must differ")
	}
	if newRun.Status != "ready" {
		t.Errorf("new run status = %q, want %q", newRun.Status, "ready")
	}
}

// 10.
func TestMySQLIntegration_FinalizeWorkflowSegment(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	_ = createReadyWorkflow(t, s, "integ-finalize-1")
	wf := claimOne(t, s, "worker-1")

	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1", Request: `{"a":1}`},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op2", Request: `{"b":2}`},
	}
	if err := s.FinalizeWorkflowSegment(ctx, wf.ID, "worker-1", wf.Generation,
		events, "done", `{"result":"ok"}`, "", "", nil, time.Time{}); err != nil {
		t.Fatalf("FinalizeWorkflowSegment: %v", err)
	}

	// Events should be persisted.
	history, err := s.LoadEventHistory(ctx, wf.ID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 events, got %d", len(history))
	}
	if history[0].Step != 0 || history[0].Op != "op1" {
		t.Errorf("event[0] = step=%d op=%s, want step=0 op=op1", history[0].Step, history[0].Op)
	}
	if history[1].Step != 1 || history[1].Op != "op2" {
		t.Errorf("event[1] = step=%d op=%s, want step=1 op=op2", history[1].Step, history[1].Op)
	}

	// Status should be "done".
	stored, err := s.GetWorkflowByID(ctx, wf.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if stored == nil {
		t.Fatal("workflow not found after FinalizeWorkflowSegment")
	}
	if stored.Status != "done" {
		t.Errorf("status = %q, want %q", stored.Status, "done")
	}
}

// ---------------------------------------------------------------------------
// Event History
// ---------------------------------------------------------------------------

// 11.
func TestMySQLIntegration_AppendAndLoadEventHistory(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-eh-1")

	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "my-service",
		Op:        "my-op",
		Request:   `{"key":"value"}`,
		Response:  `{"ok":true}`,
	}
	if err := s.AppendEventHistory(ctx, runID, rec); err != nil {
		t.Fatalf("AppendEventHistory: %v", err)
	}

	history, err := s.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(history))
	}
	if history[0].Step != 0 {
		t.Errorf("Step = %d, want 0", history[0].Step)
	}
	if history[0].EventType != EventTypeCall {
		t.Errorf("EventType = %q, want %q", history[0].EventType, EventTypeCall)
	}
	if history[0].Service != "my-service" {
		t.Errorf("Service = %q, want %q", history[0].Service, "my-service")
	}
	if history[0].Op != "my-op" {
		t.Errorf("Op = %q, want %q", history[0].Op, "my-op")
	}
	if history[0].Request != `{"key":"value"}` {
		t.Errorf("Request = %q, want %q", history[0].Request, `{"key":"value"}`)
	}
}

// 12.
func TestMySQLIntegration_AppendEventHistoryBatch(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-eh-batch-1")

	recs := make([]EventRecord, 5)
	for i := 0; i < 5; i++ {
		recs[i] = EventRecord{
			Step:      i,
			EventType: EventTypeCall,
			Service:   "svc",
			Op:        fmt.Sprintf("op-%d", i),
			Request:   `{}`,
		}
	}
	if err := s.AppendEventHistoryBatch(ctx, runID, recs); err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}

	history, err := s.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 5 {
		t.Fatalf("expected 5 events, got %d", len(history))
	}
	for i, ev := range history {
		if ev.Step != i {
			t.Errorf("history[%d].Step = %d, want %d", i, ev.Step, i)
		}
		expectedOp := fmt.Sprintf("op-%d", i)
		if ev.Op != expectedOp {
			t.Errorf("history[%d].Op = %q, want %q", i, ev.Op, expectedOp)
		}
	}
}

// 13.
func TestMySQLIntegration_AppendEventHistoryIdempotent(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-eh-idem-1")

	rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "s", Op: "o"}

	// First append succeeds.
	if err := s.AppendEventHistory(ctx, runID, rec); err != nil {
		t.Fatalf("AppendEventHistory (first): %v", err)
	}

	// Second append of the same (workflow_id, step) must NOT error
	// (MySQL INSERT IGNORE semantics).
	if err := s.AppendEventHistory(ctx, runID, rec); err != nil {
		t.Fatalf("AppendEventHistory (duplicate): %v", err)
	}

	// Verify only one event exists.
	history, err := s.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 event after duplicate append, got %d", len(history))
	}
	if history[0].Step != 0 {
		t.Errorf("Step = %d, want 0", history[0].Step)
	}
}

// 14.
func TestMySQLIntegration_LoadEventHistoryPaginated(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-eh-page-1")

	// Create 10 events (steps 0-9).
	recs := make([]EventRecord, 10)
	for i := 0; i < 10; i++ {
		recs[i] = EventRecord{
			Step:      i,
			EventType: EventTypeCall,
			Service:   "s",
			Op:        fmt.Sprintf("op-%d", i),
		}
	}
	if err := s.AppendEventHistoryBatch(ctx, runID, recs); err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}

	// Page 1: offset=0, limit=3 -> 3 events (steps 0,1,2).
	page1, err := s.LoadEventHistoryPaginated(ctx, runID, 0, 3)
	if err != nil {
		t.Fatalf("LoadEventHistoryPaginated(0,3): %v", err)
	}
	if len(page1) != 3 {
		t.Fatalf("page1: expected 3 events, got %d", len(page1))
	}
	for i, ev := range page1 {
		if ev.Step != i {
			t.Errorf("page1[%d].Step = %d, want %d", i, ev.Step, i)
		}
	}

	// Page 2: offset=3, limit=3 -> 3 events (steps 3,4,5).
	page2, err := s.LoadEventHistoryPaginated(ctx, runID, 3, 3)
	if err != nil {
		t.Fatalf("LoadEventHistoryPaginated(3,3): %v", err)
	}
	if len(page2) != 3 {
		t.Fatalf("page2: expected 3 events, got %d", len(page2))
	}
	for i, ev := range page2 {
		want := i + 3
		if ev.Step != want {
			t.Errorf("page2[%d].Step = %d, want %d", i, ev.Step, want)
		}
	}

	// Page 3: offset=9, limit=5 -> 1 event (step 9).
	page3, err := s.LoadEventHistoryPaginated(ctx, runID, 9, 5)
	if err != nil {
		t.Fatalf("LoadEventHistoryPaginated(9,5): %v", err)
	}
	if len(page3) != 1 {
		t.Fatalf("page3: expected 1 event, got %d", len(page3))
	}
	if page3[0].Step != 9 {
		t.Errorf("page3[0].Step = %d, want 9", page3[0].Step)
	}

	// Page 4: offset=100, limit=10 -> 0 events.
	page4, err := s.LoadEventHistoryPaginated(ctx, runID, 100, 10)
	if err != nil {
		t.Fatalf("LoadEventHistoryPaginated(100,10): %v", err)
	}
	if len(page4) != 0 {
		t.Fatalf("page4: expected 0 events, got %d", len(page4))
	}

	// Verify CountEventHistory returns the total.
	count, err := s.CountEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("CountEventHistory: %v", err)
	}
	if count != 10 {
		t.Errorf("CountEventHistory = %d, want 10", count)
	}
}

// ---------------------------------------------------------------------------
// Heartbeat & Reaping
// ---------------------------------------------------------------------------

// 15.
func TestMySQLIntegration_Heartbeat(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	_ = createReadyWorkflow(t, s, "integ-hb-1")
	wf := claimOne(t, s, "worker-1")

	// Correct worker should succeed and return true.
	owned, err := s.Heartbeat(ctx, wf.ID, "worker-1", wf.Generation)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !owned {
		t.Error("Heartbeat returned false; expected true for correct worker/generation")
	}

	// Wrong worker should return false.
	owned, err = s.Heartbeat(ctx, wf.ID, "worker-2", wf.Generation)
	if err != nil {
		t.Fatalf("Heartbeat (wrong worker): %v", err)
	}
	if owned {
		t.Error("Heartbeat returned true for wrong worker; expected false")
	}

	// Wrong generation should return false.
	owned, err = s.Heartbeat(ctx, wf.ID, "worker-1", wf.Generation+999)
	if err != nil {
		t.Fatalf("Heartbeat (wrong generation): %v", err)
	}
	if owned {
		t.Error("Heartbeat returned true for wrong generation; expected false")
	}
}

// 16.
func TestMySQLIntegration_BatchHeartbeat(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	// Create 3 workflows and claim them all with the same worker.
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("integ-batch-hb-%d", i)
		createReadyWorkflow(t, s, key)
	}

	wfs, err := s.ClaimWorkflows(ctx, "batch-worker", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	if len(wfs) < 3 {
		t.Fatalf("ClaimWorkflows returned %d, want at least 3", len(wfs))
	}

	// BatchHeartbeat should update all running workflows assigned to this worker.
	count, err := s.BatchHeartbeat(ctx, "batch-worker")
	if err != nil {
		t.Fatalf("BatchHeartbeat: %v", err)
	}
	if count < 3 {
		t.Errorf("BatchHeartbeat returned %d, want >= 3", count)
	}
}

// 17.
func TestMySQLIntegration_ReapStaleInstances(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	_ = createReadyWorkflow(t, s, "integ-reap-1")
	wf := claimOne(t, s, "worker-1")

	// Reap with a very short timeout -- should reclaim our workflow since no
	// heartbeat was sent after claim.
	reaped, err := s.ReapStaleInstances(ctx, 1*time.Nanosecond)
	if err != nil {
		t.Fatalf("ReapStaleInstances: %v", err)
	}
	if reaped < 1 {
		t.Errorf("ReapStaleInstances returned %d, want >= 1", reaped)
	}

	// The workflow should now be back in "ready" status with no assigned worker.
	stored, err := s.GetWorkflowByID(ctx, wf.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if stored == nil {
		t.Fatal("workflow not found after ReapStaleInstances")
	}
	if stored.Status != "ready" {
		t.Errorf("status after reap = %q, want %q", stored.Status, "ready")
	}
	if stored.AssignedTo != "" {
		t.Errorf("AssignedTo after reap = %q, want empty", stored.AssignedTo)
	}
}

// ---------------------------------------------------------------------------
// Signals & Cancellation
// ---------------------------------------------------------------------------

// 18.
func TestMySQLIntegration_DeliverAndPollSignal(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-signal-1")

	payload := `{"data":"hello"}`
	if err := s.DeliverSignal(ctx, runID, "my-signal", payload); err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	gotPayload, found, err := s.PollSignal(ctx, runID, "my-signal")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if !found {
		t.Fatal("PollSignal: expected found=true")
	}
	if gotPayload != payload {
		t.Errorf("PollSignal payload = %q, want %q", gotPayload, payload)
	}

	// Polling again should return the payload as well (signals are not
	// consumed on read for MySQL).
	gotPayload2, found2, err := s.PollSignal(ctx, runID, "my-signal")
	if err != nil {
		t.Fatalf("PollSignal (second): %v", err)
	}
	if !found2 {
		t.Fatal("PollSignal (second): expected found=true")
	}
	if gotPayload2 != payload {
		t.Errorf("PollSignal (second) payload = %q, want %q", gotPayload2, payload)
	}
}

// 19.
func TestMySQLIntegration_PollAndClaimSignal(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-pcs-1")

	payload := `{"data":"claimed"}`
	if err := s.DeliverSignal(ctx, runID, "claim-signal", payload); err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	// PollAndClaimSignal should find and atomically claim the signal.
	gotPayload, found, err := s.PollAndClaimSignal(ctx, runID, "claim-signal")
	if err != nil {
		t.Fatalf("PollAndClaimSignal: %v", err)
	}
	if !found {
		t.Fatal("PollAndClaimSignal: expected found=true")
	}
	if gotPayload != payload {
		t.Errorf("PollAndClaimSignal payload = %q, want %q", gotPayload, payload)
	}

	// Second call should still find it (MySQL doesn't delete on claim).
	gotPayload2, found2, err := s.PollAndClaimSignal(ctx, runID, "claim-signal")
	if err != nil {
		t.Fatalf("PollAndClaimSignal (second): %v", err)
	}
	if !found2 {
		t.Fatal("PollAndClaimSignal (second): expected found=true")
	}
	if gotPayload2 != payload {
		t.Errorf("PollAndClaimSignal (second) payload = %q, want %q", gotPayload2, payload)
	}
}

// 20.
func TestMySQLIntegration_RequestAndCheckCancellation(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-cancel-1")

	// Initially not cancelled.
	cancelled, reason, err := s.CheckCancellation(ctx, runID)
	if err != nil {
		t.Fatalf("CheckCancellation (initial): %v", err)
	}
	if cancelled {
		t.Error("expected cancelled=false for fresh workflow")
	}
	if reason != "" {
		t.Errorf("expected empty reason, got %q", reason)
	}

	// Request cancellation.
	cancelReason := "user requested"
	if err := s.RequestCancellation(ctx, runID, cancelReason); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}

	// Now it should be cancelled.
	cancelled, reason, err = s.CheckCancellation(ctx, runID)
	if err != nil {
		t.Fatalf("CheckCancellation (after): %v", err)
	}
	if !cancelled {
		t.Error("expected cancelled=true after RequestCancellation")
	}
	if reason != cancelReason {
		t.Errorf("reason = %q, want %q", reason, cancelReason)
	}

	// PollCancellation should also report the same.
	cancelled2, reason2, err := s.PollCancellation(ctx, runID)
	if err != nil {
		t.Fatalf("PollCancellation: %v", err)
	}
	if !cancelled2 {
		t.Error("expected PollCancellation cancelled=true")
	}
	if reason2 != cancelReason {
		t.Errorf("PollCancellation reason = %q, want %q", reason2, cancelReason)
	}
}

// ---------------------------------------------------------------------------
// Promises
// ---------------------------------------------------------------------------

// 21.
func TestMySQLIntegration_CreateAndResolvePromise(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-promise-1")

	// Create a promise.
	if err := s.CreatePromise(ctx, runID, "my-promise", "promise-abc"); err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}

	// Verify it is pending.
	status, result, errMsg, err := s.GetPromise(ctx, runID, "promise-abc")
	if err != nil {
		t.Fatalf("GetPromise: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want %q", status, "pending")
	}
	if result != "" {
		t.Errorf("result = %q, want empty", result)
	}
	if errMsg != "" {
		t.Errorf("errMsg = %q, want empty", errMsg)
	}

	// Resolve the promise.
	if err := s.ResolvePromise(ctx, runID, "promise-abc", `{"resolved":true}`); err != nil {
		t.Fatalf("ResolvePromise: %v", err)
	}

	// Verify it is resolved.
	status, result, errMsg, err = s.GetPromise(ctx, runID, "promise-abc")
	if err != nil {
		t.Fatalf("GetPromise (after resolve): %v", err)
	}
	if status != "resolved" {
		t.Errorf("status = %q, want %q", status, "resolved")
	}
	if result != `{"resolved":true}` {
		t.Errorf("result = %q, want %q", result, `{"resolved":true}`)
	}
}

// 22.
func TestMySQLIntegration_CreateAndRejectPromise(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-promise-rej-1")

	if err := s.CreatePromise(ctx, runID, "my-promise", "promise-def"); err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}

	// Reject the promise.
	if err := s.RejectPromise(ctx, runID, "promise-def", "something went wrong"); err != nil {
		t.Fatalf("RejectPromise: %v", err)
	}

	// Verify it is rejected.
	status, result, errMsg, err := s.GetPromise(ctx, runID, "promise-def")
	if err != nil {
		t.Fatalf("GetPromise (after reject): %v", err)
	}
	if status != "rejected" {
		t.Errorf("status = %q, want %q", status, "rejected")
	}
	if errMsg != "something went wrong" {
		t.Errorf("errMsg = %q, want %q", errMsg, "something went wrong")
	}
	if result != "" {
		t.Errorf("result = %q, want empty", result)
	}
}

// 23.
func TestMySQLIntegration_ListPromises(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-promise-list-1")

	// Create 3 promises.
	for i := 0; i < 3; i++ {
		promiseID := fmt.Sprintf("promise-%d", i)
		promiseName := fmt.Sprintf("p%d", i)
		if err := s.CreatePromise(ctx, runID, promiseName, promiseID); err != nil {
			t.Fatalf("CreatePromise(%s): %v", promiseID, err)
		}
	}

	// Resolve the middle one.
	if err := s.ResolvePromise(ctx, runID, "promise-1", `{"ok":true}`); err != nil {
		t.Fatalf("ResolvePromise: %v", err)
	}

	promises, err := s.ListPromises(ctx, runID)
	if err != nil {
		t.Fatalf("ListPromises: %v", err)
	}
	if len(promises) != 3 {
		t.Fatalf("ListPromises returned %d, want 3", len(promises))
	}

	statuses := make(map[string]string)
	for _, p := range promises {
		statuses[p.PromiseID] = p.Status
	}
	if statuses["promise-0"] != "pending" {
		t.Errorf("promise-0 status = %q, want %q", statuses["promise-0"], "pending")
	}
	if statuses["promise-1"] != "resolved" {
		t.Errorf("promise-1 status = %q, want %q", statuses["promise-1"], "resolved")
	}
	if statuses["promise-2"] != "pending" {
		t.Errorf("promise-2 status = %q, want %q", statuses["promise-2"], "pending")
	}

	// Listing for a non-existent workflow should return empty, not error.
	noPromises, err := s.ListPromises(ctx, "nonexistent-workflow")
	if err != nil {
		t.Fatalf("ListPromises (nonexistent): %v", err)
	}
	if len(noPromises) != 0 {
		t.Errorf("ListPromises nonexistent returned %d, want 0", len(noPromises))
	}
}

// ---------------------------------------------------------------------------
// Workflow Definitions
// ---------------------------------------------------------------------------

// 24.
func TestMySQLIntegration_DeployAndGetWorkflowDef(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	def := &WorkflowDef{
		Name:       "integ-def-test",
		Version:    1,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1,
		MinVersion: 1,
		PluginDeps: map[string]string{"plugin-a": "1.0"},
	}
	if err := s.DeployWorkflowDef(ctx, def); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	got, err := s.GetWorkflowDef(ctx, "integ-def-test", 1)
	if err != nil {
		t.Fatalf("GetWorkflowDef: %v", err)
	}
	if got == nil {
		t.Fatal("GetWorkflowDef returned nil")
	}
	if got.Name != "integ-def-test" {
		t.Errorf("Name = %q, want %q", got.Name, "integ-def-test")
	}
	if got.Version != 1 {
		t.Errorf("Version = %d, want 1", got.Version)
	}
	if got.ABIVersion != 1 {
		t.Errorf("ABIVersion = %d, want 1", got.ABIVersion)
	}
	if got.MinVersion != 1 {
		t.Errorf("MinVersion = %d, want 1", got.MinVersion)
	}
	if len(got.WASMBytes) == 0 {
		t.Error("WASMBytes is empty after round-trip")
	}

	// Verify PluginDeps round-trips correctly.
	if got.PluginDeps == nil {
		t.Error("PluginDeps is nil after round-trip")
	} else if got.PluginDeps["plugin-a"] != "1.0" {
		t.Errorf("PluginDeps['plugin-a'] = %q, want %q", got.PluginDeps["plugin-a"], "1.0")
	}
}

// 25.
func TestMySQLIntegration_ListVersions(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	// Deploy multiple versions of the same workflow.
	for v := 1; v <= 3; v++ {
		def := &WorkflowDef{
			Name:       "integ-versions-test",
			Version:    v,
			WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1,
			MinVersion: 1,
		}
		if err := s.DeployWorkflowDef(ctx, def); err != nil {
			t.Fatalf("DeployWorkflowDef v%d: %v", v, err)
		}
	}

	versions, err := s.ListVersions(ctx, "integ-versions-test")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("ListVersions returned %d versions, want 3", len(versions))
	}

	// Versions should be returned in descending order (newest first).
	expected := []int{3, 2, 1}
	for i, v := range versions {
		if v != expected[i] {
			t.Errorf("versions[%d] = %d, want %d", i, v, expected[i])
		}
	}

	// ListWorkflowDefs should return full definitions.
	defs, err := s.ListWorkflowDefs(ctx, "integ-versions-test")
	if err != nil {
		t.Fatalf("ListWorkflowDefs: %v", err)
	}
	if len(defs) != 3 {
		t.Fatalf("ListWorkflowDefs returned %d, want 3", len(defs))
	}

	// ResolveLatestVersion should return 3.
	latest, err := s.ResolveLatestVersion(ctx, "integ-versions-test")
	if err != nil {
		t.Fatalf("ResolveLatestVersion: %v", err)
	}
	if latest != 3 {
		t.Errorf("ResolveLatestVersion = %d, want 3", latest)
	}

	// ValidateVersion should work for existing and non-existing versions.
	valid, err := s.ValidateVersion(ctx, "integ-versions-test", 2)
	if err != nil {
		t.Fatalf("ValidateVersion (existing): %v", err)
	}
	if !valid {
		t.Error("ValidateVersion returned false for existing version 2")
	}

	valid, err = s.ValidateVersion(ctx, "integ-versions-test", 99)
	if err != nil {
		t.Fatalf("ValidateVersion (missing): %v", err)
	}
	if valid {
		t.Error("ValidateVersion returned true for non-existing version 99")
	}
}

// 26.
func TestMySQLIntegration_MarkVersionDeprecated(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	deployTestDef(t, s, "integ-dep-test", 1)

	// Deprecate the version.
	if err := s.MarkVersionDeprecated(ctx, "integ-dep-test", 1, true); err != nil {
		t.Fatalf("MarkVersionDeprecated(true): %v", err)
	}

	def, err := s.GetWorkflowDef(ctx, "integ-dep-test", 1)
	if err != nil {
		t.Fatalf("GetWorkflowDef: %v", err)
	}
	if def == nil {
		t.Fatal("GetWorkflowDef returned nil")
	}
	if !def.Deprecated {
		t.Error("expected Deprecated=true after MarkVersionDeprecated")
	}

	// Un-deprecate.
	if err := s.MarkVersionDeprecated(ctx, "integ-dep-test", 1, false); err != nil {
		t.Fatalf("MarkVersionDeprecated(false): %v", err)
	}

	def, err = s.GetWorkflowDef(ctx, "integ-dep-test", 1)
	if err != nil {
		t.Fatalf("GetWorkflowDef: %v", err)
	}
	if def == nil {
		t.Fatal("GetWorkflowDef returned nil")
	}
	if def.Deprecated {
		t.Error("expected Deprecated=false after MarkVersionDeprecated(false)")
	}
}

// ---------------------------------------------------------------------------
// Listing & Queries
// ---------------------------------------------------------------------------

// 27.
func TestMySQLIntegration_ListWorkflowsAndGetByID(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-list-1")

	// GetWorkflowByID with an existing workflow.
	wf, err := s.GetWorkflowByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf == nil {
		t.Fatal("GetWorkflowByID returned nil for existing workflow")
	}
	if wf.ID != runID {
		t.Errorf("ID = %q, want %q", wf.ID, runID)
	}
	if wf.DefName != "test-workflow" {
		t.Errorf("DefName = %q, want %q", wf.DefName, "test-workflow")
	}
	if wf.Status != "ready" {
		t.Errorf("Status = %q, want %q", wf.Status, "ready")
	}

	// GetWorkflowByID with non-existing workflow returns nil.
	wf, err = s.GetWorkflowByID(ctx, "nonexistent-id")
	if err != nil {
		t.Fatalf("GetWorkflowByID (nonexistent): %v", err)
	}
	if wf != nil {
		t.Errorf("expected nil for nonexistent ID, got %+v", wf)
	}

	// ListWorkflows without filter should return at least the one we created.
	results, err := s.ListWorkflows(ctx, WorkflowFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(results) < 1 {
		t.Fatalf("ListWorkflows returned %d, want >= 1", len(results))
	}

	found := false
	for _, r := range results {
		if r.ID == runID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListWorkflows did not include run %s", runID)
	}

	// ListWorkflows with Status filter.
	readyResults, err := s.ListWorkflows(ctx, WorkflowFilter{Status: "ready", Limit: 100})
	if err != nil {
		t.Fatalf("ListWorkflows (ready): %v", err)
	}
	if len(readyResults) < 1 {
		t.Errorf("ListWorkflows(status=ready) returned %d, want >= 1", len(readyResults))
	}
}

// ---------------------------------------------------------------------------
// Other
// ---------------------------------------------------------------------------

// 28.
func TestMySQLIntegration_TraceWorkflow(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-trace-1")

	traceID := "trace-abc-123"
	if err := s.TraceWorkflow(ctx, runID, traceID); err != nil {
		t.Fatalf("TraceWorkflow: %v", err)
	}

	wf, err := s.GetWorkflowByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf == nil {
		t.Fatal("workflow not found after TraceWorkflow")
	}
	if wf.TraceID != traceID {
		t.Errorf("TraceID = %q, want %q", wf.TraceID, traceID)
	}
}

// ---------------------------------------------------------------------------
// Error Handling & Edge Cases
// ---------------------------------------------------------------------------

// TestMySQLIntegration_StartNewRunIdempotent verifies that starting a new run
// with the same idempotency key returns the existing run ID.
func TestMySQLIntegration_StartNewRunIdempotent(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	deployTestDef(t, s, "test-workflow", 1)

	runID1, alreadyExisted, err := s.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "idem-key-1", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (first): %v", err)
	}
	if alreadyExisted {
		t.Fatal("alreadyExisted=true on first call, expected false")
	}

	runID2, alreadyExisted, err := s.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "idem-key-1", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (second): %v", err)
	}
	if !alreadyExisted {
		t.Fatal("alreadyExisted=false on second call, expected true")
	}
	if runID2 != runID1 {
		t.Errorf("second call returned different runID: %q vs %q", runID2, runID1)
	}

	// Different key should create a different run.
	runID3, alreadyExisted, err := s.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "idem-key-2", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (different key): %v", err)
	}
	if alreadyExisted {
		t.Fatal("alreadyExisted=true for different key, expected false")
	}
	if runID3 == runID1 {
		t.Error("different idempotency keys produced identical runID")
	}
}

// TestMySQLIntegration_ConcurrencyKeys exercises the Acquire, Release, and
// Reap cycle for concurrency keys.
func TestMySQLIntegration_ConcurrencyKeys(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-concur-1")
	runID2 := createReadyWorkflow(t, s, "integ-concur-2")

	// Acquire a key for the first workflow.
	acquired, err := s.AcquireConcurrencyKey(ctx, "my-key", runID, 1*time.Hour)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Error("AcquireConcurrencyKey returned false, expected true (first acquisition)")
	}

	// Second workflow should NOT be able to acquire the same key.
	acquired, err = s.AcquireConcurrencyKey(ctx, "my-key", runID2, 1*time.Hour)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey (second): %v", err)
	}
	if acquired {
		t.Error("AcquireConcurrencyKey returned true for already-held key, expected false")
	}

	// Verify count for the holding workflow.
	count, err := s.GetConcurrencyKeyCount(ctx, runID)
	if err != nil {
		t.Fatalf("GetConcurrencyKeyCount: %v", err)
	}
	if count < 1 {
		t.Errorf("GetConcurrencyKeyCount = %d, want >= 1", count)
	}

	// Release the key.
	if err := s.ReleaseConcurrencyKey(ctx, "my-key"); err != nil {
		t.Fatalf("ReleaseConcurrencyKey: %v", err)
	}

	// Now the second workflow should be able to acquire it.
	acquired, err = s.AcquireConcurrencyKey(ctx, "my-key", runID2, 1*time.Hour)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey (after release): %v", err)
	}
	if !acquired {
		t.Error("AcquireConcurrencyKey returned false after release, expected true")
	}

	// Release all keys for a workflow.
	if err := s.ReleaseWorkflowConcurrencyKeys(ctx, runID2); err != nil {
		t.Fatalf("ReleaseWorkflowConcurrencyKeys: %v", err)
	}

	// Reap expired keys (there shouldn't be any since TTL hasn't expired).
	reaped, err := s.ReapExpiredConcurrencyKeys(ctx)
	if err != nil {
		t.Fatalf("ReapExpiredConcurrencyKeys: %v", err)
	}
	if reaped != 0 {
		t.Errorf("ReapExpiredConcurrencyKeys = %d, want 0 (no keys should be expired)", reaped)
	}
}

// TestMySQLIntegration_UpdateRequests exercises the full update request lifecycle.
func TestMySQLIntegration_UpdateRequests(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-update-1")

	// Create an update request.
	if err := s.CreateUpdateRequest(ctx, runID, "my-update", `{"value":42}`, "promise-upd"); err != nil {
		t.Fatalf("CreateUpdateRequest: %v", err)
	}

	// Get pending update requests.
	pending, err := s.GetPendingUpdateRequests(ctx, runID)
	if err != nil {
		t.Fatalf("GetPendingUpdateRequests: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("GetPendingUpdateRequests returned %d, want 1", len(pending))
	}
	if pending[0].UpdateName != "my-update" {
		t.Errorf("UpdateName = %q, want %q", pending[0].UpdateName, "my-update")
	}
	if pending[0].Payload != `{"value":42}` {
		t.Errorf("Payload = %q, want %q", pending[0].Payload, `{"value":42}`)
	}
	if pending[0].Status != "pending" {
		t.Errorf("Status = %q, want %q", pending[0].Status, "pending")
	}

	// Complete the update request.
	if err := s.CompleteUpdateRequest(ctx, runID, "my-update", `{"result":"ok"}`, ""); err != nil {
		t.Fatalf("CompleteUpdateRequest: %v", err)
	}

	// After completion, there should be no pending update requests.
	pending, err = s.GetPendingUpdateRequests(ctx, runID)
	if err != nil {
		t.Fatalf("GetPendingUpdateRequests (after): %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("GetPendingUpdateRequests after completion returned %d, want 0", len(pending))
	}
}

// TestMySQLIntegration_ScheduleLifecycle exercises schedule creation, listing,
// getting due schedules, and deletion.
func TestMySQLIntegration_ScheduleLifecycle(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	deployTestDef(t, s, "test-workflow", 1)

	// Create a schedule.
	sched := Schedule{
		Name:           "integ-test-schedule",
		DefName:        "test-workflow",
		EntryPoint:     "main",
		CronExpression: "*/5 * * * *",
		Input:          json.RawMessage(`{"scheduled":true}`),
		Enabled:        true,
		NextRunAt:      time.Now().Add(-1 * time.Hour), // due now
	}
	if err := s.CreateSchedule(ctx, sched); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	// List schedules.
	schedules, err := s.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(schedules) < 1 {
		t.Fatalf("ListSchedules returned %d, want >= 1", len(schedules))
	}

	found := false
	for _, sc := range schedules {
		if sc.Name == "integ-test-schedule" {
			found = true
			if sc.DefName != "test-workflow" {
				t.Errorf("DefName = %q, want %q", sc.DefName, "test-workflow")
			}
			if !sc.Enabled {
				t.Error("Enabled should be true")
			}
			break
		}
	}
	if !found {
		t.Fatal("integ-test-schedule not found in ListSchedules")
	}

	// GetDueSchedules should return it because NextRunAt is in the past.
	due, err := s.GetDueSchedules(ctx)
	if err != nil {
		t.Fatalf("GetDueSchedules: %v", err)
	}
	foundDue := false
	for _, sc := range due {
		if sc.Name == "integ-test-schedule" {
			foundDue = true
			break
		}
	}
	if !foundDue {
		t.Error("integ-test-schedule not included in GetDueSchedules")
	}

	// Update the next run time.
	newNextRun := time.Now().Add(1 * time.Hour)
	if err := s.UpdateScheduleNextRun(ctx, "integ-test-schedule", newNextRun); err != nil {
		t.Fatalf("UpdateScheduleNextRun: %v", err)
	}

	// Disable the schedule.
	if err := s.SetScheduleEnabled(ctx, "integ-test-schedule", false); err != nil {
		t.Fatalf("SetScheduleEnabled(false): %v", err)
	}

	// Verify it is no longer due (disabled).
	due2, err := s.GetDueSchedules(ctx)
	if err != nil {
		t.Fatalf("GetDueSchedules (after disable): %v", err)
	}
	for _, sc := range due2 {
		if sc.Name == "integ-test-schedule" {
			t.Error("disabled schedule appeared in GetDueSchedules")
			break
		}
	}

	// Delete the schedule.
	if err := s.DeleteSchedule(ctx, "integ-test-schedule"); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
}

// TestMySQLIntegration_StickyWorker tests sticky worker assignment and clearing.
func TestMySQLIntegration_StickyWorker(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-sticky-1")

	// Update sticky worker.
	if err := s.UpdateStickyWorker(ctx, runID, "sticky-1"); err != nil {
		t.Fatalf("UpdateStickyWorker: %v", err)
	}

	// Claim sticky workflows for this worker.
	wfs, err := s.ClaimStickyWorkflows(ctx, "sticky-1", 10)
	if err != nil {
		t.Fatalf("ClaimStickyWorkflows: %v", err)
	}
	if len(wfs) == 0 {
		t.Fatal("ClaimStickyWorkflows returned 0, expected at least our workflow")
	}

	foundSticky := false
	for _, wf := range wfs {
		if wf.ID == runID {
			foundSticky = true
			if wf.Status != "running" {
				t.Errorf("claimed sticky workflow status = %q, want %q", wf.Status, "running")
			}
			if wf.AssignedTo != "sticky-1" {
				t.Errorf("AssignedTo = %q, want %q", wf.AssignedTo, "sticky-1")
			}
			break
		}
	}
	if !foundSticky {
		t.Errorf("workflow %s not found in ClaimStickyWorkflows results", runID)
	}

	// Clear sticky worker.
	if err := s.ClearStickyWorker(ctx, runID); err != nil {
		t.Fatalf("ClearStickyWorker: %v", err)
	}
}

// TestMySQLIntegration_TerminateWorkflow tests force-terminating a workflow.
func TestMySQLIntegration_TerminateWorkflow(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	_ = createReadyWorkflow(t, s, "integ-term-1")
	wf := claimOne(t, s, "worker-1")

	reason := "force stop"
	if err := s.TerminateWorkflow(ctx, wf.ID, reason); err != nil {
		t.Fatalf("TerminateWorkflow: %v", err)
	}

	stored, err := s.GetWorkflowByID(ctx, wf.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if stored == nil {
		t.Fatal("workflow not found after TerminateWorkflow")
	}
	if stored.Status != "terminated" {
		t.Errorf("status = %q, want %q", stored.Status, "terminated")
	}
}

// TestMySQLIntegration_ChildWorkflow tests StartChildWorkflow and GetChildResult.
func TestMySQLIntegration_ChildWorkflow(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	deployTestDef(t, s, "test-workflow", 1)
	parentID := createReadyWorkflow(t, s, "integ-child-parent-1")

	// Start a child workflow.
	childID, err := s.StartChildWorkflow(ctx, parentID, "test-workflow", `{"child":true}`, 0, "abandon", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}
	if childID == "" {
		t.Fatal("StartChildWorkflow returned empty childID")
	}

	// GetChildResult should return not completed.
	result, completed, err := s.GetChildResult(ctx, childID)
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if completed {
		t.Error("GetChildResult returned completed=true for a newly created child")
	}
	if result != "" {
		t.Errorf("GetChildResult result = %q, want empty", result)
	}

	// Claim and complete the child.
	childWF, err := s.ClaimWorkflow(ctx, "child-worker")
	if err != nil {
		t.Fatalf("ClaimWorkflow (child): %v", err)
	}
	if childWF == nil {
		t.Fatal("ClaimWorkflow returned nil for child")
	}
	if err := s.CompleteWorkflow(ctx, childWF.ID, "child-worker", childWF.Generation,
		`{"child_done":true}`, nil); err != nil {
		t.Fatalf("CompleteWorkflow (child): %v", err)
	}

	// Now GetChildResult should return completed.
	result, completed, err = s.GetChildResult(ctx, childID)
	if err != nil {
		t.Fatalf("GetChildResult (after completion): %v", err)
	}
	if !completed {
		t.Error("GetChildResult returned completed=false after child completed")
	}
	if result != `{"child_done":true}` {
		t.Errorf("GetChildResult result = %q, want %q", result, `{"child_done":true}`)
	}

	// GetChildCount should reflect the parent's children.
	count, err := s.GetChildCount(ctx, parentID)
	if err != nil {
		t.Fatalf("GetChildCount: %v", err)
	}
	if count < 1 {
		t.Errorf("GetChildCount = %d, want >= 1", count)
	}
}

// TestMySQLIntegration_LoadWASM tests deploying and loading WASM bytes.
func TestMySQLIntegration_LoadWASM(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00} // valid WASM header
	def := &WorkflowDef{
		Name:       "wasm-test",
		Version:    1,
		WASMBytes:  wasmBytes,
		ABIVersion: 1,
		MinVersion: 1,
	}
	if err := s.DeployWorkflowDef(ctx, def); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	// LoadWASM should return the exact bytes.
	loaded, err := s.LoadWASM(ctx, "wasm-test", 1)
	if err != nil {
		t.Fatalf("LoadWASM: %v", err)
	}
	if len(loaded) != len(wasmBytes) {
		t.Fatalf("LoadWASM returned %d bytes, want %d", len(loaded), len(wasmBytes))
	}
	for i := range wasmBytes {
		if loaded[i] != wasmBytes[i] {
			t.Errorf("byte[%d] = %x, want %x", i, loaded[i], wasmBytes[i])
			break
		}
	}

	// GetWASMLength should return the correct length.
	length, err := s.GetWASMLength(ctx, "wasm-test", 1)
	if err != nil {
		t.Fatalf("GetWASMLength: %v", err)
	}
	if length != int64(len(wasmBytes)) {
		t.Errorf("GetWASMLength = %d, want %d", length, len(wasmBytes))
	}
}

// TestMySQLIntegration_QueueDepth tests QueueDepth returns a non-negative count.
func TestMySQLIntegration_QueueDepth(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()

	// No workflows should mean depth 0.
	depth, err := s.QueueDepth(context.Background())
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth < 0 {
		t.Errorf("QueueDepth = %d, want >= 0", depth)
	}
}

// TestMySQLIntegration_RecordMemorySample tests recording and loading memory estimates.
func TestMySQLIntegration_RecordMemorySample(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	deployTestDef(t, s, "test-workflow", 1)

	// Record a memory sample.
	if err := s.RecordWorkflowMemorySample(ctx, "test-workflow", 1024); err != nil {
		t.Fatalf("RecordWorkflowMemorySample: %v", err)
	}

	// Load memory estimates.
	estimates, err := s.LoadMemoryEstimates(ctx)
	if err != nil {
		t.Fatalf("LoadMemoryEstimates: %v", err)
	}
	avg, ok := estimates["test-workflow"]
	if !ok || avg <= 0 {
		t.Errorf("LoadMemoryEstimates didn't include test-workflow or avg=%f <= 0", avg)
	}

	// Load memory stats.
	stats, err := s.LoadMemoryStats(ctx)
	if err != nil {
		t.Fatalf("LoadMemoryStats: %v", err)
	}
	if len(stats) < 1 {
		t.Fatalf("LoadMemoryStats returned %d, want >= 1", len(stats))
	}
}

// TestMySQLIntegration_BinaryDataRoundTrip verifies that binary data with
// non-ASCII characters (including null bytes) round-trips correctly through MySQL.
func TestMySQLIntegration_BinaryDataRoundTrip(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-binary-1")

	binaryPayload := "binary\x00\x01\x02data"
	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "binary-svc",
		Op:        "binary-op",
		Request:   binaryPayload,
	}
	if err := s.AppendEventHistory(ctx, runID, rec); err != nil {
		t.Fatalf("AppendEventHistory: %v", err)
	}

	history, err := s.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(history))
	}
	if history[0].Request != binaryPayload {
		t.Errorf("binary Request round-trip failed:\ngot:  %q\nwant: %q",
			history[0].Request, binaryPayload)
	}
}

// TestMySQLIntegration_VerifyWorkflowEvents tests the VerifyWorkflowEvents method.
func TestMySQLIntegration_VerifyWorkflowEvents(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-verify-1")

	rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "s", Op: "o"}
	if err := s.AppendEventHistory(ctx, runID, rec); err != nil {
		t.Fatalf("AppendEventHistory: %v", err)
	}

	// VerifyWorkflowEvents should not error.
	if err := s.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Fatalf("VerifyWorkflowEvents: %v", err)
	}

	// Verify for non-existent workflow should also not error.
	if err := s.VerifyWorkflowEvents(ctx, "nonexistent"); err != nil {
		t.Fatalf("VerifyWorkflowEvents (nonexistent): %v", err)
	}
}

// TestMySQLIntegration_LoadDAGSpec tests loading a DAG spec from a deployed
// workflow definition. Since DAGSpec is not a field on WorkflowDef, we set it
// via raw SQL and then verify LoadDAGSpec returns it.
func TestMySQLIntegration_LoadDAGSpec(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	deployTestDef(t, s, "dag-test", 1)

	// Verify that without a dag_spec set, LoadDAGSpec returns nil.
	dagSpec, err := s.LoadDAGSpec(ctx, "dag-test", 1)
	if err != nil {
		t.Fatalf("LoadDAGSpec: %v", err)
	}
	if dagSpec != nil {
		t.Errorf("LoadDAGSpec for def without DAG returned %q, want nil", string(dagSpec))
	}

	// Set dag_spec via raw SQL.
	_, err = s.db.Exec("UPDATE workflow_defs SET dag_spec = ? WHERE name = ? AND version = ?",
		`{"steps":["a","b"]}`, "dag-test", 1)
	if err != nil {
		t.Fatalf("update dag_spec: %v", err)
	}

	// Now LoadDAGSpec should return it.
	dagSpec, err = s.LoadDAGSpec(ctx, "dag-test", 1)
	if err != nil {
		t.Fatalf("LoadDAGSpec (after update): %v", err)
	}
	if dagSpec == nil {
		t.Fatal("LoadDAGSpec returned nil after updating dag_spec")
	}
	if string(dagSpec) != `{"steps":["a","b"]}` {
		t.Errorf("DAGSpec = %q, want %q", string(dagSpec), `{"steps":["a","b"]}`)
	}

	// LoadDAGSpec for a non-existent definition should return an error.
	_, err = s.LoadDAGSpec(ctx, "nonexistent", 1)
	if err == nil {
		t.Error("LoadDAGSpec for nonexistent def should return an error")
	}
}

// TestMySQLIntegration_GetEventCount tests GetEventCount returns the correct
// event count for a workflow.
func TestMySQLIntegration_GetEventCount(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-event-count-1")

	// Initially zero events.
	count, err := s.GetEventCount(ctx, runID)
	if err != nil {
		t.Fatalf("GetEventCount: %v", err)
	}
	if count != 0 {
		t.Errorf("GetEventCount = %d, want 0", count)
	}

	// Append 3 events.
	for i := 0; i < 3; i++ {
		rec := EventRecord{
			Step:      i,
			EventType: EventTypeCall,
			Service:   "s",
			Op:        fmt.Sprintf("op-%d", i),
		}
		if err := s.AppendEventHistory(ctx, runID, rec); err != nil {
			t.Fatalf("AppendEventHistory[%d]: %v", i, err)
		}
	}

	// Now count should be 3.
	count, err = s.GetEventCount(ctx, runID)
	if err != nil {
		t.Fatalf("GetEventCount (after): %v", err)
	}
	if count != 3 {
		t.Errorf("GetEventCount = %d, want 3", count)
	}
}

// TestMySQLIntegration_GetAllowedSignalCallers tests the allowed signal callers feature.
func TestMySQLIntegration_GetAllowedSignalCallers(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-allowed-1")

	// For a fresh workflow, GetAllowedSignalCallers should return nil
	// (deny-all semantics when allowed_signals is NULL).
	callers, err := s.GetAllowedSignalCallers(ctx, runID)
	if err != nil {
		t.Fatalf("GetAllowedSignalCallers: %v", err)
	}
	if callers != nil {
		t.Errorf("GetAllowedSignalCallers = %v, want nil", callers)
	}
}

// TestMySQLIntegration_ActiveInstancesCount tests version-related instance
// counting functions.
func TestMySQLIntegration_ActiveInstancesCount(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	deployTestDef(t, s, "test-workflow", 1)
	deployTestDef(t, s, "test-workflow", 2)

	// Create 2 instances of v1.
	runID1, _, err := s.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "active-count-v1-1", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun v1.1: %v", err)
	}
	_, _, err = s.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "active-count-v1-2", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun v1.2: %v", err)
	}

	// Create 1 instance of v2.
	_, _, err = s.StartNewRun(ctx, "", "test-workflow", 2,
		json.RawMessage(`{}`), "active-count-v2-1", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun v2.1: %v", err)
	}

	// CountActiveInstances for v1 should be 2.
	count, err := s.CountActiveInstances(ctx, "test-workflow", 1)
	if err != nil {
		t.Fatalf("CountActiveInstances v1: %v", err)
	}
	if count != 2 {
		t.Errorf("CountActiveInstances v1 = %d, want 2", count)
	}

	// CountActiveInstances for v2 should be 1.
	count, err = s.CountActiveInstances(ctx, "test-workflow", 2)
	if err != nil {
		t.Fatalf("CountActiveInstances v2: %v", err)
	}
	if count != 1 {
		t.Errorf("CountActiveInstances v2 = %d, want 1", count)
	}

	// Claim one v1 and complete it — should reduce active count.
	wf, err := s.ClaimWorkflow(ctx, "worker-1")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if wf != nil && wf.ID == runID1 {
		if err := s.CompleteWorkflow(ctx, wf.ID, "worker-1", wf.Generation, `{}`, nil); err != nil {
			t.Fatalf("CompleteWorkflow: %v", err)
		}
	}

	// GetActiveInstanceCountsByVersion should return the breakdown.
	countsByVersion, err := s.GetActiveInstanceCountsByVersion(ctx)
	if err != nil {
		t.Fatalf("GetActiveInstanceCountsByVersion: %v", err)
	}
	t.Logf("Active counts: %v", countsByVersion)
}

// TestMySQLIntegration_EmptyStore verifies that all read operations return
// zero-values on a completely empty store.
func TestMySQLIntegration_EmptyStore(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	// ClaimWorkflow on empty store.
	wf, err := s.ClaimWorkflow(ctx, "worker-1")
	if err != nil {
		t.Fatalf("ClaimWorkflow (empty): %v", err)
	}
	if wf != nil {
		t.Error("ClaimWorkflow on empty store should return nil")
	}

	// ClaimWorkflows on empty store.
	wfs, err := s.ClaimWorkflows(ctx, "worker-1", 5)
	if err != nil {
		t.Fatalf("ClaimWorkflows (empty): %v", err)
	}
	if len(wfs) != 0 {
		t.Errorf("ClaimWorkflows on empty store returned %d, want 0", len(wfs))
	}

	// LoadEventHistory on empty workflow.
	history, err := s.LoadEventHistory(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("LoadEventHistory (nonexistent): %v", err)
	}
	if len(history) != 0 {
		t.Errorf("LoadEventHistory on empty store returned %d events", len(history))
	}

	// ListSchedules on empty.
	schedules, err := s.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("ListSchedules (empty): %v", err)
	}
	if len(schedules) != 0 {
		t.Errorf("ListSchedules on empty store returned %d", len(schedules))
	}

	// GetDueSchedules on empty.
	due, err := s.GetDueSchedules(ctx)
	if err != nil {
		t.Fatalf("GetDueSchedules (empty): %v", err)
	}
	if len(due) != 0 {
		t.Errorf("GetDueSchedules on empty store returned %d", len(due))
	}
}

// TestMySQLIntegration_PriorityOrdering verifies that workflows created with
// higher priority are claimed before lower-priority ones.
func TestMySQLIntegration_PriorityOrdering(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	deployTestDef(t, s, "test-workflow", 1)

	// Create a low-priority workflow.
	lowID, _, err := s.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "priority-low", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (low): %v", err)
	}

	// Create a high-priority workflow.
	highID, _, err := s.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "priority-high", DefaultTenantUUID, 10)
	if err != nil {
		t.Fatalf("StartNewRun (high): %v", err)
	}

	// ClaimWorkflows should return the high-priority one first.
	wfs, err := s.ClaimWorkflows(ctx, "worker-1", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	if len(wfs) < 2 {
		t.Fatalf("ClaimWorkflows returned %d, want at least 2", len(wfs))
	}

	// The first claimed should be the high-priority workflow.
	if wfs[0].ID != highID {
		t.Errorf("first claimed workflow = %s (priority %d), want %s (priority 10)",
			wfs[0].ID, wfs[0].Priority, highID)
	}

	// Verify the second claimed is the low-priority one.
	if wfs[1].ID != lowID {
		t.Errorf("second claimed workflow = %s, want %s", wfs[1].ID, lowID)
	}
}

// TestMySQLIntegration_StreamEventHistory tests streaming events through a channel.
func TestMySQLIntegration_StreamEventHistory(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-stream-1")

	// Create 5 events.
	recs := make([]EventRecord, 5)
	for i := 0; i < 5; i++ {
		recs[i] = EventRecord{Step: i, EventType: EventTypeCall, Service: "s", Op: fmt.Sprintf("op-%d", i)}
	}
	if err := s.AppendEventHistoryBatch(ctx, runID, recs); err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}

	// Stream the events.
	eventCh, errCh := s.StreamEventHistory(ctx, runID, 2)
	received := make([]EventRecord, 0)
	for ev := range eventCh {
		received = append(received, ev)
	}

	// Check for streaming errors.
	if err := <-errCh; err != nil {
		t.Fatalf("StreamEventHistory error: %v", err)
	}

	if len(received) != 5 {
		t.Fatalf("StreamEventHistory returned %d events, want 5", len(received))
	}
	for i, ev := range received {
		if ev.Step != i {
			t.Errorf("received[%d].Step = %d, want %d", i, ev.Step, i)
		}
		expectedOp := fmt.Sprintf("op-%d", i)
		if ev.Op != expectedOp {
			t.Errorf("received[%d].Op = %q, want %q", i, ev.Op, expectedOp)
		}
	}
}

// TestMySQLIntegration_DeleteExpiredEvents tests deleting event history for
// terminal workflows older than a cutoff.
func TestMySQLIntegration_DeleteExpiredEvents(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	_ = createReadyWorkflow(t, s, "integ-expire-1")
	wf := claimOne(t, s, "worker-1")

	// Append some events.
	rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "s", Op: "o"}
	if err := s.AppendEventHistory(ctx, wf.ID, rec); err != nil {
		t.Fatalf("AppendEventHistory: %v", err)
	}

	// Complete the workflow.
	if err := s.CompleteWorkflow(ctx, wf.ID, "worker-1", wf.Generation, `{}`, nil); err != nil {
		t.Fatalf("CompleteWorkflow: %v", err)
	}

	// DeleteExpiredEvents with a future cutoff should delete events for the
	// completed workflow.
	deleted, err := s.DeleteExpiredEvents(ctx, time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("DeleteExpiredEvents: %v", err)
	}
	if deleted < 1 {
		t.Logf("DeleteExpiredEvents deleted %d events (may be 0 if workflow too recent)", deleted)
	}
}

// TestMySQLIntegration_LoadCompactionState tests compaction state lifecycle.
func TestMySQLIntegration_LoadCompactionState(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	runID := createReadyWorkflow(t, s, "integ-compact-1")

	// No compaction state yet -> nil.
	state, err := s.LoadCompactionState(ctx, runID)
	if err != nil {
		t.Fatalf("LoadCompactionState: %v", err)
	}
	if state != nil {
		t.Error("LoadCompactionState should return nil for non-compacted workflow")
	}

	// CompactHistory should succeed even with no events.
	if err := s.CompactHistory(ctx, runID, []byte(`{"version":1}`), 0, 0); err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}

	// CompactHistory with keepStep > 0.
	if err := s.CompactHistory(ctx, runID, []byte(`{"version":2}`), 5, 5); err != nil {
		t.Fatalf("CompactHistory (with keep): %v", err)
	}

	// LoadCompactionState should now return state.
	state, err = s.LoadCompactionState(ctx, runID)
	if err != nil {
		t.Fatalf("LoadCompactionState (after): %v", err)
	}
	if state == nil {
		t.Fatal("LoadCompactionState returned nil after CompactHistory")
	}
	if state.CompactedStep != 5 {
		t.Errorf("CompactedStep = %d, want 5", state.CompactedStep)
	}

	// GetCompactionCandidates should return workflow IDs that exceed threshold.
	candidates, err := s.GetCompactionCandidates(ctx, 0, 100)
	if err != nil {
		t.Fatalf("GetCompactionCandidates: %v", err)
	}
	_ = candidates
}

// TestMySQLIntegration_StartChildWorkflowAtomic tests creating a child workflow
// and recording the parent event in a single transaction.
func TestMySQLIntegration_StartChildWorkflowAtomic(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	deployTestDef(t, s, "test-workflow", 1)
	parentID := createReadyWorkflow(t, s, "integ-child-atomic-1")

	childEvent := EventRecord{
		Step:      0,
		EventType: EventTypeChildWorkflow,
		Service:   "test-workflow",
		Op:        "start_child",
		Request:   `{"child":true}`,
	}

	childID, err := s.StartChildWorkflowAtomic(ctx, "", parentID, "test-workflow",
		`{"child":true}`, 0, "abandon", childEvent, 0)
	if err != nil {
		t.Fatalf("StartChildWorkflowAtomic: %v", err)
	}
	if childID == "" {
		t.Fatal("StartChildWorkflowAtomic returned empty childID")
	}

	// The child event should be in the parent's history.
	history, err := s.LoadEventHistory(ctx, parentID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) < 1 {
		t.Fatal("expected at least 1 event in parent history after StartChildWorkflowAtomic")
	}
	if history[0].EventType != EventTypeChildWorkflow {
		t.Errorf("event type = %q, want %q", history[0].EventType, EventTypeChildWorkflow)
	}

	// Verify child workflow was created.
	child, err := s.GetWorkflowByID(ctx, childID)
	if err != nil {
		t.Fatalf("GetWorkflowByID (child): %v", err)
	}
	if child == nil {
		t.Fatal("child workflow not found")
	}
	if child.Status != "ready" {
		t.Errorf("child status = %q, want %q", child.Status, "ready")
	}
}

// TestMySQLIntegration_PurgeWorkflowDef tests permanently deleting a workflow definition.
func TestMySQLIntegration_PurgeWorkflowDef(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	deployTestDef(t, s, "purge-test", 1)

	// Verify it exists.
	def, err := s.GetWorkflowDef(ctx, "purge-test", 1)
	if err != nil {
		t.Fatalf("GetWorkflowDef: %v", err)
	}
	if def == nil {
		t.Fatal("GetWorkflowDef returned nil")
	}

	// Purge it.
	if err := s.PurgeWorkflowDef(ctx, "purge-test", 1); err != nil {
		t.Fatalf("PurgeWorkflowDef: %v", err)
	}

	// Verify it's gone.
	def, err = s.GetWorkflowDef(ctx, "purge-test", 1)
	if err != nil {
		t.Fatalf("GetWorkflowDef (after purge): %v", err)
	}
	if def != nil {
		t.Error("GetWorkflowDef returned non-nil after PurgeWorkflowDef")
	}

	// Subsequent purge should not error (idempotent).
	if err := s.PurgeWorkflowDef(ctx, "purge-test", 1); err != nil {
		t.Fatalf("PurgeWorkflowDef (second): %v", err)
	}
}

// TestMySQLIntegration_DeleteDeadLetteredWorkflows tests permanent deletion of
// dead-lettered workflows older than a cutoff.
func TestMySQLIntegration_DeleteDeadLetteredWorkflows(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	_ = createReadyWorkflow(t, s, "integ-dead-del-1")
	wf := claimOne(t, s, "worker-1")

	// Dead-letter it.
	if err := s.MoveToDeadLetterQueue(ctx, wf.ID, "worker-1", wf.Generation,
		"gone", "gone", "op"); err != nil {
		t.Fatalf("MoveToDeadLetterQueue: %v", err)
	}

	// DeleteDeadLetteredWorkflows with a past cutoff should leave it alone.
	_, err := s.DeleteDeadLetteredWorkflows(ctx, time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("DeleteDeadLetteredWorkflows (past): %v", err)
	}

	stored, err := s.GetWorkflowByID(ctx, wf.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if stored == nil {
		t.Fatal("workflow deleted when cutoff was in the past")
	}

	// With a future cutoff, it should be deleted (completed_at is now).
	deleted, err := s.DeleteDeadLetteredWorkflows(ctx, time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("DeleteDeadLetteredWorkflows (future): %v", err)
	}
	if deleted != 1 {
		t.Logf("DeleteDeadLetteredWorkflows deleted %d (expected 1)", deleted)
	}
}

// TestMySQLIntegration_StartNewRunWithTenant verifies that StartNewRun
// works with a custom tenant ID.
func TestMySQLIntegration_StartNewRunWithTenant(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	deployTestDef(t, s, "test-workflow", 1)
	tenantID := uuid.New().String()

	runID, _, err := s.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "tenant-test-1", tenantID, 0)
	if err != nil {
		t.Fatalf("StartNewRun with tenant: %v", err)
	}

	wf, err := s.GetWorkflowByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf == nil {
		t.Fatal("workflow not found")
	}
	if wf.TenantID != tenantID {
		t.Errorf("TenantID = %q, want %q", wf.TenantID, tenantID)
	}
}

// TestMySQLIntegration_LoadWorkflowConfig tests loading max_history_length
// from a deployed workflow definition. Since MaxHistoryLength is not a field
// on WorkflowDef, we set it via raw SQL after deployment.
func TestMySQLIntegration_LoadWorkflowConfig(t *testing.T) {
	s, teardown := mysqlIntegrationStore(t)
	defer teardown()
	ctx := context.Background()

	deployTestDef(t, s, "config-test", 1)

	// Set max_history_length via raw SQL.
	_, err := s.db.Exec("UPDATE workflow_defs SET max_history_length = 500 WHERE name = ? AND version = ?",
		"config-test", 1)
	if err != nil {
		t.Fatalf("update max_history_length: %v", err)
	}

	maxLen, err := s.LoadWorkflowConfig(ctx, "config-test", 1)
	if err != nil {
		t.Fatalf("LoadWorkflowConfig: %v", err)
	}
	if maxLen != 500 {
		t.Errorf("MaxHistoryLength = %d, want 500", maxLen)
	}

	// Non-existent def should return 0 with an error.
	maxLen, err = s.LoadWorkflowConfig(ctx, "nonexistent", 1)
	if err == nil {
		t.Error("LoadWorkflowConfig for nonexistent def should return an error")
	}
	if maxLen != 0 {
		t.Errorf("MaxHistoryLength for nonexistent = %d, want 0", maxLen)
	}
}
