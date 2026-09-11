// cleat#1115. A child that FAILED was reported to its parent as a success with
// an empty result.
//
// The information never left the store. finalize_workflow_status writes a
// failed run's message to `error_msg` (migration 053, the `p_result` parameter
// on the 'failed' branch); GetChildResult selected `COALESCE(result, '{}')` and
// `status`, treated 'done' and 'failed' as one condition, and returned no way
// to tell them apart. So AwaitChild saw (result="{}", completed=true, err=nil)
// -- err being a STORE error, not the child's -- and took its success branch.
//
// The natural guest shape takes the wrong branch silently:
//
//	result, err := h.AwaitChild(childID)
//	if err != nil { /* never reached */ }
//
// and an empty result is a plausible success value, so nothing downstream looks
// wrong either. That is worse than a missing error: the parent is told the
// opposite of what happened.
//
// THIS TEST SPANS BOTH HALVES ON PURPOSE. A store-level check would pass
// against a session that ignored the new field, and a session-level check with
// a mock store would pass against a store that never reads error_msg -- which
// is the half that was actually broken. So it drives the real store through
// the real AwaitChild.
//
// THE CONTROL IS LOAD-BEARING. A sibling child of the same parent succeeds and
// must still be reported as a success carrying its result. Without it, "the
// failed child is reported as failed" is equally satisfied by a change that
// reports everything as failed.
package engine

import (
	"context"
	"strings"
	"testing"
)

func TestAParentIsToldWhenItsChildFailed(t *testing.T) {
	const marker = "child failed: cleat-1115-marker"
	const siblingResult = `{"sibling":"finished"}`

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
			childStore, ok := store.(ChildWorkflowStore)
			if !ok {
				t.Fatalf("%T is not a ChildWorkflowStore", store)
			}

			parent := newIntentWorkflow(t, ctx, store, "failing-child-parent")

			failing, err := starter.StartChildWorkflow(ctx, parent, "intent-workflow", `{}`, 1, "ABANDON", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow (failing): %v", err)
			}
			sibling, err := starter.StartChildWorkflow(ctx, parent, "intent-workflow", `{}`, 1, "ABANDON", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow (control sibling): %v", err)
			}

			// Claim BOTH children in one pass and keep each generation.
			// ClaimWorkflow takes whatever is runnable, so claiming one child
			// by looping until the id matches leaves the OTHER one claimed and
			// unclaimable, and the second lookup then fails. It passed on
			// postgres and mysql, where the order happened to suit, and failed
			// on mssql -- an order dependence that reads as a dialect problem.
			gens := map[string]int64{}
			for i := 0; i < 50 && len(gens) < 2; i++ {
				wf, err := store.ClaimWorkflow(ctx, "w-child")
				if err != nil {
					t.Fatalf("ClaimWorkflow: %v", err)
				}
				if wf == nil {
					break
				}
				if wf.ID == failing || wf.ID == sibling {
					gens[wf.ID] = wf.Generation
				}
			}
			if len(gens) != 2 {
				t.Fatalf("claimed %d of the 2 children; cannot seed the fixture", len(gens))
			}

			// FailWorkflow, not FinalizeWorkflowSegment(..., "failed", msg).
			// The worker fails a run through FailWorkflow, whose errorMsg
			// parameter reaches error_msg as written. The finalize path runs
			// its payload through coerceResultJSON first, which replaces
			// anything that is not valid JSON with "{}" -- so a plain-text
			// message seeded that way arrives as "{}", and a test asserting on
			// it measures the coercion rather than the fix. Measured: this
			// test read Err="{}" until it used the path the worker uses.
			if err := store.FailWorkflow(ctx, failing, "w-child", gens[failing],
				marker, "", "", map[string]string{}); err != nil {
				t.Fatalf("FailWorkflow: %v", err)
			}
			if err := store.FinalizeWorkflowSegment(ctx, sibling, "w-child", gens[sibling], nil,
				"done", siblingResult, "", "", map[string]string{}, time0()); err != nil {
				t.Fatalf("finalize the control sibling as done: %v", err)
			}

			// --- the control, first, so a blanket "everything failed" cannot
			// pass the assertion below ---
			ctl := newTestExecSession()
			ctl.engine.childWfStore = childStore
			if code := awaitCode(ctl.AwaitChild(ctx, nil, sibling, 0, 0)); code != 0 {
				t.Fatalf("CONTROL FAILED: awaiting a child that SUCCEEDED returned error code %d, "+
					"want 0. Nothing below distinguishes a fix from a store that reports every "+
					"child as failed.", code)
			}
			if len(ctl.history) != 1 || ctl.history[0].Response != siblingResult {
				t.Fatalf("CONTROL FAILED: the successful child's result was not recorded; got %+v", ctl.history)
			}

			// --- the subject ---
			s := newTestExecSession()
			s.engine.childWfStore = childStore
			code := awaitCode(s.AwaitChild(ctx, nil, failing, 0, 0))
			if code != 1 {
				t.Errorf("a parent awaiting a child that FAILED got error code %d, want 1.\n\n"+
					"The child's status is 'failed' and its message is in error_msg. Reporting "+
					"success here does not merely withhold the reason -- the guest's "+
					"`if err != nil` takes the wrong branch, and an empty result is a plausible "+
					"success value, so nothing downstream looks wrong either (cleat#1115).", code)
			}
			if len(s.history) != 1 {
				t.Fatalf("expected one recorded event, got %d", len(s.history))
			}
			if got := s.history[0].Err; !strings.Contains(got, marker) {
				t.Errorf("the recorded await_child event carries Err=%q, want it to contain %q.\n\n"+
					"The message is what the parent can act on, and it is what replay will "+
					"return on the next execution. An error flag with no message moves the "+
					"defect rather than fixing it.", got, marker)
			}
			if s.history[0].Response != "" {
				t.Errorf("the recorded await_child event carries Response=%q for a FAILED child; "+
					"a failure must not also present as a result", s.history[0].Response)
			}
		})
	}
}

// awaitCode extracts the error flag from a packAwaitChildResult return.
func awaitCode(packed int64) uint32 { return uint32(packed & 0xFFFFFFFF) }
