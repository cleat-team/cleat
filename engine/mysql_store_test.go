package engine

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/google/uuid"

	_ "github.com/go-sql-driver/mysql"
)

// ---------------------------------------------------------------------------
// Non-DB unit tests — constructor defaults
// ---------------------------------------------------------------------------

func TestMySQLStore_NewStoreDefaults(t *testing.T) {
	store := NewMySQLStore(nil)
	if store.tenantID != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("default tenantID = %q, want zero UUID", store.tenantID)
	}
	if store.dialect != DialectMySQL {
		t.Fatalf("dialect = %q, want mysql", store.dialect)
	}
	if len(store.taskQueues) != 1 || store.taskQueues[0] != "default" {
		t.Fatalf("taskQueues = %v, want [default]", store.taskQueues)
	}
	if store.idempotencyKeyTTL != 720*time.Hour {
		t.Fatalf("idempotencyKeyTTL = %v, want 720h", store.idempotencyKeyTTL)
	}
	if store.encryptSensitivePayloads {
		t.Fatal("encryptSensitivePayloads should default to false")
	}
	if store.disableReadRedaction {
		t.Fatal("disableReadRedaction should default to false")
	}
	if store.encryption != nil {
		t.Fatal("encryption should default to nil")
	}
}

func TestMySQLStore_NewStoreTaskQueues(t *testing.T) {
	store := NewMySQLStore(nil, "gpu", "high-memory")
	if len(store.taskQueues) != 2 {
		t.Fatalf("taskQueues len = %d, want 2", len(store.taskQueues))
	}
	if store.taskQueues[0] != "gpu" || store.taskQueues[1] != "high-memory" {
		t.Fatalf("taskQueues = %v, want [gpu high-memory]", store.taskQueues)
	}
}

// ---------------------------------------------------------------------------
// WithTenant — copy-on-write
// ---------------------------------------------------------------------------

func TestMySQLStore_WithTenant(t *testing.T) {
	store := NewMySQLStore(nil, "default")

	scoped := store.WithTenant("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if scoped.tenantID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("scoped tenantID = %q", scoped.tenantID)
	}
	if store.tenantID != "00000000-0000-0000-0000-000000000000" {
		t.Fatal("WithTenant mutated original store")
	}
	if scoped.taskQueues[0] != store.taskQueues[0] {
		t.Fatal("WithTenant changed taskQueues")
	}
	if scoped.dialect != store.dialect {
		t.Fatal("WithTenant changed dialect")
	}
}

// ---------------------------------------------------------------------------
// Builder methods — copy-on-write
// ---------------------------------------------------------------------------

func TestMySQLStore_WithIdempotencyKeyTTL(t *testing.T) {
	store := NewMySQLStore(nil)
	updated := store.WithIdempotencyKeyTTL(24 * time.Hour)

	if store.idempotencyKeyTTL != 720*time.Hour {
		t.Fatal("original store was mutated")
	}
	if updated.idempotencyKeyTTL != 24*time.Hour {
		t.Fatalf("idempotencyKeyTTL = %v, want 24h", updated.idempotencyKeyTTL)
	}
}

func TestMySQLStore_WithReadRedactionDisabled(t *testing.T) {
	store := NewMySQLStore(nil)

	disabled := store.WithReadRedactionDisabled(true)
	if !disabled.disableReadRedaction {
		t.Fatal("disableReadRedaction should be true")
	}
	if store.disableReadRedaction {
		t.Fatal("original store was mutated")
	}

	reEnabled := disabled.WithReadRedactionDisabled(false)
	if reEnabled.disableReadRedaction {
		t.Fatal("disableReadRedaction should be false after re-enabling")
	}
}

