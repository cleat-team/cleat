package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// A completed run names the worker that ran it. cleat#1118.
//
// TWO WORKERS, NOT ONE, AND THAT IS THE WHOLE TEST. Asserting that the field is
// non-empty passes against a column populated with a constant -- the worker id,
// a literal, the string "worker", anything. It is an assertion that runs and
// cannot distinguish, which is the class of defect the owner's decision note
// singles out. So this claims two runs as two DIFFERENT workers and requires
// the recorded identities to DIFFER and to match the claimant in each case.
//
// WHY THE FIELD IS NEEDED AT ALL. workflow_instances.assigned_to is a LEASE,
// not a record: every terminal write clears it *while fencing on it*, so the
// field that would name the worker is the fence for the write that erases it.
// Measured 185 of 185 blank on a ports database. completed_by takes the
// identity at exactly the moment the lease is surrendered.
//
// AND IT ASSERTS assigned_to IS STILL CLEARED, because the cheap way to make
// the first half pass is to stop clearing the lease -- which would break the
// fence this engine depends on. The two have to hold together.
func TestACompletedRunNamesTheWorkerThatRanIt(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: "completer", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			started := map[string]bool{}
			for i := 0; i < 2; i++ {
				id, _, err := store.StartNewRun(ctx, "", "completer", 1,
					json.RawMessage(`{}`),
					fmt.Sprintf("completer-%d-%d", i, time.Now().UnixNano()),
					DefaultTenantUUID, 0)
				if err != nil {
					t.Fatalf("StartNewRun %d: %v", i, err)
				}
				started[id] = true
			}

			// BIND EACH WORKER TO THE RUN IT ACTUALLY CLAIMED, rather than
			// asserting which run it gets. cleat#1574.
			//
			// Both runs are created above before either is claimed, so TWO are
			// outstanding at the first claim -- not one, as this comment used
			// to say while citing cleat#1115. Which one comes back is not
			// determined: the claim orders by `priority ASC, created_at` with
			// no unique tiebreak (engine/mssql_lifecycle.go:232 and the same
			// shape at engine/mysql_lifecycle.go:98), and on SQL Server the
			// READPAST/UPDLOCK hints let the engine skip locked rows and do not
			// oblige it to scan in index order. It failed roughly half the time
			// on mssql, on branches containing no Go source at all.
			//
			// The ordering was incidental to what is being verified: that a
			// completed run names ITS OWN claimant, and that two claimants are
			// recorded as different. Binding after the fact tests exactly that
			// and assumes nothing about order.
			//
			// The two guards below keep what the discarded assertion was
			// really protecting. It could not tell "the claim returned my other
			// run" from "the claim returned a run belonging to something else",
			// and reported both as the former; these separate them, and the
			// second also catches a claim consuming a run that was not
			// outstanding, which is cleat#1115's actual subject.
			type run struct {
				id     string
				worker string
			}
			var runs []run
			claimedIDs := map[string]string{}
			for _, worker := range []string{"worker-alpha", "worker-beta"} {
				claimed, err := store.ClaimWorkflow(ctx, worker)
				if err != nil || claimed == nil {
					t.Fatalf("ClaimWorkflow as %s: %v (nil=%v)", worker, err, claimed == nil)
				}
				if !started[claimed.ID] {
					t.Fatalf("UNMEASURED: %s claimed %s, which this test did not start -- "+
						"something else's run is in this database, so the identities below "+
						"would be attributed to rows this test knows nothing about",
						worker, claimed.ID)
				}
				if prev, dup := claimedIDs[claimed.ID]; dup {
					t.Fatalf("%s claimed %s, which %s already holds -- a claim consumed a "+
						"run that was not outstanding (cleat#1115)", worker, claimed.ID, prev)
				}
				claimedIDs[claimed.ID] = worker
				runs = append(runs, run{id: claimed.ID, worker: worker})
				if err := store.CompleteWorkflow(ctx, claimed.ID, worker,
					claimed.Generation, `{"ok":true}`, nil); err != nil {
					t.Fatalf("CompleteWorkflow as %s: %v", worker, err)
				}
			}

			var got []string
			for _, r := range runs {
				wf, err := store.GetWorkflowByID(ctx, r.id)
				if err != nil || wf == nil {
					t.Fatalf("GetWorkflowByID %s: %v (nil=%v)", r.id, err, wf == nil)
				}
				if wf.Status != "done" {
					t.Fatalf("UNMEASURED: %s is %q, not done -- the run did not reach a "+
						"terminal status, so nothing below is about a completed run",
						r.id, wf.Status)
				}
				if wf.CompletedBy != r.worker {
					t.Errorf("%s was run by %q and reports completed_by=%q",
						r.id, r.worker, wf.CompletedBy)
				}
				// The lease must still be surrendered. Recording the worker by
				// simply not clearing assigned_to would satisfy the check above
				// and break the fence every terminal write depends on.
				if wf.AssignedTo != "" {
					t.Errorf("%s still holds a lease (assigned_to=%q) after completing. "+
						"completed_by must RECORD the worker, not retain the lease -- the "+
						"fence is (assigned_to, generation) and a run that keeps its "+
						"assignment can be finalised twice.", r.id, wf.AssignedTo)
				}
				got = append(got, wf.CompletedBy)
			}

			// The discriminating assertion. Everything above passes against a
			// column filled with one constant; this is the only line that does
			// not.
			if got[0] == got[1] {
				t.Errorf("both runs report the same worker (%q), but they were claimed by "+
					"%q and %q.\n\nA constant satisfies every other assertion here -- this "+
					"is the one that separates a recorded identity from a populated column.",
					got[0], runs[0].worker, runs[1].worker)
			}
		})
	}
}
