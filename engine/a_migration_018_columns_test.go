package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// cleat#3245 Phase 3 step 3 piece B's blocker, migration 018 (folded into migrations/postgres/001_schema.sql by #3358): every creation
// path that writes one of migration 018's eight new columns onto
// workflow_instances must now write the SAME value onto its
// workflow_leases/workflow_payloads mirror, in the same transaction, via
// insertLeaseAndPayloadRows' new leaseAndPayloadCols parameter. Each
// creation path exercises a different subset -- a fix applied to only one
// path's call site would pass that path's own test here while leaving the
// others silently wrong, same reasoning as the sibling test files for
// pieces 1 and the child-workflow gap fix.
//
// Every test below is PostgreSQL-only (migration 018 is Postgres-only, same
// as 016) and reuses newRunTestStore/readLeaseRow/readInstanceRow/
// readPayloadDef from a_new_run_creates_its_lease_and_payload_rows_test.go.

// TestStartNewRunWithConcurrencyKeyMirrorsItOntoLeases covers
// StartNewRunWithOptions' ConcurrencyKey -- the column piece B's claim
// predicate (acquireCandidateConcurrencyKey) actually needs on
// workflow_leases.
func TestStartNewRunWithConcurrencyKeyMirrorsItOntoLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "migration-018-concurrency-key-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	runID, _, err := store.StartNewRunWithConcurrencyKey(ctx, "", defName, 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0, "order-123")
	if err != nil {
		t.Fatalf("StartNewRunWithConcurrencyKey: %v", err)
	}

	instRow := readInstanceRow(t, db, runID)
	leaseRow := readLeaseRow(t, db, runID)

	// cleat#3360 dropped concurrency_key (the text) from workflow_instances
	// -- workflow_leases is the only copy left, so there is nothing to
	// assert on instRow.concurrencyKey any more. concurrency_key_hash stays
	// on workflow_instances for now (idx_instances_concurrency_key indexes
	// it, and migrations/postgres/001_schema.sql is frozen -- see
	// engine/db.go's UpdateStickyWorker), so the byte-identical check below
	// still holds.
	if !leaseRow.concurrencyKey.Valid || leaseRow.concurrencyKey.String != "order-123" {
		t.Fatalf("workflow_leases.concurrency_key = %+v, want valid %q", leaseRow.concurrencyKey, "order-123")
	}
	if len(instRow.concurrencyKeyHash) == 0 {
		t.Fatal("workflow_instances.concurrency_key_hash is empty (sanity on the existing write)")
	}
	if string(leaseRow.concurrencyKeyHash) != string(instRow.concurrencyKeyHash) {
		t.Errorf("workflow_leases.concurrency_key_hash = %x, workflow_instances.concurrency_key_hash = %x, want byte-identical", leaseRow.concurrencyKeyHash, instRow.concurrencyKeyHash)
	}

	defNameGot, defVersionGot := readPayloadDef(t, db, runID)
	if !defNameGot.Valid || defNameGot.String != defName {
		t.Errorf("workflow_payloads.def_name = %+v, want valid %q", defNameGot, defName)
	}
	if !defVersionGot.Valid || defVersionGot.Int64 != 1 {
		t.Errorf("workflow_payloads.def_version = %+v, want valid 1", defVersionGot)
	}
}

