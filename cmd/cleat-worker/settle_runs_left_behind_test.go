package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// settleRunsLeftBehind terminates what its test added and nothing else: a run
// already ready when the test began belongs to someone, and terminating it
// would break whichever test that was.
func TestSettleRunsLeftBehindTerminatesWhatTheTestAddedAndNothingElse(t *testing.T) {
	ctx := context.Background()
	db := testutil.SuiteTestDB(t, "cleat_worker_settle")
	stamp := time.Now().UnixNano()
	stranger := fmt.Sprintf("settle-stranger-%d", stamp)
	added := fmt.Sprintf("settle-added-ready-%d", stamp)
	addedTerminating := fmt.Sprintf("settle-added-terminating-%d", stamp)
	running := fmt.Sprintf("settle-added-running-%d", stamp)

	// A run's payload row has a foreign key to a deployed definition. The name
	// is unique per run: the database persists, and a definition with an open
	// run is not replaced (cleat#3497).
	defName := fmt.Sprintf("settle-probe-%d", stamp)
	if err := engine.NewPostgresStore(db).DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	// Registered before any seed, so a seed that fails half way (SeedRun's three
	// inserts are not one transaction) is still settled.
	t.Cleanup(func() {
		db.ExecContext(ctx, `UPDATE workflow_leases SET status = 'done' WHERE id LIKE $1`, fmt.Sprintf("settle-%%-%d", stamp))
	})
	seed := func(id, status string) {
		t.Helper()
		testutil.SeedRun(t, db, testutil.RunSeed{ID: id, DefName: defName, Status: status})
	}
	statusOf := func(id string) string {
		t.Helper()
		var s string
		if err := db.QueryRowContext(ctx, `SELECT status FROM workflow_leases WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatalf("status of %s: %v", id, err)
		}
		return s
	}

	seed(stranger, "ready")

	t.Run("a test that leaves runs behind", func(t *testing.T) {
		settleRunsLeftBehind(t, db)
		seed(added, "ready")
		seed(addedTerminating, "terminating")
		seed(running, "running")
		if got := statusOf(added); got != "ready" {
			t.Fatalf("the fixture is not what the test is about: %s is %q before the cleanup, want ready", added, got)
		}
	})
	// The subtest's cleanup has run by here.

	for id, want := range map[string]string{
		added:            "terminated",
		addedTerminating: "terminated",
		running:          "running", // not a leftover state, so not the helper's to touch
		stranger:         "ready",   // there before the test began
	} {
		if got := statusOf(id); got != want {
			t.Errorf("%s is %q after the test, want %q", id, got, want)
		}
	}
}
