package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestAContinuedChildStaysItsParentsChild is cleat#955.
//
// A child that continues as new was orphaned: `ContinueAsNew` wrote a new run
// with `parent_workflow_id` NULL, so from the parent's side the chain vanished
// in two independent ways.
//
//	the CLOSE POLICY  enforceParentClosePolicy selects on parent_workflow_id,
//	                  and NULL matches nothing -- so a TERMINATE parent
//	                  reported that it had stopped its child while every
//	                  continued iteration kept running
//
//	the RESULT        the parent holds the id of the run it STARTED, which
//	                  ContinueAsNew leaves at status 'done' with an empty
//	                  result -- 'done' because it was superseded, not because
//	                  it finished. GetChildResult read that row and returned
//	                  {} while the real result sat on the last run
//
// The two need different fixes and neither implies the other, which is why
// both are asserted here. Propagating the link reconnects the policy; it does
// nothing for the result, because the parent is still holding run 1's id.
// Resolving the chain reconnects the result; it does nothing for the policy,
// because the rows are still NULL.
//
// THE CONTROL IS LOAD-BEARING. A plain sibling child, same parent, same
// TERMINATE policy, is stopped by the same call in the same transaction.
// Without it, "the continued run was not stopped" is equally consistent with
// "the policy call did nothing", which is the reading I could not otherwise
// exclude -- and it is how this was first measured.
func TestAContinuedChildStaysItsParentsChild(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			starter, ok := store.(interface {
				StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string,
					defVersion int, parentClosePolicy string, priority int) (string, error)
			})
			if !ok {
				t.Fatalf("%T cannot start a child workflow", store)
			}

			parent := newIntentWorkflow(t, ctx, store, "cont-child-parent")

			child, err := starter.StartChildWorkflow(ctx, parent, "intent-workflow", `{}`, 1, "TERMINATE", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}
			sibling, err := starter.StartChildWorkflow(ctx, parent, "intent-workflow", `{}`, 1, "TERMINATE", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow (control sibling): %v", err)
			}

			// The child continues as new, twice, the way a long-running one does.
			cur := child
			for i := 0; i < 2; i++ {
				wf, err := claimSpecific(t, ctx, store, cur, "w-child")
				if err != nil {
					t.Fatalf("claim %s: %v", cur, err)
				}
				next, err := store.ContinueAsNew(ctx, cur, "w-child", wf.Generation,
					"intent-workflow", 1, json.RawMessage(`{}`), nil, "", map[string]string{}, 0)
				if err != nil {
					t.Fatalf("ContinueAsNew: %v", err)
				}
				cur = next
			}

			// --- HALF ONE: the result the parent gets ---
			//
			// Finish the LAST run with a distinctive result. The parent still
			// holds the id of the first, so this is only reachable by
			// following the chain.
			last, err := claimSpecific(t, ctx, store, cur, "w-child")
			if err != nil {
				t.Fatalf("claim final run: %v", err)
			}
			const want = `{"final":true}`
			if err := store.FinalizeWorkflowSegment(ctx, cur, "w-child", last.Generation, nil,
				"done", want, "", "", map[string]string{}, time0()); err != nil {
				t.Fatalf("finalize final run: %v", err)
			}

			childStore, ok := store.(ChildWorkflowStore)
			if !ok {
				t.Fatalf("%T is not a ChildWorkflowStore", store)
			}
			_outcome, err := childStore.GetChildResult(ctx, child)
			got := _outcome.Result
			completed := _outcome.Completed
			if err != nil {
				t.Fatalf("GetChildResult: %v", err)
			}
			if !completed || got != want {
				t.Errorf("the parent asked for its child's result and got (%q, completed=%v), want (%q, true).\n\n"+
					"It holds the id of the run it STARTED, and ContinueAsNew leaves that run "+
					"'done' with an empty result -- 'done' because it was superseded. Nothing on "+
					"the row distinguishes that from having finished, so the answer has to come "+
					"from the last run in the chain.", got, completed, want)
			}

			// --- HALF TWO: the close policy ---
			ps, ok := store.(interface {
				enforceParentClosePolicy(ctx context.Context, parentWorkflowID string)
			})
			if !ok {
				// Fatal, not Skip. Every registered backend is a concrete
				// store that has this method; a build where one does not is a
				// tree that cannot enforce the policy, not an environment
				// missing an optional resource. A skip here would leave the
				// half below silently unverified, which is the shape this
				// whole test exists to close.
				t.Fatalf("%T does not expose enforceParentClosePolicy, so the close-policy "+
					"half of cleat#955 cannot be checked on this backend", store)
			}
			ps.enforceParentClosePolicy(ctx, parent)

			// The control first: if this was not stopped, the call did nothing
			// and the assertion below would pass for the wrong reason.
			sibWf, err := store.GetWorkflowByID(ctx, sibling)
			if err != nil {
				t.Fatalf("GetWorkflowByID (sibling): %v", err)
			}
			if sibWf == nil || sibWf.Status != "failed" {
				t.Fatalf("CONTROL FAILED: a plain child of the same parent, with the same "+
					"TERMINATE policy, is %v after enforceParentClosePolicy -- want status "+
					"'failed'. The policy call did nothing, so nothing below is measuring the "+
					"continued child.", sibWf)
			}

			// Every run in the chain must now be reachable from the parent.
			// Read the column directly: WorkflowInstance does not carry it, and
			// the column is what enforceParentClosePolicy selects on.
			d := dialectOf(t, store)
			db := rawDBOf(t, store)
			for i, id := range chainOf(t, ctx, store, child) {
				var got *string
				q := "SELECT parent_workflow_id FROM workflow_instances WHERE id = " + d.placeholder(1)
				if err := db.QueryRow(q, id).Scan(&got); err != nil {
					t.Fatalf("reading parent_workflow_id for chain[%d]: %v", i, err)
				}
				link := "NULL"
				if got != nil {
					link = *got
				}
				if link != parent {
					t.Errorf("chain run %d (%s) has parent_workflow_id = %q, want %q.\n\n"+
						"enforceParentClosePolicy selects on that column and NULL matches "+
						"nothing, so a TERMINATE parent reported stopping its child while this "+
						"run kept going. The control sibling above WAS stopped by the same "+
						"call, so the policy works and only the link was missing.",
						i, id, link, parent)
				}
			}
		})
	}
}

