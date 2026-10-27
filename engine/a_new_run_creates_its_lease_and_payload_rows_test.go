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
//
// The six fields below concurrencyKey were added for migration 018 (folded into migrations/postgres/001_schema.sql by #3358)
// (cleat#3245 Phase 3 step 3 piece B's blocker): concurrencyKey/
// concurrencyKeyHash/parentWorkflowID/continuedFrom/runWasmInstanceTimeoutMs
// mirror workflow_instances columns of the same shape that existed before
// this struct did, left out of the original SELECT because piece 1 never
// wrote them. All nullable here (sql.NullString/sql.NullInt64) because most
// creation paths leave most of them NULL -- see
// a_migration_018_columns_test.go for which path sets which.
type leaseAndPayloadRow struct {
	tenantID           string
	taskQueue          string
	priority           int
	status             string
	nextWake           sql.NullTime
	input              string
	concurrencyKey     sql.NullString
	concurrencyKeyHash []byte
	parentWorkflowID   sql.NullString
	continuedFrom      sql.NullString
	runInstanceMs      sql.NullInt64
}

func readLeaseRow(t *testing.T, db *sql.DB, id string) leaseAndPayloadRow {
	t.Helper()
	var r leaseAndPayloadRow
	if err := db.QueryRow(`
		SELECT tenant_id::text, task_queue, priority, status, next_wake_at,
		       concurrency_key, concurrency_key_hash, parent_workflow_id, continued_from,
		       run_wasm_instance_timeout_ms
		FROM workflow_leases WHERE id = $1
	`, id).Scan(&r.tenantID, &r.taskQueue, &r.priority, &r.status, &r.nextWake,
		&r.concurrencyKey, &r.concurrencyKeyHash, &r.parentWorkflowID, &r.continuedFrom,
		&r.runInstanceMs); err != nil {
		t.Fatalf("read workflow_leases for %s: %v", id, err)
	}
	return r
}

// readInstanceRow leaves concurrencyKey zero-valued: cleat#3360 dropped
// concurrency_key (the text) from workflow_instances -- workflow_leases has
// been its only reader since piece B. concurrencyKeyHash is still read here:
// concurrency_key_hash stays on workflow_instances for now, because
// idx_instances_concurrency_key (migrations/postgres/001_schema.sql) indexes
// it and 001 is frozen -- see engine/db.go's UpdateStickyWorker for the full
// reasoning. Callers that need concurrencyKey read readLeaseRow's copy
// instead.
func readInstanceRow(t *testing.T, db *sql.DB, id string) leaseAndPayloadRow {
	t.Helper()
	var r leaseAndPayloadRow
	if err := db.QueryRow(`
		SELECT tenant_id::text, task_queue, priority, status, next_wake_at,
		       concurrency_key_hash, parent_workflow_id, continued_from,
		       run_wasm_instance_timeout_ms
		FROM workflow_instances WHERE id = $1
	`, id).Scan(&r.tenantID, &r.taskQueue, &r.priority, &r.status, &r.nextWake,
		&r.concurrencyKeyHash, &r.parentWorkflowID, &r.continuedFrom,
		&r.runInstanceMs); err != nil {
		t.Fatalf("read workflow_instances for %s: %v", id, err)
	}
	return r
}

func readPayloadDef(t *testing.T, db *sql.DB, id string) (defName sql.NullString, defVersion sql.NullInt64) {
	t.Helper()
	if err := db.QueryRow(`SELECT def_name, def_version FROM workflow_payloads WHERE id = $1`, id).
		Scan(&defName, &defVersion); err != nil {
		t.Fatalf("read workflow_payloads.def_name/def_version for %s: %v", id, err)
	}
	return defName, defVersion
}

func readPayloadInput(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var input string
	if err := db.QueryRow(`SELECT input::text FROM workflow_payloads WHERE id = $1`, id).Scan(&input); err != nil {
		t.Fatalf("read workflow_payloads for %s: %v", id, err)
	}
	return input
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
	// cleat#3245 B1b: workflow_payloads holds the only copy of input; the
	// workflow_instances column keeps its default (asserted by
	// TestAPayloadWriteLeavesTheInstanceCopyUntouched).
	if gotInput == "{}" {
		t.Errorf("workflow_payloads.input = {} -- the run's input was not stored")
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