func TestMySQLStore_WithEncryption(t *testing.T) {
	store := NewMySQLStore(nil)

	enc := &PayloadEncryption{key: make([]byte, 32)}
	encrypted := store.WithEncryption(enc, true)

	if encrypted.encryption != enc {
		t.Fatal("encryption reference not stored")
	}
	if !encrypted.encryptSensitivePayloads {
		t.Fatal("encryptSensitivePayloads should be true")
	}
	if store.encryptSensitivePayloads {
		t.Fatal("original store was mutated")
	}
	if store.encryption != nil {
		t.Fatal("original store encryption should be nil")
	}

	disabled := encrypted.WithEncryption(enc, false)
	if disabled.encryptSensitivePayloads {
		t.Fatal("encryptSensitivePayloads should be false after disabling")
	}
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

func TestMySQLStore_InClausePlaceholders(t *testing.T) {
	tests := []struct {
		name string
		n    int
		want string
	}{
		{"zero elements", 0, ""},
		{"one element", 1, "?"},
		{"two elements", 2, "?, ?"},
		{"five elements", 5, "?, ?, ?, ?, ?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inClausePlaceholders(tt.n)
			if got != tt.want {
				t.Errorf("inClausePlaceholders(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}

func TestMySQLStore_TaskQueueClause(t *testing.T) {
	tests := []struct {
		name       string
		taskQueues []string
		want       string
	}{
		{"default queue", []string{"default"}, "?"},
		{"single custom queue", []string{"gpu"}, "?"},
		{"multiple queues", []string{"default", "gpu", "high-memory"}, "?, ?, ?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &MySQLStore{taskQueues: tt.taskQueues}
			clause, args := store.taskQueueClause()
			if clause != tt.want {
				t.Errorf("taskQueueClause() clause = %q, want %q", clause, tt.want)
			}
			if len(args) != len(tt.taskQueues) {
				t.Errorf("taskQueueClause() args len = %d, want %d", len(args), len(tt.taskQueues))
			}
			for i, tq := range tt.taskQueues {
				if args[i] != tq {
					t.Errorf("taskQueueClause() args[%d] = %v, want %v", i, args[i], tq)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MySQLStoreFactory — non-DB tests
// ---------------------------------------------------------------------------

func TestMySQLStoreFactory_DriverName(t *testing.T) {
	factory := NewMySQLStoreFactory(nil, "user:pass@tcp(localhost:3306)/")
	if factory.DriverName() != "mysql" {
		t.Fatalf("DriverName = %q, want mysql", factory.DriverName())
	}
}

func TestMySQLStoreFactory_Dialect(t *testing.T) {
	factory := NewMySQLStoreFactory(nil, "user:pass@tcp(localhost:3306)/")
	if factory.Dialect() != DialectMySQL {
		t.Fatalf("Dialect = %q, want mysql", factory.Dialect())
	}
}

func TestMySQLStoreFactory_Defaults(t *testing.T) {
	factory := NewMySQLStoreFactory(nil, "user:pass@tcp(localhost:3306)/")
	if factory.idempotencyKeyTTL != 720*time.Hour {
		t.Fatalf("default idempotencyKeyTTL = %v, want 720h", factory.idempotencyKeyTTL)
	}
	if factory.tenantDBs == nil {
		t.Fatal("tenantDBs map should be initialized")
	}
	if len(factory.tenantDBs) != 0 {
		t.Fatalf("tenantDBs should be empty, got %d entries", len(factory.tenantDBs))
	}
}

func TestMySQLStoreFactory_CustomIdempotencyKeyTTL(t *testing.T) {
	factory := NewMySQLStoreFactory(nil, "user:pass@tcp(localhost:3306)/", 48*time.Hour)
	if factory.idempotencyKeyTTL != 48*time.Hour {
		t.Fatalf("idempotencyKeyTTL = %v, want 48h", factory.idempotencyKeyTTL)
	}
}

func TestMySQLStoreFactory_BuildTenantDSN(t *testing.T) {
	tests := []struct {
		name    string
		baseDSN string
		dbName  string
		want    string
	}{
		{
			name:    "standard DSN",
			baseDSN: "root:pass@tcp(127.0.0.1:3306)/?parseTime=true",
			dbName:  "cleat_mytenant",
			want:    "root:pass@tcp(127.0.0.1:3306)/cleat_mytenant?parseTime=true",
		},
		{
			name:    "DSN with multiStatements",
			baseDSN: "root:pass@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true",
			dbName:  "cleat_test",
			want:    "root:pass@tcp(127.0.0.1:3306)/cleat_test?parseTime=true&multiStatements=true",
		},
		{
			name:    "DSN without slash — db appended directly",
			baseDSN: "root:pass@tcp(127.0.0.1:3306)",
			dbName:  "cleat_db",
			want:    "root:pass@tcp(127.0.0.1:3306)cleat_db",
		},
		{
			name:    "DSN without query params",
			baseDSN: "root:pass@tcp(127.0.0.1:3306)/",
			dbName:  "cleat_db",
			want:    "root:pass@tcp(127.0.0.1:3306)/cleat_db",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factory := NewMySQLStoreFactory(nil, tt.baseDSN)
			got := factory.buildTenantDSN(tt.dbName)
			if got != tt.want {
				t.Errorf("buildTenantDSN(%q) = %q, want %q", tt.dbName, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// DB-gated tests (require CLEAT_TEST_MYSQL and a running MySQL 8.0+ instance)
// ---------------------------------------------------------------------------

func skipUnlessMySQL(t *testing.T) {
	t.Helper()
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}
}

// TestMySQLStore_BasicConnection verifies that MySQLStore can be created and
// connected. Tests the full NewMySQLStore → schema setup → cleanup flow.
func TestMySQLStore_BasicConnection(t *testing.T) {
	skipUnlessMySQL(t)

	db := testutil.MySQLTestDB(t)
	defer db.Close()
	testutil.SetupMySQLFullSchema(t, db)
	defer testutil.CleanupMySQLTestData(t, db)

	store := NewMySQLStore(db, "default")
	if store == nil {
		t.Fatal("NewMySQLStore returned nil")
	}
	if store.db == nil {
		t.Fatal("store.db is nil")
	}
	if len(store.taskQueues) != 1 || store.taskQueues[0] != "default" {
		t.Fatalf("unexpected taskQueues: %v", store.taskQueues)
	}
}

// TestMySQLStore_DeployAndListWorkflowDefs tests MySQL-specific ON DUPLICATE KEY
// UPDATE behavior in DeployWorkflowDef and listing via ListVersions.
func TestMySQLStore_DeployAndListWorkflowDefs(t *testing.T) {
	skipUnlessMySQL(t)

	db := testutil.MySQLTestDB(t)
	defer db.Close()
	testutil.SetupMySQLFullSchema(t, db)
	defer testutil.CleanupMySQLTestData(t, db)

	store := NewMySQLStore(db, "default")
	ctx := t.Context()

	def := &WorkflowDef{
		Name:       "test-deploy-wf",
		Version:    1,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1,
		MinVersion: 1,
	}
	if err := store.DeployWorkflowDef(ctx, def); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	// Re-deploy same version (exercises ON DUPLICATE KEY UPDATE).
	def.WASMBytes = []byte{0x00, 0x61, 0x73, 0x6d, 0x01}
	if err := store.DeployWorkflowDef(ctx, def); err != nil {
		t.Fatalf("DeployWorkflowDef (re-deploy): %v", err)
	}

	versions, err := store.ListVersions(ctx, "test-deploy-wf")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("expected 1 version, got %d", len(versions))
	}
	if versions[0] != 1 {
		t.Fatalf("version = %d, want 1", versions[0])
	}
}

// TestMySQLStore_StartNewRun verifies workflow creation including idempotency key
// insertion into the MySQL-specific idempotency_keys table.
func TestMySQLStore_StartNewRun(t *testing.T) {
	skipUnlessMySQL(t)

	db := testutil.MySQLTestDB(t)
	defer db.Close()
	testutil.SetupMySQLFullSchema(t, db)
	defer testutil.CleanupMySQLTestData(t, db)

	store := NewMySQLStore(db, "default")
	ctx := t.Context()

	deployTestWorkflowDef(t, store, ctx, "test-start-wf")

	// Non-idempotent path.
	runID := uuid.New().String()
	id, isDup, err := store.StartNewRun(ctx, runID, "test-start-wf", 1,
		json.RawMessage(`{"step":"start"}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (no idemkey): %v", err)
	}
	if isDup {
		t.Fatal("StartNewRun (no idemkey): unexpected duplicate")
	}
	if id != runID {
		t.Fatalf("StartNewRun (no idemkey): got %s, want %s", id, runID)
	}

	// Idempotency-key path.
	idemRunID := uuid.New().String()
	id2, isDup2, err := store.StartNewRun(ctx, idemRunID, "test-start-wf", 1,
		json.RawMessage(`{"step":"start"}`), "idem-key-001", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (idemkey): %v", err)
	}
	if isDup2 {
		t.Fatal("StartNewRun (idemkey): unexpected duplicate")
	}
	if id2 != idemRunID {
		t.Fatalf("StartNewRun (idemkey): got %s, want %s", id2, idemRunID)
	}
}

// TestMySQLStore_CompleteWorkflow verifies workflow completion.
func TestMySQLStore_CompleteWorkflow(t *testing.T) {
	skipUnlessMySQL(t)

	db := testutil.MySQLTestDB(t)
	defer db.Close()
	testutil.SetupMySQLFullSchema(t, db)
	defer testutil.CleanupMySQLTestData(t, db)

	store := NewMySQLStore(db, "default")
	ctx := t.Context()

	deployTestWorkflowDef(t, store, ctx, "test-complete-wf")

	runID := uuid.New().String()
	id, isDup, err := store.StartNewRun(ctx, runID, "test-complete-wf", 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	if isDup {
		t.Fatal("unexpected duplicate")
	}

	// Claim the workflow to set it running.
	wf, err := store.ClaimWorkflow(ctx, "test-worker")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if wf == nil {
		t.Fatal("ClaimWorkflow returned nil, workflow not claimed")
	}

	// Complete it.
	if err := store.CompleteWorkflow(ctx, id, "test-worker", wf.Generation, `{"status":"done"}`, nil); err != nil {
		t.Fatalf("CompleteWorkflow: %v", err)
	}

	// Verify terminal status.
	var storedStatus string
	err = db.QueryRow(`SELECT status FROM workflow_instances WHERE id = ?`, id).Scan(&storedStatus)
	if err != nil {
		t.Fatalf("query status: %v", err)
	}
	if storedStatus != "completed" {
		t.Fatalf("status = %q, want completed", storedStatus)
	}
}

// TestMySQLStore_FailWorkflow verifies workflow failure.
func TestMySQLStore_FailWorkflow(t *testing.T) {
	skipUnlessMySQL(t)

	db := testutil.MySQLTestDB(t)
	defer db.Close()
	testutil.SetupMySQLFullSchema(t, db)
	defer testutil.CleanupMySQLTestData(t, db)

	store := NewMySQLStore(db, "default")
	ctx := t.Context()

	deployTestWorkflowDef(t, store, ctx, "test-fail-wf")

	runID := uuid.New().String()
	id, _, err := store.StartNewRun(ctx, runID, "test-fail-wf", 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	wf, err := store.ClaimWorkflow(ctx, "test-worker")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if wf == nil {
		t.Fatal("ClaimWorkflow returned nil")
	}

	errMsg := "something went wrong"
	if err := store.FailWorkflow(ctx, id, "test-worker", wf.Generation, errMsg, "", "", nil); err != nil {
		t.Fatalf("FailWorkflow: %v", err)
	}

	var storedStatus, storedErr string
	err = db.QueryRow(`SELECT status, error_msg FROM workflow_instances WHERE id = ?`, id).Scan(&storedStatus, &storedErr)
	if err != nil {
		t.Fatalf("query status: %v", err)
	}
	if storedStatus != "failed" {
		t.Fatalf("status = %q, want failed", storedStatus)
	}
	if storedErr != errMsg {
		t.Fatalf("error_msg = %q, want %q", storedErr, errMsg)
	}
}

// TestMySQLStore_PromiseLifecycle tests the MySQL-specific INSERT IGNORE
// behavior in CreatePromise and the full resolve/reject/get lifecycle.
func TestMySQLStore_PromiseLifecycle(t *testing.T) {
	skipUnlessMySQL(t)

	db := testutil.MySQLTestDB(t)
	defer db.Close()
	testutil.SetupMySQLFullSchema(t, db)
	defer testutil.CleanupMySQLTestData(t, db)

	store := NewMySQLStore(db, "default")
	ctx := t.Context()

	deployTestWorkflowDef(t, store, ctx, "test-promise-wf")

	runID := uuid.New().String()
	id, _, err := store.StartNewRun(ctx, runID, "test-promise-wf", 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	// Create a promise (INSERT IGNORE path).
	promiseID := "promise-001"
	if err := store.CreatePromise(ctx, id, "await-result", promiseID); err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}

	// Duplicate create should be a no-op (INSERT IGNORE).
	if err := store.CreatePromise(ctx, id, "await-result", promiseID); err != nil {
		t.Fatalf("CreatePromise (dup): %v", err)
	}

	// Get pending promise.
	status, result, errMsg, err := store.GetPromise(ctx, id, promiseID)
	if err != nil {
		t.Fatalf("GetPromise (pending): %v", err)
	}
	if status != "pending" {
		t.Fatalf("promise status = %q, want pending", status)
	}

	// Resolve the promise.
	resolveResult := `{"answer":42}`
	if err := store.ResolvePromise(ctx, id, promiseID, resolveResult); err != nil {
		t.Fatalf("ResolvePromise: %v", err)
	}

	// Get resolved promise.
	status, result, errMsg, err = store.GetPromise(ctx, id, promiseID)
	if err != nil {
		t.Fatalf("GetPromise (resolved): %v", err)
	}
	if status != "resolved" {
		t.Fatalf("promise status = %q, want resolved", status)
	}
	if result != resolveResult {
		t.Fatalf("promise result = %q, want %q", result, resolveResult)
	}
	if errMsg != "" {
		t.Fatalf("promise errMsg = %q, want empty", errMsg)
	}
}

// TestMySQLStore_ScheduleLifecycle tests CREATE, LIST, GET_DUE, UPDATE_NEXT_RUN,
// SET_ENABLED, and DELETE for schedules.
func TestMySQLStore_ScheduleLifecycle(t *testing.T) {
	skipUnlessMySQL(t)

	db := testutil.MySQLTestDB(t)
	defer db.Close()
	testutil.SetupMySQLFullSchema(t, db)
	defer testutil.CleanupMySQLTestData(t, db)

	store := NewMySQLStore(db, "default")
	ctx := t.Context()

	deployTestWorkflowDef(t, store, ctx, "test-sched-wf")

	now := time.Now()
	schedule := Schedule{
		Name:           "test-schedule",
		DefName:        "test-sched-wf",
		EntryPoint:     "main",
		CronExpression: "0 * * * *",
		Input:          json.RawMessage(`{"cron":"hourly"}`),
		Enabled:        true,
		NextRunAt:      now.Add(-1 * time.Hour),
	}
	if err := store.CreateSchedule(ctx, schedule); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	// List schedules.
	schedules, err := store.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	found := false
	for _, s := range schedules {
		if s.Name == "test-schedule" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("created schedule not found in ListSchedules")
	}

	// Get due schedules (should find our past-due schedule).
	due, err := store.GetDueSchedules(ctx)
	if err != nil {
		t.Fatalf("GetDueSchedules: %v", err)
	}
	if len(due) == 0 {
		t.Fatal("GetDueSchedules returned empty, expected at least 1 due schedule")
	}

	// Update next run.
	nextRun := now.Add(2 * time.Hour)
	if err := store.UpdateScheduleNextRun(ctx, "test-schedule", nextRun); err != nil {
		t.Fatalf("UpdateScheduleNextRun: %v", err)
	}

	// Disable schedule.
	if err := store.SetScheduleEnabled(ctx, "test-schedule", false); err != nil {
		t.Fatalf("SetScheduleEnabled(false): %v", err)
	}

	// Verify disabled.
	schedules, _ = store.ListSchedules(ctx)
	for _, s := range schedules {
		if s.Name == "test-schedule" && s.Enabled {
			t.Fatal("schedule should be disabled")
		}
	}

	// Delete schedule.
	if err := store.DeleteSchedule(ctx, "test-schedule"); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}

	schedules, _ = store.ListSchedules(ctx)
	for _, s := range schedules {
		if s.Name == "test-schedule" {
			t.Fatal("schedule should be deleted")
		}
	}
}

// TestMySQLStore_ConcurrencyKeyLifecycle tests Acquire, Release, and Reap
// for concurrency keys, exercising the MySQL-specific INSERT IGNORE and
// SHA-256 hashing path.
func TestMySQLStore_ConcurrencyKeyLifecycle(t *testing.T) {
	skipUnlessMySQL(t)

	db := testutil.MySQLTestDB(t)
	defer db.Close()
	testutil.SetupMySQLFullSchema(t, db)
	defer testutil.CleanupMySQLTestData(t, db)

	store := NewMySQLStore(db, "default")
	ctx := t.Context()

	deployTestWorkflowDef(t, store, ctx, "test-ck-wf")

	runID := uuid.New().String()
	id, _, err := store.StartNewRun(ctx, runID, "test-ck-wf", 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	// Acquire a concurrency key (INSERT IGNORE path + SHA-256 hash).
	acquired, err := store.AcquireConcurrencyKey(ctx, "my-lock", id, 10*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Fatal("AcquireConcurrencyKey should succeed on first acquire")
	}

	// Second acquire by same workflow should succeed (same owner).
	acquired2, err := store.AcquireConcurrencyKey(ctx, "my-lock", id, 10*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey (2nd): %v", err)
	}
	if !acquired2 {
		t.Fatal("AcquireConcurrencyKey should succeed for same owner")
	}

	// Release the key.
	if err := store.ReleaseConcurrencyKey(ctx, "my-lock"); err != nil {
		t.Fatalf("ReleaseConcurrencyKey: %v", err)
	}

	// Reap should not find our released key.
	reaped, err := store.ReapExpiredConcurrencyKeys(ctx)
	if err != nil {
		t.Fatalf("ReapExpiredConcurrencyKeys: %v", err)
	}
	if reaped != 0 {
		t.Fatalf("ReapExpiredConcurrencyKeys: reaped %d, expected 0", reaped)
	}
}

// TestMySQLStore_AppendAndLoadEventHistory verifies event history append and load
// including the MySQL-specific NOW(6) timestamp precision.
func TestMySQLStore_AppendAndLoadEventHistory(t *testing.T) {
	skipUnlessMySQL(t)

	db := testutil.MySQLTestDB(t)
	defer db.Close()
	testutil.SetupMySQLFullSchema(t, db)
	defer testutil.CleanupMySQLTestData(t, db)

	store := NewMySQLStore(db, "default")
	ctx := t.Context()

	deployTestWorkflowDef(t, store, ctx, "test-events-wf")

	runID := uuid.New().String()
	id, _, err := store.StartNewRun(ctx, runID, "test-events-wf", 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	// Append first event.
	rec1 := EventRecord{
		Step:      1,
		EventType: "call",
		Service:   "svc-a",
		Op:        "op-a",
		Request:   `{"arg":1}`,
	}
	if err := store.AppendEventHistory(ctx, id, rec1); err != nil {
		t.Fatalf("AppendEventHistory (first): %v", err)
	}

	// Append second event.
	rec2 := EventRecord{
		Step:      2,
		EventType: "rpc",
		Service:   "svc-b",
		Op:        "op-b",
		Request:   `{"arg":2}`,
		Response:  `{"res":2}`,
	}
	if err := store.AppendEventHistory(ctx, id, rec2); err != nil {
		t.Fatalf("AppendEventHistory (second): %v", err)
	}

	// Load event history.
	events, err := store.LoadEventHistory(ctx, id)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// Verify event ordering and content.
	if events[0].Step != 1 || events[1].Step != 2 {
		t.Fatalf("unexpected event steps: %d, %d", events[0].Step, events[1].Step)
	}
	if events[0].Request != `{"arg":1}` {
		t.Fatalf("event[0] Request = %s", events[0].Request)
	}
	if events[1].Response != `{"res":2}` {
		t.Fatalf("event[1] Response = %s", events[1].Response)
	}
}

// TestMySQLStore_SignalDelivery tests signal delivery and polling including
// the MySQL-specific CAST(result AS CHAR) for JSON column handling.
func TestMySQLStore_SignalDelivery(t *testing.T) {
	skipUnlessMySQL(t)

	db := testutil.MySQLTestDB(t)
	defer db.Close()
	testutil.SetupMySQLFullSchema(t, db)
	defer testutil.CleanupMySQLTestData(t, db)

	store := NewMySQLStore(db, "default")
	ctx := t.Context()

	deployTestWorkflowDef(t, store, ctx, "test-signal-wf")

	runID := uuid.New().String()
	id, _, err := store.StartNewRun(ctx, runID, "test-signal-wf", 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	// Deliver a signal.
	signalPayload := `{"msg":"hello"}`
	if err := store.DeliverSignal(ctx, id, "greeting", signalPayload); err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	// Poll for the signal.
	payload, found, err := store.PollSignal(ctx, id, "greeting")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if !found {
		t.Fatal("PollSignal: signal not found")
	}

	// Verify the signal payload survived the CAST(result AS CHAR) round-trip.
	if payload != signalPayload {
		t.Fatalf("signal payload = %s, want %s", payload, signalPayload)
	}
}

// TestMySQLStore_StartNewRun_Duplicate verifies that a second StartNewRun with
// the same idempotency key returns the original workflow ID (duplicate detection).
func TestMySQLStore_StartNewRun_Duplicate(t *testing.T) {
	skipUnlessMySQL(t)

	db := testutil.MySQLTestDB(t)
	defer db.Close()
	testutil.SetupMySQLFullSchema(t, db)
	defer testutil.CleanupMySQLTestData(t, db)

	store := NewMySQLStore(db, "default")
	ctx := t.Context()

	deployTestWorkflowDef(t, store, ctx, "test-dup-wf")

	runID := uuid.New().String()
	idemKey := "idem-key-dup-001"

	// First run.
	id1, isDup, err := store.StartNewRun(ctx, runID, "test-dup-wf", 1,
		json.RawMessage(`{}`), idemKey, DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (first): %v", err)
	}
	if isDup {
		t.Fatal("first run should not be duplicate")
	}

	// Second run with same idempotency key.
	id2, isDup, err := store.StartNewRun(ctx, uuid.New().String(), "test-dup-wf", 1,
		json.RawMessage(`{}`), idemKey, DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (dup): %v", err)
	}
	if !isDup {
		t.Fatal("second run should be flagged as duplicate")
	}
	if id2 != id1 {
		t.Fatalf("duplicate id = %q, want original id %q", id2, id1)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func deployTestWorkflowDef(t *testing.T, store WorkflowStore, ctx context.Context, name string) {
	t.Helper()
	def := &WorkflowDef{
		Name:       name,
		Version:    1,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1,
		MinVersion: 1,
	}
	if err := store.DeployWorkflowDef(ctx, def); err != nil {
		t.Fatalf("deployTestWorkflowDef(%s): %v", name, err)
	}
}
