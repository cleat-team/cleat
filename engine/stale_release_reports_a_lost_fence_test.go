package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// A stale ReleaseWorkflow reports a lost fence, on every dialect. cleat#1223.
//
// Measured before this change, same sequence on live PostgreSQL 16, MySQL 8.4
// and SQL Server 2022 -- and the row either side of it is what makes the
// divergence indefensible rather than merely untidy:
//
//	call                     postgres      mysql         mssql
//	ReleaseWorkflow          nil           nil           "no rows affected for <uuid>"
//	CompleteWorkflow         ErrFenceLost  ErrFenceLost  ErrFenceLost
//	FailWorkflow             ErrFenceLost  ErrFenceLost  ErrFenceLost
//	FinalizeWorkflowSegment  ErrFenceLost  ErrFenceLost  ErrFenceLost
//
// So the other three fenced writes were already uniform. Release was the sole
// outlier, silent on two dialects and erroring on the third WITHOUT the
// established vocabulary, so errors.Is(err, ErrFenceLost) was false for it.
//
// THE CALLER HAD ALREADY DECIDED, which is what settles the direction.
// cmd/cleat-worker/setup.go:3157 branches on errors.Is(err, ErrFenceLost) and
// treats it as "the no-op it is", logging at Debug. On PostgreSQL and MySQL
// that branch was dead code and had never run. On SQL Server the raw error
// fell through to the next branch, which logs a WARNING reading "release
// failed, workflow stays claimed until its lease expires" -- false on both
// counts: the workflow belongs to the other worker, and nothing went wrong.
// finishClaim's over-claim release logs the same falsehood at Error.
//
// The argument for silence -- that a release has no outcome to lose, unlike
// the three writes above -- is the argument that comment already makes, and
// the codebase's answer to it is "return the error and let the caller decide"
// rather than "discard it".
func TestAStaleReleaseReportsALostFence(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: "test-workflow", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "stale-release", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			claimed, err := store.ClaimWorkflows(ctx, "worker-1", 10)
			if err != nil {
				t.Fatalf("ClaimWorkflows(worker-1): %v", err)
			}
			var gen int64
			var found bool
			for _, wf := range claimed {
				if wf.ID == id {
					gen, found = wf.Generation, true
				}
			}
			if !found {
				t.Fatalf("worker-1 did not claim the run")
			}

			// In the PAST, and both halves of that matter.
			//
			// Not the future: a released run is `ready` with next_wake_at set,
			// and ClaimWorkflows will not take one that is not yet due. A wake
			// time a minute ahead made worker-2's claim silently do nothing,
			// and the test then failed on its own premise -- assigned_to was
			// "" rather than "worker-2" -- while the assertion it exists for
			// had already started passing.
			//
			// Not the zero value either: MySQL rejects '0000-00-00' for this
			// column where PostgreSQL and SQL Server accept it. No in-tree
			// caller can reach that, since every one passes wf.NextWakeAt from
			// a claimed row and the column is NOT NULL, but it costs a run to
			// rediscover.
			wake := time.Now().Add(-time.Minute)

			if err := store.ReleaseWorkflow(ctx, id, "worker-1", gen, wake); err != nil {
				t.Fatalf("the first release should succeed: %v", err)
			}
			if _, err := store.ClaimWorkflows(ctx, "worker-2", 10); err != nil {
				t.Fatalf("ClaimWorkflows(worker-2): %v", err)
			}

			// The stale release: worker-1's credentials no longer hold.
			err = store.ReleaseWorkflow(ctx, id, "worker-1", gen, wake)

			if !errors.Is(err, ErrFenceLost) {
				t.Errorf("a stale release returned %v, want ErrFenceLost.\n\n"+
					"CompleteWorkflow, FailWorkflow and FinalizeWorkflowSegment all report a "+
					"lost fence this way on all three dialects; release did not, and the "+
					"worker's releaseWorkflow branches on errors.Is(err, ErrFenceLost). "+
					"Without it that branch is dead and a raw error is logged as \"release "+
					"failed, workflow stays claimed until its lease expires\", which is not "+
					"true of either the workflow or the failure.", err)
			}

			// The property the report is ABOUT, asserted separately. A store
			// that returned ErrFenceLost while actually resurrecting the row
			// would satisfy the check above and be far worse than the bug.
			after, err := store.GetWorkflowByID(ctx, id)
			if err != nil || after == nil {
				t.Fatalf("GetWorkflowByID: %v (nil=%v)", err, after == nil)
			}
			if after.AssignedTo != "worker-2" {
				t.Errorf("after the stale release the run is assigned to %q, want \"worker-2\".\n\n"+
					"Reporting the lost fence must not change what the stale call DOES, which "+
					"is nothing.", after.AssignedTo)
			}
		})
	}
}
