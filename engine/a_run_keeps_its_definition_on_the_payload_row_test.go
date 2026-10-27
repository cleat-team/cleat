package engine

import (
	"context"
	"strings"
	"testing"
)

// cleat#3245 B5: workflow_instances no longer carries def_name and def_version,
// so the foreign key that kept a definition from being deleted under a live run
// moved to workflow_payloads, which carries the same two values.

// A definition a run still uses cannot be deleted. The control is the same
// delete against a definition no run uses, which has to succeed: without it a
// refusal could come from anything.
func TestADefinitionAStartedRunUsesCannotBeDeleted(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	startPayloadReaderRun(t, store, "b5-def-in-use")
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: "b5-def-unused", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	if _, err := db.Exec(`DELETE FROM workflow_defs WHERE name = 'b5-def-unused'`); err != nil {
		t.Fatalf("control: deleting a definition no run uses = %v, want success", err)
	}
	_, err := db.Exec(`DELETE FROM workflow_defs WHERE name = 'b5-def-in-use'`)
	if err == nil {
		t.Fatal("deleting a definition a started run uses succeeded, want a foreign key refusal")
	}
	if !strings.Contains(err.Error(), "workflow_payloads_def_fkey") {
		t.Fatalf("the delete was refused, but not by workflow_payloads_def_fkey: %v", err)
	}
}

// A child of a definition with no enabled version has nothing to run. This used
// to be refused by accident, as a NOT NULL violation on workflow_instances.def_version;
// it is refused by name now. The control starts a child of a deployed definition
// the same way.
func TestAChildOfADefinitionWithNoEnabledVersionIsRefusedByName(t *testing.T) {
	store, _ := newRunTestStore(t)
	ctx := context.Background()
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: "b5-child-def", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	if _, err := store.StartChildWorkflow(ctx, "b5-parent", "b5-child-def", `{}`, 0, "ABANDON", 0); err != nil {
		t.Fatalf("control: StartChildWorkflow of a deployed definition = %v, want success", err)
	}
	_, err := store.StartChildWorkflow(ctx, "b5-parent", "b5-never-deployed", `{}`, 0, "ABANDON", 0)
	if err == nil {
		t.Fatal("StartChildWorkflow of a definition with no version succeeded")
	}
	if !strings.Contains(err.Error(), "no enabled version") {
		t.Fatalf("refused, but not by name: %v", err)
	}
}
