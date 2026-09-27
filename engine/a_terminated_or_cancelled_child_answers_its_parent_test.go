// cleat#1974. A parent awaiting a TERMINATED or CANCELLED child never got an
// answer. GetChildResult mapped 'failed'/'dead_lettered' to a failure and
// 'done' to a success -- every other status, including 'terminated' and
// 'cancelled', fell through to ChildOutcome{}, which AwaitChild reads as
// "not yet". The parent suspended, was re-claimed on its next_wake_at,
// replayed, got the same non-answer and suspended again, for the life of the
// deployment -- the exact shape cleat#1213 (dead_lettered) and the
// GetChildCount bug (a_terminal_child_does_not_hold_its_parents_quota_test.go)
// already fixed for their own callers. This is the same defect in the one
// caller that decides whether the parent stops waiting at all.
//
// PER THE OWNER'S DESIGN DECISION (2026-09-22, failure-model-2026-09-22.md
// §2/§6): no new SDK surface. A terminated/cancelled child arrives as a
// FAILED child, same as today's 'failed'/'dead_lettered' case, and the KIND
// travels as a stable message prefix -- "[TERMINATED] "/"[CANCELLED] " --
// the same convention "[AMBIGUOUS]" already uses (durablecalls.go,
// heartbeats.go). ChildOutcome's own doc comment already says cancellation
// "needs no third case": Completed+Failed, not a new field.
//
// THE CONTROL IS LOAD-BEARING, same reason as cleat#1115's test
// (child_failure_reaches_the_parent_test.go): a store that reports every
// child as failed would pass an assertion that only checks the two broken
// statuses. A done sibling in the same fixture must still read back as a
// clean success.
package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAParentIsToldWhenItsChildIsTerminatedOrCancelled(t *testing.T) {
	const terminatedReason = "cleat-1974-terminated-marker"
	const cancelledReason = "cleat-1974-cancelled-marker"
	const siblingResult = `{"sibling":"finished"}`

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			children, ok := store.(ChildWorkflowStore)
			if !ok {
				t.Fatalf("%T does not implement ChildWorkflowStore", store)
			}

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "terminated-cancelled-parent", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun(parent): %v", err)
			}

			terminatedChild, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{}`, 1, "ABANDON", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow (terminated): %v", err)
			}
			cancelledChild, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{}`, 1, "ABANDON", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow (cancelled): %v", err)
			}
			sibling, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{}`, 1, "ABANDON", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow (control sibling): %v", err)
			}

			// TerminateWorkflow/CancelWorkflow are force operations -- unlike
			// MoveToDeadLetterQueue, they carry no claim/generation fence
			// (see PostgresStore.TerminateWorkflow's own doc comment: "an
			// ALREADY-terminated workflow still matches"), so the children
			// need no claiming step here.
			if err := store.TerminateWorkflow(ctx, terminatedChild, terminatedReason); err != nil {
				t.Fatalf("TerminateWorkflow: %v", err)
			}
			if err := store.CancelWorkflow(ctx, cancelledChild, cancelledReason); err != nil {
				t.Fatalf("CancelWorkflow: %v", err)
			}

			// The control sibling settles the ordinary way, claimed then
			// finalized -- same as the cleat#1115 regression test.
			gen, err := claimGeneration(t, store, sibling)
			if err != nil {
				t.Fatalf("claim the control sibling: %v", err)
			}
			if err := store.FinalizeWorkflowSegment(ctx, sibling, "w-1974", gen, nil,
				"done", siblingResult, "", "", map[string]string{}, time0()); err != nil {
				t.Fatalf("finalize the control sibling as done: %v", err)
			}

			// The premise, for both subjects: without it, a store that
			// silently failed to terminate/cancel the row would make every
			// assertion below pass for the wrong reason -- the child would
			// just be running, and "not completed" would be correct.
			assertStatus(t, store, terminatedChild, "terminated")
			assertStatus(t, store, cancelledChild, "cancelled")

			// --- store level: GetChildResult directly ---
			terminatedOut, err := children.GetChildResult(ctx, terminatedChild)
			if err != nil {
				t.Fatalf("GetChildResult(terminated): %v", err)
			}
			assertTerminalFailure(t, "terminated", terminatedOut, "[TERMINATED] "+terminatedReason)

			cancelledOut, err := children.GetChildResult(ctx, cancelledChild)
			if err != nil {
				t.Fatalf("GetChildResult(cancelled): %v", err)
			}
			assertTerminalFailure(t, "cancelled", cancelledOut, "[CANCELLED] "+cancelledReason)

			// --- session level: the real AwaitChild path a guest takes ---

			// Control first, so a blanket "everything failed" change cannot
			// pass the assertions that follow.
			ctl := newTestExecSession()
			ctl.engine.childWfStore = children
			if code := awaitCode(ctl.AwaitChild(ctx, nil, sibling, 0, 0)); code != 0 {
				t.Fatalf("CONTROL FAILED: awaiting a child that SUCCEEDED returned error code %d, "+
					"want 0. Nothing below distinguishes a fix from a store that reports every "+
					"child as failed.", code)
			}
			if len(ctl.history) != 1 || ctl.history[0].Response != siblingResult {
				t.Fatalf("CONTROL FAILED: the successful child's result was not recorded; got %+v", ctl.history)
			}

			term := newTestExecSession()
			term.engine.childWfStore = children
			code := awaitCode(term.AwaitChild(ctx, nil, terminatedChild, 0, 0))
			if code != 1 {
				t.Errorf("a parent awaiting a TERMINATED child got error code %d, want 1 -- "+
					"AwaitChild reads a non-completed ChildOutcome as \"not yet\", so this parent "+
					"suspends and, on every re-claim, replays into the same non-answer forever.", code)
			}
			if len(term.history) != 1 {
				t.Fatalf("expected one recorded event for the terminated child, got %d", len(term.history))
			}
			if got := term.history[0].Err; !strings.HasPrefix(got, "[TERMINATED] "+terminatedReason) {
				t.Errorf("the recorded await_child event for the terminated child carries Err=%q, "+
					"want it to start with %q", got, "[TERMINATED] "+terminatedReason)
			}
			if term.history[0].Response != "" {
				t.Errorf("the recorded await_child event for the terminated child carries "+
					"Response=%q; a failure must not also present as a result", term.history[0].Response)
			}

			canc := newTestExecSession()
			canc.engine.childWfStore = children
			code = awaitCode(canc.AwaitChild(ctx, nil, cancelledChild, 0, 0))
			if code != 1 {
				t.Errorf("a parent awaiting a CANCELLED child got error code %d, want 1 (same "+
					"livelock as the terminated case above)", code)
			}
			if len(canc.history) != 1 {
				t.Fatalf("expected one recorded event for the cancelled child, got %d", len(canc.history))
			}
			if got := canc.history[0].Err; !strings.HasPrefix(got, "[CANCELLED] "+cancelledReason) {
				t.Errorf("the recorded await_child event for the cancelled child carries Err=%q, "+
					"want it to start with %q", got, "[CANCELLED] "+cancelledReason)
			}
			if canc.history[0].Response != "" {
				t.Errorf("the recorded await_child event for the cancelled child carries "+
					"Response=%q; a failure must not also present as a result", canc.history[0].Response)
			}
		})
	}
}

// assertStatus fails the test if workflowID's status is not want -- the
// premise every assertion after it depends on.
func assertStatus(t *testing.T, store WorkflowStore, workflowID, want string) {
	t.Helper()
	wf, err := store.GetWorkflowByID(context.Background(), workflowID)
	if err != nil || wf == nil {
		t.Fatalf("GetWorkflowByID(%s): %v (nil=%v)", workflowID, err, wf == nil)
	}
	if wf.Status != want {
		t.Fatalf("premise failed: %s is %q, not %q -- nothing below is a test of cleat#1974",
			workflowID, wf.Status, want)
	}
}

// assertTerminalFailure checks the shared shape of a settled-but-not-done
// ChildOutcome: complete, failed, the expected error prefix, and no result --
// a settled-but-not-done run's `result` column is never written (migration
// 053 routes finalize's payload to error_msg on that branch), so a non-empty
// Result here would be the COALESCE/ISNULL default passed off as a result.
func assertTerminalFailure(t *testing.T, label string, out ChildOutcome, wantErrPrefix string) {
	t.Helper()
	if !out.Completed {
		t.Errorf("%s child reports Completed=false, want true", label)
	}
	if !out.Failed {
		t.Errorf("%s child reports Failed=false, want true -- Completed without Failed is how "+
			"a SUCCESS is spelled (cleat#1115)", label)
	}
	if out.Error != wantErrPrefix {
		t.Errorf("%s child reports Error=%q, want %q", label, out.Error, wantErrPrefix)
	}
	if out.Result != "" {
		t.Errorf("%s child carries Result=%q, want empty", label, out.Result)
	}
}

// claimGeneration claims workflows until workflowID is seen and returns its
// generation. ClaimWorkflow takes whatever is runnable next, so this can
// claim (and leave claimed) other rows in the same fixture along the way --
// matching child_failure_reaches_the_parent_test.go's own note that looping
// a single-child claim left a sibling claimed-and-unclaimable on mssql.
func claimGeneration(t *testing.T, store WorkflowStore, workflowID string) (int64, error) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		wf, err := store.ClaimWorkflow(ctx, "w-1974")
		if err != nil {
			return 0, err
		}
		if wf == nil {
			break
		}
		if wf.ID == workflowID {
			return wf.Generation, nil
		}
	}
	t.Fatalf("could not claim %s", workflowID)
	return 0, nil
}

// --- AwaitAllChildren: fan-out over a mix of terminated/cancelled/done ---

// terminatedCancelledDoneStore answers GetChildResult from a fixed map,
// mirroring fakeChildResultStore (children_await_all_test.go) but able to
// express Failed+Error, which that type cannot. AwaitAllChildren's own
// fan-out logic is store-agnostic once GetChildResult returns the right
// ChildOutcome -- the per-dialect correctness of THAT is what
// TestAParentIsToldWhenItsChildIsTerminatedOrCancelled covers above -- so a
// fake here is testing the aggregation, not re-testing the three stores.
type terminatedCancelledDoneStore struct {
	outcomes map[string]ChildOutcome
}

func (f *terminatedCancelledDoneStore) StartChildWorkflow(context.Context, string, string, string, int, string, int) (string, error) {
	return "", nil
}

func (f *terminatedCancelledDoneStore) StartChildWorkflowAtomic(context.Context, string, string, string, string, int, string, EventRecord, int) (string, error) {
	return "", nil
}

func (f *terminatedCancelledDoneStore) GetChildResult(_ context.Context, runID string) (ChildOutcome, error) {
	out, ok := f.outcomes[runID]
	if !ok {
		return ChildOutcome{}, nil
	}
	return out, nil
}

func (f *terminatedCancelledDoneStore) ResolveVersionByTag(context.Context, string, string) (int, error) {
	return 0, nil
}

func (f *terminatedCancelledDoneStore) GetChildCompletedAtMs(ctx context.Context, runID string) (int64, bool, error) {
	if _, ok := f.outcomes[runID]; ok {
		return 0, true, nil
	}
	return 0, false, nil
}

func TestAwaitAllChildrenReportsTerminatedAndCancelledChildren(t *testing.T) {
	store := &terminatedCancelledDoneStore{outcomes: map[string]ChildOutcome{
		"child-done":       {Completed: true, Result: `{"tag":"done"}`},
		"child-terminated": {Completed: true, Failed: true, Error: "[TERMINATED] op said so"},
		"child-cancelled":  {Completed: true, Failed: true, Error: "[CANCELLED] op said so"},
	}}

	s := newTestExecSession()
	s.engine = NewEngine(nil, nil, WithChildWorkflowStore(store))

	runIDs := `["child-done","child-terminated","child-cancelled"]`
	s.AwaitAllChildren(context.Background(), nil, runIDs, 0, 0)

	if len(s.history) != 1 {
		t.Fatalf("expected exactly one recorded event, got %d", len(s.history))
	}
	rec := s.history[0]
	if rec.EventType != EventTypeAwaitAllChildren {
		t.Fatalf("recorded %q, want %q", rec.EventType, EventTypeAwaitAllChildren)
	}
	if rec.Response == "" {
		t.Fatal("no response recorded: every child was settled, so this should not have suspended " +
			"-- a terminated/cancelled child not counting as settled is exactly cleat#1974")
	}

	var outcomes []struct {
		RunID  string `json:"run_id"`
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(rec.Response), &outcomes); err != nil {
		t.Fatalf("unmarshal recorded response: %v\nraw: %s", err, rec.Response)
	}
	got := map[string]string{}
	for _, o := range outcomes {
		if o.Error != "" {
			got[o.RunID] = o.Error
		} else {
			got[o.RunID] = "OK:" + o.Result
		}
	}
	want := map[string]string{
		"child-done":       `OK:{"tag":"done"}`,
		"child-terminated": "[TERMINATED] op said so",
		"child-cancelled":  "[CANCELLED] op said so",
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("child %s: got %q, want %q", id, got[id], w)
		}
	}
}
