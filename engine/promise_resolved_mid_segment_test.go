package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestAPromiseResolvedWhileTheWorkflowIsAwakeStillWakesIt,
// TestAPromiseRejectedWhileTheWorkflowIsAwakeStillWakesIt and
// TestAnUpdateDispatchedWhileTheWorkflowIsAwakeStillWakesIt are cleat#3171,
// the sibling cleat#953's own decision left for later: "The same shape guards
// three more wake paths -- promise resolved, promise rejected, update
// dispatched -- ... [they] remain source reads" until reproduced.
//
// ResolvePromise, RejectPromise and CreateUpdateRequest each pull
// next_wake_at forward only for a workflow that is already suspended --
// 'running' is excluded, because a claimed workflow's row is about to be
// overwritten by finalize anyway. A settlement/dispatch arriving mid-segment
// therefore scheduled nothing, the workflow re-suspended, and finalize wrote
// its own timeout deadline over next_wake_at -- the delivery sat durable
// until that timeout expired.
//
// promise_seq is bumped by every one of the three writes;
// promise_seq_at_claim is stamped by every claim. finalize compares them IN
// ITS OWN TRANSACTION, the same reason signal_seq was shaped this way: a
// post-commit poll closes the same window in the steady state but loses the
// wake entirely if the worker dies between the commit and the poll.
//
// THE CONTROL IS THE HALF THAT MATTERS, same as cleat#953's test: the obvious
// wrong fix is "wake if the triggering table has any row", and it SPINS -- a
// workflow awaiting promise {a} with an unrelated promise {z} pending would
// wake, poll, find nothing it wants, re-suspend, and repeat forever. A
// counter only moves on a NEW settlement. The no-settlement case below is
// what tells those two implementations apart.
//
// SCOPE IS THE #981 SHAPE ONLY (one counter, no drain/burst handling).
// cleat#953's burst extension (#985, signal_consumed_seq) exists because
// signals have a separate poll-then-consume step whose consumption can itself
// race finalize; promises and update requests have no such second step here.
// If a burst-shaped gap is later measured on this path, it is a new issue.

// promiseMidSegmentCase names one of the three call sites under test, and
// supplies the one thing that differs between them: how to make the
// settlement/dispatch happen once the workflow is claimed and running.
type promiseMidSegmentCase struct {
	name            string
	settle          func(t *testing.T, ctx context.Context, store WorkflowStore, workflowID, promiseID string)
	seedsOwnPromise bool // true for Resolve/Reject, which need CreatePromise first
}

var promiseMidSegmentCases = []promiseMidSegmentCase{
	{
		name: "resolve",
		settle: func(t *testing.T, ctx context.Context, store WorkflowStore, workflowID, promiseID string) {
			t.Helper()
			if err := store.ResolvePromise(ctx, promiseID, `{"ok":true}`); err != nil {
				t.Fatalf("ResolvePromise: %v", err)
			}
		},
		seedsOwnPromise: true,
	},
	{
		name: "reject",
		settle: func(t *testing.T, ctx context.Context, store WorkflowStore, workflowID, promiseID string) {
			t.Helper()
			if err := store.RejectPromise(ctx, promiseID, "boom"); err != nil {
				t.Fatalf("RejectPromise: %v", err)
			}
		},
		seedsOwnPromise: true,
	},
	{
		name: "createUpdateRequest",
		settle: func(t *testing.T, ctx context.Context, store WorkflowStore, workflowID, promiseID string) {
			t.Helper()
			if err := store.CreateUpdateRequest(ctx, workflowID, "do-the-thing", `{}`, promiseID); err != nil {
				t.Fatalf("CreateUpdateRequest: %v", err)
			}
		},
		seedsOwnPromise: false,
	},
}

func TestAPromiseOrUpdateSettledWhileTheWorkflowIsAwakeStillWakesIt(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			for _, tc := range promiseMidSegmentCases {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					store, teardown := backend.Setup(t)
					defer teardown()
					ctx := context.Background()
					d := dialectOf(t, store)
					db := rawDBOf(t, store)
					_ = newIntentWorkflow(t, ctx, store, "promise-mid-segment-seed-"+tc.name)

					// deadline is far enough out that "woke immediately" and
					// "slept to its deadline" cannot be confused by
					// scheduling noise -- same reasoning as cleat#953's
					// test, and the same reason the assertion below is on
					// which side of the midpoint the wake lands rather than
					// on any particular duration.
					const deadline = 60 * time.Second

					runSegment := func(label string, settle bool) bool {
						t.Helper()
						id := fmt.Sprintf("promise-mid-segment-%s-%s-%d", tc.name, label, time.Now().UnixNano())
						promiseID := fmt.Sprintf("p-%s-%s-%d", tc.name, label, time.Now().UnixNano())
						if _, _, err := store.StartNewRun(ctx, id, "intent-workflow", 1,
							json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
							t.Fatalf("StartNewRun: %v", err)
						}
						if tc.seedsOwnPromise {
							if err := store.CreatePromise(ctx, id, "thing", promiseID); err != nil {
								t.Fatalf("CreatePromise: %v", err)
							}
						}
						wf, err := claimSpecific(t, ctx, store, id, "worker-promise-mid-segment")
						if err != nil {
							t.Fatalf("claiming %s: %v", id, err)
						}
						// THE WINDOW: the workflow is claimed, so status is
						// 'running' and the settlement's own wake update
						// cannot match it.
						if settle {
							tc.settle(t, ctx, store, id, promiseID)
						}
						handed := time.Now().Add(deadline)
						if err := store.FinalizeWorkflowSegment(ctx, id, "worker-promise-mid-segment",
							wf.Generation, nil, "ready", "", "", "",
							map[string]string{}, handed); err != nil {
							t.Fatalf("FinalizeWorkflowSegment: %v", err)
						}
						var wake time.Time
						q := "SELECT next_wake_at FROM workflow_instances WHERE id = " + d.placeholder(1)
						if err := db.QueryRow(q, id).Scan(&wake); err != nil {
							t.Fatalf("reading next_wake_at: %v", err)
						}
						// Exact: either finalize wrote the deadline it was
						// given, or it wrote something earlier. No
						// tolerance, no clock reading.
						return wake.Before(handed.Add(-time.Second))
					}

					if woken := runSegment("settled", true); !woken {
						t.Errorf("%s while the workflow was RUNNING left next_wake_at at the "+
							"deadline finalize was handed.\n\n"+
							"The settlement is durable and the workflow is waiting for exactly "+
							"it, so a poller will see a timeout with the result already stored. "+
							"The wake-write's own next_wake_at update cannot see a 'running' row, "+
							"and finalize overwrites next_wake_at with the deadline -- promise_seq "+
							"differing from promise_seq_at_claim is what closes that.", tc.name)
					}

					// CONTROL: no settlement, so nothing should have moved
					// the deadline.
					if woken := runSegment("CONTROL-no-settlement", false); woken {
						t.Errorf("CONTROL FAILED (%s): a workflow with NO settlement had "+
							"next_wake_at pulled BEFORE the deadline finalize was handed.\n\n"+
							"That is the spin promise_seq exists to avoid: an implementation "+
							"that wakes whenever the triggering table is non-empty, or one that "+
							"always wakes, passes the assertion above and burns a core here.",
							tc.name)
					}
				})
			}
		})
	}
}
