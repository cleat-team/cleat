package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// cleat#3245 Phase 3 step 3 piece F: PostgresStore's ListWorkflows,
// CountWorkflows and GetWorkflowByID now read workflow_leases JOIN
// workflow_payloads, not workflow_instances. TraceWorkflow's write now
// mirrors onto workflow_payloads too, which these reads depend on.
//
// Same shape as pieces A/C/D/E's deliberately-diverging tests: ordinary use
// keeps all three tables in sync, so a test that only exercises normal
// writes cannot tell "reads the join" apart from "reads workflow_instances"
// -- both readings agree. These tests diverge workflow_leases/
// workflow_payloads directly (bypassing every write path that would mirror
// the change) while leaving workflow_instances at its original value, so
// the two readings disagree about the outcome and the result says which
// table was actually read.

func TestListWorkflowsReadsFromLeasesAndPayloadsNotInstances(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "list-reads-leases-payloads-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	id, _, err := store.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{}`), "list-reads-leases-payloads", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	// Diverge workflow_leases.status and workflow_payloads.def_name away
	// from workflow_instances, which still says status='ready',
	// def_name=defName.
	if _, err := db.Exec(`UPDATE workflow_leases SET status = 'failed' WHERE id = $1`, id); err != nil {
		t.Fatalf("diverge workflow_leases.status: %v", err)
	}
	if _, err := db.Exec(`UPDATE workflow_payloads SET def_name = 'diverged-def-name' WHERE id = $1`, id); err != nil {
		t.Fatalf("diverge workflow_payloads.def_name: %v", err)
	}

	// Filtering by the DIVERGED status finds the row only if the predicate
	// ran against workflow_leases -- workflow_instances still says 'ready'.
	rows, err := store.ListWorkflows(ctx, WorkflowFilter{Status: "failed", Limit: 10})
	if err != nil {
		t.Fatalf("ListWorkflows(Status=failed): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("ListWorkflows(Status=failed) = %v, want [%s] -- a miss means the filter "+
			"used workflow_instances.status ('ready') instead of workflow_leases.status ('failed')", rows, id)
	}
	if rows[0].DefName != "diverged-def-name" {
		t.Errorf("ListWorkflows returned DefName = %q, want %q -- the SELECT used "+
			"workflow_instances.def_name instead of workflow_payloads.def_name",
			rows[0].DefName, "diverged-def-name")
	}

	// The OLD status, still true of workflow_instances alone, must find
	// nothing -- a hit here means the filter read the old table.
	rows, err = store.ListWorkflows(ctx, WorkflowFilter{Status: "ready", Limit: 10})
	if err != nil {
		t.Fatalf("ListWorkflows(Status=ready): %v", err)
	}
	for _, r := range rows {
		if r.ID == id {
			t.Fatalf("ListWorkflows(Status=ready) found %s, want it excluded -- "+
				"workflow_leases.status is now 'failed', so a match means the "+
				"filter read workflow_instances.status instead", id)
		}
	}
}

func TestCountWorkflowsReadsFromLeasesAndPayloadsNotInstances(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "count-reads-leases-payloads-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	id, _, err := store.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{}`), "count-reads-leases-payloads", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	if _, err := db.Exec(`UPDATE workflow_leases SET status = 'failed' WHERE id = $1`, id); err != nil {
		t.Fatalf("diverge workflow_leases.status: %v", err)
	}

	n, err := store.CountWorkflows(ctx, WorkflowFilter{Status: "failed"})
	if err != nil {
		t.Fatalf("CountWorkflows(Status=failed): %v", err)
	}
	if n != 1 {
		t.Fatalf("CountWorkflows(Status=failed) = %d, want 1 -- a 0 means the predicate "+
			"used workflow_instances.status ('ready') instead of workflow_leases.status ('failed')", n)
	}

	n, err = store.CountWorkflows(ctx, WorkflowFilter{Status: "ready"})
	if err != nil {
		t.Fatalf("CountWorkflows(Status=ready): %v", err)
	}
	if n != 0 {
		t.Fatalf("CountWorkflows(Status=ready) = %d, want 0 -- a 1 means the predicate "+
			"used workflow_instances.status instead of workflow_leases.status", n)
	}
}

func TestGetWorkflowByIDReadsFromLeasesAndPayloadsNotInstances(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "get-reads-leases-payloads-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	id, _, err := store.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{}`), "get-reads-leases-payloads", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	if _, err := db.Exec(`UPDATE workflow_leases SET status = 'failed' WHERE id = $1`, id); err != nil {
		t.Fatalf("diverge workflow_leases.status: %v", err)
	}
	if _, err := db.Exec(`UPDATE workflow_payloads SET def_name = 'diverged-def-name' WHERE id = $1`, id); err != nil {
		t.Fatalf("diverge workflow_payloads.def_name: %v", err)
	}

	wf, err := store.GetWorkflowByID(ctx, id)
	if err != nil || wf == nil {
		t.Fatalf("GetWorkflowByID: %v (nil=%v)", err, wf == nil)
	}
	if wf.Status != "failed" {
		t.Errorf("GetWorkflowByID returned Status = %q, want %q -- the SELECT used "+
			"workflow_instances.status instead of workflow_leases.status", wf.Status, "failed")
	}
	if wf.DefName != "diverged-def-name" {
		t.Errorf("GetWorkflowByID returned DefName = %q, want %q -- the SELECT used "+
			"workflow_instances.def_name instead of workflow_payloads.def_name",
			wf.DefName, "diverged-def-name")
	}
}

// TestTraceWorkflowMirrorsOntoWorkflowPayloadsNotJustInstances covers the one
// write-mirror site piece F added that has no pre-existing lease/payload
// regression test to extend (TraceWorkflow wrote only workflow_instances
// before this piece, with no dual-write at all).
func TestTraceWorkflowMirrorsOntoWorkflowPayloadsNotJustInstances(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "trace-mirrors-payloads-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	id, _, err := store.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{}`), "trace-mirrors-payloads", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	const traceID = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	if err := store.TraceWorkflow(ctx, id, traceID); err != nil {
		t.Fatalf("TraceWorkflow: %v", err)
	}

	var payloadTraceID string
	if err := db.QueryRow(`SELECT trace_id FROM workflow_payloads WHERE id = $1`, id).Scan(&payloadTraceID); err != nil {
		t.Fatalf("read workflow_payloads.trace_id: %v", err)
	}
	if payloadTraceID != traceID {
		t.Fatalf("workflow_payloads.trace_id = %q, want %q -- TraceWorkflow is not mirroring onto workflow_payloads", payloadTraceID, traceID)
	}

	// Diverge workflow_instances.trace_id directly, bypassing TraceWorkflow.
	// GetWorkflowByID reading it instead of workflow_payloads would see this
	// wrong value.
	if _, err := db.Exec(`UPDATE workflow_instances SET trace_id = 'wrong-trace-id' WHERE id = $1`, id); err != nil {
		t.Fatalf("diverge workflow_instances.trace_id: %v", err)
	}

	wf, err := store.GetWorkflowByID(ctx, id)
	if err != nil || wf == nil {
		t.Fatalf("GetWorkflowByID: %v (nil=%v)", err, wf == nil)
	}
	if wf.TraceID != traceID {
		t.Errorf("GetWorkflowByID returned TraceID = %q, want %q -- a %q means the read "+
			"used workflow_instances.trace_id instead of workflow_payloads.trace_id",
			wf.TraceID, traceID, "wrong-trace-id")
	}
}