// TestStartNewRunWithOptionsMirrorsRunLimitsOntoLeases covers the four
// run_* override columns engine/run_limits.go's GetRunLimits reads --
// found outside the piece C/D/E/F breakdown by a blanket grep for every
// remaining FROM workflow_instances site, same creation INSERT as
// concurrency_key.
func TestStartNewRunWithOptionsMirrorsRunLimitsOntoLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "migration-018-run-limits-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	runID, _, err := store.StartNewRunWithOptions(ctx, "", defName, 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0, StartOptions{
			RunLimits: TenantSettings{
				WasmInstanceTimeout:  5 * time.Second,
				WasmWallClockCeiling: 10 * time.Second,
				HostRetryBudget:      30 * time.Second,
				MaxWorkflowDuration:  time.Hour,
			},
		})
	if err != nil {
		t.Fatalf("StartNewRunWithOptions: %v", err)
	}

	instRow := readInstanceRow(t, db, runID)
	leaseRow := readLeaseRow(t, db, runID)

	if !instRow.runInstanceMs.Valid || instRow.runInstanceMs.Int64 != 5000 {
		t.Fatalf("workflow_instances.run_wasm_instance_timeout_ms = %+v, want valid 5000 (sanity on the existing write)", instRow.runInstanceMs)
	}
	if !leaseRow.runInstanceMs.Valid || leaseRow.runInstanceMs.Int64 != 5000 {
		t.Errorf("workflow_leases.run_wasm_instance_timeout_ms = %+v, want valid 5000", leaseRow.runInstanceMs)
	}
}

// TestStartChildWorkflowMirrorsParentWorkflowIDOntoLeases covers the column
// engine/store_children.go's ChildWorkflowCount and OriginalChildRunIDs
// read (piece E) -- including the resolved def_version, since this path
// resolves an unspecified version via a MAX(version) subquery rather than
// receiving it as a Go-level argument.
func TestStartChildWorkflowMirrorsParentWorkflowIDOntoLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "migration-018-child-parent-id-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	parentID, _, err := store.StartNewRun(ctx, "", defName, 1, []byte(`{}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (parent): %v", err)
	}

	// defVersion=0 exercises the MAX(version)-subquery resolution path, not
	// the pass-through -- the case readLeaseRow's resolvedDefVersion
	// RETURNING capture exists for.
	childID, err := store.StartChildWorkflow(ctx, parentID, defName, `{"child":true}`, 0, "ABANDON", 1)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}

	instRow := readInstanceRow(t, db, childID)
	leaseRow := readLeaseRow(t, db, childID)

	if !instRow.parentWorkflowID.Valid || instRow.parentWorkflowID.String != parentID {
		t.Fatalf("workflow_instances.parent_workflow_id = %+v, want valid %q (sanity on the existing write)", instRow.parentWorkflowID, parentID)
	}
	if !leaseRow.parentWorkflowID.Valid || leaseRow.parentWorkflowID.String != parentID {
		t.Errorf("workflow_leases.parent_workflow_id = %+v, want valid %q", leaseRow.parentWorkflowID, parentID)
	}

	defNameGot, defVersionGot := readPayloadDef(t, db, childID)
	if !defNameGot.Valid || defNameGot.String != defName {
		t.Errorf("workflow_payloads.def_name = %+v, want valid %q", defNameGot, defName)
	}
	if !defVersionGot.Valid || defVersionGot.Int64 != 1 {
		t.Errorf("workflow_payloads.def_version = %+v, want valid 1 (the MAX(version)-resolved value, captured via RETURNING, not the 0 passed in)", defVersionGot)
	}
}

// TestStartChildWorkflowAtomicMirrorsParentWorkflowIDOntoLeases is
// StartChildWorkflow's test above, for the SECOND child-creating entry
// point, which has its own copy of the INSERT and its own
// insertLeaseAndPayloadRows call.
func TestStartChildWorkflowAtomicMirrorsParentWorkflowIDOntoLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "migration-018-child-atomic-parent-id-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	parentID, _, err := store.StartNewRun(ctx, "", defName, 1, []byte(`{}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (parent): %v", err)
	}

	childID := "migration-018-child-atomic-" + parentID
	event := EventRecord{Step: 0, EventType: "child_workflow", ChildName: defName, ChildInput: `{"child":true}`}
	got, err := store.StartChildWorkflowAtomic(ctx, childID, parentID, defName, `{"child":true}`, 0, "ABANDON", event, 1)
	if err != nil {
		t.Fatalf("StartChildWorkflowAtomic: %v", err)
	}
	if got != childID {
		t.Fatalf("StartChildWorkflowAtomic returned %q, want %q", got, childID)
	}

	instRow := readInstanceRow(t, db, childID)
	leaseRow := readLeaseRow(t, db, childID)

	if !instRow.parentWorkflowID.Valid || instRow.parentWorkflowID.String != parentID {
		t.Fatalf("workflow_instances.parent_workflow_id = %+v, want valid %q (sanity on the existing write)", instRow.parentWorkflowID, parentID)
	}
	if !leaseRow.parentWorkflowID.Valid || leaseRow.parentWorkflowID.String != parentID {
		t.Errorf("workflow_leases.parent_workflow_id = %+v, want valid %q", leaseRow.parentWorkflowID, parentID)
	}

	defNameGot, defVersionGot := readPayloadDef(t, db, childID)
	if !defVersionGot.Valid || defVersionGot.Int64 != 1 {
		t.Errorf("workflow_payloads.def_version = %+v, want valid 1 (the MAX(version)-resolved value)", defVersionGot)
	}
	_ = defNameGot
}

