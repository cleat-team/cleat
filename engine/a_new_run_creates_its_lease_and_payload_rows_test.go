package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 Phase 3 step 2, piece 1: startNewRun now creates a matching
// workflow_leases and workflow_payloads row in the same transaction as the
// workflow_instances row it has always created. This is the only place a
// pair is created -- there is no backfill (owner decision: 0.5.0 requires a
// fresh database, so no supported upgrade path has a pre-existing
// workflow_instances row to catch up) -- so if StartNewRun does not create
// both, nothing else in this step ever will.
//
// Both tables are PostgreSQL-only (the issue's own scoping decision), so
// this is a PostgreSQL-only test built directly on testutil.TestDB rather
// than the three-dialect registeredBackends harness store_backends_test.go
// uses elsewhere -- MySQL and MSSQL have no such tables to assert against.
func newRunTestStore(t *testing.T) (*PostgresStore, *sql.DB) {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	applyPostgresProcedures(t, db)
	testutil.CleanupPostgresTestData(t, db)
	t.Cleanup(func() {
		testutil.CleanupPostgresTestData(t, db)
		db.Close()
	})
	return NewPostgresStore(db), db
}

// leaseAndPayloadRow is what this test reads back from the two new tables,
// restricted to the columns piece 1 actually writes -- generation,
// assigned_to and the rest come later pieces, and asserting on them here
// would assert on columns this change does not touch.
type leaseAndPayloadRow struct {
	tenantID  string
	taskQueue string
	priority  int
	status    string
	nextWake  sql.NullTime
	input     string
}

func readLeaseRow(t *testing.T, db *sql.DB, id string) leaseAndPayloadRow {
	t.Helper()
	var r leaseAndPayloadRow
	if err := db.QueryRow(`
		SELECT tenant_id::text, task_queue, priority, status, next_wake_at
		FROM workflow_leases WHERE id = $1
	`, id).Scan(&r.tenantID, &r.taskQueue, &r.priority, &r.status, &r.nextWake); err != nil {
		t.Fatalf("read workflow_leases for %s: %v", id, err)
	}
	return r
}

func readInstanceRow(t *testing.T, db *sql.DB, id string) leaseAndPayloadRow {
	t.Helper()
	var r leaseAndPayloadRow
	if err := db.QueryRow(`
		SELECT tenant_id::text, task_queue, priority, status, next_wake_at
		FROM workflow_instances WHERE id = $1
	`, id).Scan(&r.tenantID, &r.taskQueue, &r.priority, &r.status, &r.nextWake); err != nil {
		t.Fatalf("read workflow_instances for %s: %v", id, err)
	}
	return r
}

func readPayloadInput(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var input string
	if err := db.QueryRow(`SELECT input::text FROM workflow_payloads WHERE id = $1`, id).Scan(&input); err != nil {
		t.Fatalf("read workflow_payloads for %s: %v", id, err)
	}
	return input
}