// claimSpecific claims until it gets the run it was asked for, so a test that
// needs a particular workflow's generation is not at the mercy of what else is
// ready. It gives up rather than looping forever.
// claimSpecific claims repeatedly until it gets the workflow asked for.
//
// IT REPORTS WHICH WAY IT GAVE UP, and the two are different diagnoses. Both
// returned the same errClaimEmpty until 2026-09-13, so a failing test said
// "claim returned nothing runnable" whichever had happened:
//
//	QUEUE EMPTY   ClaimWorkflow returned nil. The row is not claimable at all
//	              -- wrong status, a next_wake_at still in the future, a
//	              different tenant, or already assigned. Nothing was competing
//	              with it; it simply was not on offer.
//	BOUND HIT     50 other workflows were claimed and none was the one asked
//	              for. The row may be perfectly claimable and merely behind
//	              more work than the bound. This is the shape cleat#1447 is
//	              chasing next door, where a sweep bounded at TOP (100) skips
//	              the row a test is about.
//
// Both still satisfy errors.Is(err, errClaimEmpty), so any caller matching on
// that keeps working; what changed is what a human reads.
func claimSpecific(t *testing.T, ctx context.Context, store WorkflowStore, id, worker string) (*WorkflowInstance, error) {
	t.Helper()
	const maxClaims = 50
	for i := 0; i < maxClaims; i++ {
		wf, err := store.ClaimWorkflow(ctx, worker)
		if err != nil {
			return nil, err
		}
		if wf == nil {
			return nil, fmt.Errorf("%w: the queue went empty after %d claim(s) without "+
				"offering %s -- it is not claimable (status, next_wake_at, tenant or "+
				"assigned_to), rather than queued behind other work", errClaimEmpty, i, id)
		}
		if wf.ID == id {
			return wf, nil
		}
	}
	return nil, fmt.Errorf("%w: claimed %d other workflow(s) without reaching %s -- the "+
		"bound was hit, not the queue, so this says nothing about whether that row is "+
		"claimable", errClaimEmpty, maxClaims, id)
}

var errClaimEmpty = errors.New("claim did not reach the requested workflow")

// chainOf returns every run in a continue-as-new chain, head first.
func chainOf(t *testing.T, ctx context.Context, store WorkflowStore, head string) []string {
	t.Helper()
	finder, ok := store.(runSuccessorFinder)
	if !ok {
		t.Fatalf("%T cannot look up continue-as-new successors", store)
	}
	out := []string{head}
	cur := head
	for i := 0; i < 20; i++ {
		next, err := finder.successorOfRun(ctx, cur)
		if err != nil {
			t.Fatalf("successorOfRun(%s): %v", cur, err)
		}
		if next == "" {
			return out
		}
		out = append(out, next)
		cur = next
	}
	t.Fatalf("chain from %s did not terminate in 20 hops", head)
	return nil
}

func time0() time.Time { return time.Time{} }