// TestContinueAsNewMirrorsContinuedFromAndInheritedParentOntoLeases covers
// ContinueAsNew's two new columns: continued_from (the run being continued,
// a Go-level value) and parent_workflow_id (INHERITED via a subquery on
// workflow_instances, captured here via RETURNING rather than
// re-evaluated -- see the comment at its call site). seedClaimedTriple
// stands in for a prior StartChildWorkflow call (same pattern piece 4a's
// own test file uses), with parent_workflow_id set by hand afterwards so
// this test can tell "inherited correctly" apart from "NULL throughout".
func TestContinueAsNewMirrorsContinuedFromAndInheritedParentOntoLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "migration-018-continue-as-new-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const grandparentID = "migration-018-canew-grandparent"
	const firstRunID = "migration-018-canew-first-run"
	const worker = "migration-018-canew-worker"
	seedClaimedTriple(t, db, grandparentID, defName, worker, 1)
	seedClaimedTriple(t, db, firstRunID, defName, worker, 1)
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_instances SET parent_workflow_id = $1 WHERE id = $2`,
		grandparentID, firstRunID); err != nil {
		t.Fatalf("set parent_workflow_id on the run being continued: %v", err)
	}

	newRunID, err := store.ContinueAsNew(ctx, firstRunID, worker, 1, defName, 1,
		json.RawMessage(`{"n":2}`), nil, `{}`, nil, 0)
	if err != nil {
		t.Fatalf("ContinueAsNew: %v", err)
	}

	instRow := readInstanceRow(t, db, newRunID)
	leaseRow := readLeaseRow(t, db, newRunID)

	if !instRow.continuedFrom.Valid || instRow.continuedFrom.String != firstRunID {
		t.Fatalf("workflow_instances.continued_from = %+v, want valid %q (sanity on the existing write)", instRow.continuedFrom, firstRunID)
	}
	if !leaseRow.continuedFrom.Valid || leaseRow.continuedFrom.String != firstRunID {
		t.Errorf("workflow_leases.continued_from = %+v, want valid %q", leaseRow.continuedFrom, firstRunID)
	}
	if !instRow.parentWorkflowID.Valid || instRow.parentWorkflowID.String != grandparentID {
		t.Fatalf("workflow_instances.parent_workflow_id = %+v, want valid %q (sanity on the existing INHERITED write)", instRow.parentWorkflowID, grandparentID)
	}
	if !leaseRow.parentWorkflowID.Valid || leaseRow.parentWorkflowID.String != grandparentID {
		t.Errorf("workflow_leases.parent_workflow_id = %+v, want valid %q (inherited value, captured via RETURNING)", leaseRow.parentWorkflowID, grandparentID)
	}

	defNameGot, defVersionGot := readPayloadDef(t, db, newRunID)
	if !defNameGot.Valid || defNameGot.String != defName {
		t.Errorf("workflow_payloads.def_name = %+v, want valid %q", defNameGot, defName)
	}
	if !defVersionGot.Valid || defVersionGot.Int64 != 1 {
		t.Errorf("workflow_payloads.def_version = %+v, want valid 1", defVersionGot)
	}
}