func TestStartNewRunCreatesMatchingLeaseAndPayloadRows(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "new-run-lease-payload-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	// task_queue has no field on WorkflowDef's Go API (COALESCE's subquery
	// reads the column directly), so set a NON-default value by hand -- a
	// test that only ever saw "default" could not tell a correctly captured
	// RETURNING value from a hardcoded one.
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_defs SET task_queue = 'gpu-queue' WHERE name = $1 AND version = 1 AND tenant_id = $2`,
		defName, DefaultTenantUUID); err != nil {
		t.Fatalf("set def task_queue: %v", err)
	}

	input := json.RawMessage(`{"order_id":"abc123"}`)
	runID, alreadyExisted, err := store.StartNewRun(ctx, "", defName, 1, input, "", DefaultTenantUUID, 7)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	if alreadyExisted {
		t.Fatal("alreadyExisted=true for a new run")
	}

	instRow := readInstanceRow(t, db, runID)
	leaseRow := readLeaseRow(t, db, runID)

	if leaseRow.tenantID != instRow.tenantID {
		t.Errorf("workflow_leases.tenant_id = %q, workflow_instances.tenant_id = %q, want equal", leaseRow.tenantID, instRow.tenantID)
	}
	if leaseRow.taskQueue != "gpu-queue" {
		t.Errorf("workflow_leases.task_queue = %q, want %q (the resolved value, not the literal default)", leaseRow.taskQueue, "gpu-queue")
	}
	if leaseRow.taskQueue != instRow.taskQueue {
		t.Errorf("workflow_leases.task_queue = %q, workflow_instances.task_queue = %q, want equal", leaseRow.taskQueue, instRow.taskQueue)
	}
	if leaseRow.priority != 7 || leaseRow.priority != instRow.priority {
		t.Errorf("workflow_leases.priority = %d, workflow_instances.priority = %d, want both 7", leaseRow.priority, instRow.priority)
	}
	if leaseRow.status != "ready" || leaseRow.status != instRow.status {
		t.Errorf("workflow_leases.status = %q, workflow_instances.status = %q, want both %q", leaseRow.status, instRow.status, "ready")
	}
	if !leaseRow.nextWake.Valid || !instRow.nextWake.Valid {
		t.Fatal("next_wake_at is NULL on one of the two tables")
	}
	// Both tables default next_wake_at to `now()`, which is the one
	// documented place the two could legitimately disagree -- the
	// workflow_instances INSERT overrides it to `now() - 1ms` and the dual
	// write must pass that SAME override through explicitly rather than
	// falling back to workflow_leases' own default. Equal, not merely
	// close: both statements run inside one transaction, where every
	// evaluation of now() returns the identical value.
	if !leaseRow.nextWake.Time.Equal(instRow.nextWake.Time) {
		t.Errorf("next_wake_at disagrees: workflow_leases=%v workflow_instances=%v -- the dual write must pass the SAME `now() - 1ms` the instances INSERT uses, not rely on this table's own DEFAULT now()",
			leaseRow.nextWake.Time, instRow.nextWake.Time)
	}

	gotInput := readPayloadInput(t, db, runID)
	var wantInput string
	if err := db.QueryRow(`SELECT input::text FROM workflow_instances WHERE id = $1`, runID).Scan(&wantInput); err != nil {
		t.Fatalf("read workflow_instances.input: %v", err)
	}
	if gotInput != wantInput {
		t.Errorf("workflow_payloads.input = %q, workflow_instances.input = %q, want byte-identical (same sealed value reused, not re-sealed)", gotInput, wantInput)
	}
}

// TestStartNewRunWithIdempotencyKeyAlsoCreatesLeaseAndPayloadRows covers the
// SECOND insert site in startNewRun -- the idempotency-key branch has its own
// copy of the workflow_instances INSERT (and now its own call to
// insertLeaseAndPayloadRows), and a fix applied to only one of the two
// branches would pass the test above while leaving this path uncovered.
func TestStartNewRunWithIdempotencyKeyAlsoCreatesLeaseAndPayloadRows(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "new-run-lease-payload-idem-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	input := json.RawMessage(`{"order_id":"idem-1"}`)
	runID, alreadyExisted, err := store.StartNewRun(ctx, "", defName, 1, input, "idem-key-3245", DefaultTenantUUID, 3)
	if err != nil {
		t.Fatalf("StartNewRun with idempotency key: %v", err)
	}
	if alreadyExisted {
		t.Fatal("alreadyExisted=true for a new run")
	}

	leaseRow := readLeaseRow(t, db, runID)
	if leaseRow.priority != 3 {
		t.Errorf("workflow_leases.priority = %d, want 3", leaseRow.priority)
	}
	gotInput := readPayloadInput(t, db, runID)
	var wantInput string
	if err := db.QueryRow(`SELECT input::text FROM workflow_instances WHERE id = $1`, runID).Scan(&wantInput); err != nil {
		t.Fatalf("read workflow_instances.input: %v", err)
	}
	if gotInput != wantInput {
		t.Errorf("workflow_payloads.input = %q, workflow_instances.input = %q, want byte-identical", gotInput, wantInput)
	}

	// A SECOND call with the same key must short-circuit before either
	// INSERT runs (the early `return existingWfID, true, nil`), not create a
	// second pair of rows for the same id -- which the primary key would
	// refuse anyway, but refuse with an error this call must not surface.
	runID2, alreadyExisted2, err := store.StartNewRun(ctx, "", defName, 1, input, "idem-key-3245", DefaultTenantUUID, 3)
	if err != nil {
		t.Fatalf("StartNewRun second call with same idempotency key: %v", err)
	}
	if !alreadyExisted2 || runID2 != runID {
		t.Fatalf("second call: alreadyExisted=%v runID=%q, want true and %q", alreadyExisted2, runID2, runID)
	}
}
