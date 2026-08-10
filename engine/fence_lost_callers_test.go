package engine

// Real-database reproduction of the IMPROVEMENT-PLAN.md 1.2 *caller* residual.
//
// The store half of 1.2 has landed: all twelve fenced lifecycle writes
// (CompleteWorkflow, FailWorkflow, MoveToDeadLetterQueue, ContinueAsNew,
// across the three dialects) now inspect RowsAffected and return
// ErrFenceLost before running any post-commit cleanup. What did not land is
// the caller half -- the plan's "make callers handle it rather than
// fire-and-forget".
//
// The interesting case is not a caller that ignores a fence it genuinely
// lost. It is a caller that passes fence arguments which cannot match any
// row, so its write is *always* skipped, and then discards the error that
// says so. cmd/cleat-worker/server.go's concurrency-key conflict path did
// exactly that:
//
//	acquired, err := s.store.AcquireConcurrencyKey(...)
//	if !acquired {
//	    s.store.FailWorkflow(context.Background(), runID, "", 0, "concurrency key conflict: "+key, "", "", nil)
//	    s.writeError(w, 409, "workflow already running with key "+concurrencyKey)
//	    return
//	}
//
// FailWorkflow is fenced on `assigned_to = $2 AND generation = $7`. The run
// was created moments earlier by StartNewRun, which inserts it 'ready' with
// assigned_to NULL -- and `NULL = ''` is NULL, not true, so the UPDATE
// matches zero rows for any workerID the caller could pass. The client is
// told 409 "workflow already running with key X" while the run stays 'ready'
// with next_wake_at in the past, and the next worker to poll claims and
// executes it. The HTTP layer is the only enforcement point for the
// Cleat-Concurrency-Key header; ClaimWorkflows does not consult
// concurrency_keys.
//
// Needs a real database; see fence_lost_integration_test.go's header for the
// environment variables and the skip/fail policy.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestConcurrencyKeyConflict_RejectedRunIsNotClaimable asserts the property
// the 409 response promises: a run rejected for a concurrency-key conflict
// does not go on to execute.
//
// It deliberately asserts on claimability rather than on the error returned
// by the rejection write. A test that only checked for ErrFenceLost would
// pass against a caller that logged the error and still left the run
// runnable -- which is the bug.
func TestConcurrencyKeyConflict_RejectedRunIsNotClaimable(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	tenant := "dddddddd-dddd-4ddd-dddd-dddddddddddd"
	store := NewPostgresStore(adminDB).WithTenant(tenant)

	const defName = "conflict-key-def"
	def := &WorkflowDef{
		Name:       defName,
		Version:    1,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1,
		MinVersion: 1,
	}
	if err := store.DeployWorkflowDef(ctx, def); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	key := fmt.Sprintf("order-42-%d", time.Now().UnixNano())

	// First run acquires the key, exactly as handleEnqueue does.
	holderID := fmt.Sprintf("conflict-holder-%d", time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, holderID, defName, 1, json.RawMessage(`{}`), "", tenant, 0); err != nil {
		t.Fatalf("StartNewRun(holder): %v", err)
	}
	acquired, err := store.AcquireConcurrencyKey(ctx, key, holderID, 30*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey(holder): %v", err)
	}
	if !acquired {
		t.Fatalf("AcquireConcurrencyKey(holder) = false, want true on a fresh key")
	}

	// Second run loses the key and must be rejected.
	rejectedID := fmt.Sprintf("conflict-rejected-%d", time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, rejectedID, defName, 1, json.RawMessage(`{}`), "", tenant, 0); err != nil {
		t.Fatalf("StartNewRun(rejected): %v", err)
	}
	acquired, err = store.AcquireConcurrencyKey(ctx, key, rejectedID, 30*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey(rejected): %v", err)
	}
	if acquired {
		t.Fatalf("AcquireConcurrencyKey(rejected) = true, want false while %s holds the key", holderID)
	}

	// The rejection write the HTTP layer performs before answering 409.
	// Before the fix this was FailWorkflow(ctx, rejectedID, "", 0, ...),
	// whose fence cannot match an unclaimed row -- swap it back in and this
	// test fails with the run claimed and running.
	if err := store.TerminateWorkflow(ctx, rejectedID, "concurrency key conflict: "+key); err != nil {
		t.Fatalf("TerminateWorkflow: %v", err)
	}

	// The property under test: no worker may pick this run up.
	claimed, err := store.ClaimWorkflows(ctx, "worker-after-conflict", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	for _, wf := range claimed {
		if wf.ID == rejectedID {
			var status string
			var assigned any
			if qerr := adminDB.QueryRowContext(ctx,
				`SELECT status, assigned_to FROM workflow_instances WHERE id = $1`, rejectedID,
			).Scan(&status, &assigned); qerr != nil {
				t.Fatalf("post-claim status query: %v", qerr)
			}
			t.Fatalf("run %s was rejected with 409 but a worker claimed and will execute it "+
				"(status=%q assigned_to=%v); the concurrency key is not enforced",
				rejectedID, status, assigned)
		}
	}
}

// TestFailWorkflowFenceCannotMatchUnclaimedRun pins the store behaviour the
// bug above rested on, so that a future caller reaching for FailWorkflow to
// reject an unclaimed run fails a test rather than shipping.
//
// This is correct behaviour, not a defect: FailWorkflow is the *owner's*
// terminal write and its fence is doing its job. The defect was using it
// from a caller that never owned the run.
func TestFailWorkflowFenceCannotMatchUnclaimedRun(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	tenant := "dddddddd-dddd-4ddd-dddd-dddddddddddd"
	store := NewPostgresStore(adminDB).WithTenant(tenant)

	const defName = "unclaimed-fence-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name:       defName,
		Version:    1,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1,
		MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	runID := fmt.Sprintf("unclaimed-fence-%d", time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, runID, defName, 1, json.RawMessage(`{}`), "", tenant, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	// The arguments cmd/cleat-worker/server.go used to pass: no worker owns
	// the run yet, so there is no (workerID, generation) pair that matches.
	err := store.FailWorkflow(ctx, runID, "", 0, "concurrency key conflict: k", "", "", nil)
	if !errors.Is(err, ErrFenceLost) {
		t.Fatalf("FailWorkflow on an unclaimed run = %v, want ErrFenceLost", err)
	}

	var status string
	if err := adminDB.QueryRowContext(ctx,
		`SELECT status FROM workflow_instances WHERE id = $1`, runID,
	).Scan(&status); err != nil {
		t.Fatalf("status query: %v", err)
	}
	if status != "ready" {
		t.Fatalf("status after fence-lost FailWorkflow = %q, want %q (the write must not have applied)", status, "ready")
	}
}
