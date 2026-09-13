package engine

import (
	"context"
	"testing"
)

// TestPollChildTakesAnyRunInTheTenant pins the contract documented on
// HostCalls.PollChild and in docs/reference/sdk-api.md: the call is scoped by
// TENANT, not by parentage, despite the name.
//
// cleat#1120 was filed on the opposite belief -- that poll_child and
// await_child are children-only -- and a port was shelved for a year of
// work-list revisions because of it. Nobody had run it. The belief was
// plausible enough to be repeated in four files, so the contract needs a test
// that would notice if it changed, in either direction:
//
//   - if a parentage predicate is ever ADDED to GetChildResult, this fails and
//     says so, which is what a deliberate capability change should look like
//     rather than a silent break of every workflow observing a run it did not
//     spawn;
//   - the TENANT half of the claim is NOT tested here, deliberately. It is not
//     a property of this query: workflow_instances has RLS enabled with the
//     tenant_isolation_instances policy (migrations/postgres/001_schema.sql),
//     so the scope is enforced at the table and already covered by
//     completed_workflows_rls_test.go, dead_lettered_workflows_rls_test.go and
//     assert_tenant_set_test.go. A copy here would test the policy twice and
//     tell us nothing about GetChildResult.
//
// Deliberately at the store layer. PollChild adds the durable-clock logic on
// top and has its own mock-based tests; the parentage question is entirely
// decided by the query, and a mock cannot answer a question about SQL.
func TestPollChildTakesAnyRunInTheTenant(t *testing.T) {
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

			// Two unrelated parents, each with one child.
			parentA := newIntentWorkflow(t, ctx, store, "poll-any-parent-a")
			parentB := newIntentWorkflow(t, ctx, store, "poll-any-parent-b")

			childA, err := starter.StartChildWorkflow(ctx, parentA, "intent-workflow", `{}`, 1, "ABANDON", 0)
			if err != nil {
				t.Fatalf("start child A: %v", err)
			}
			childB, err := starter.StartChildWorkflow(ctx, parentB, "intent-workflow", `{}`, 1, "ABANDON", 0)
			if err != nil {
				t.Fatalf("start child B: %v", err)
			}

			finish := func(run, result string) {
				w, err := claimSpecific(t, ctx, store, run, "w")
				if err != nil {
					t.Fatalf("claim %s: %v", run, err)
				}
				if err := store.FinalizeWorkflowSegment(ctx, run, "w", w.Generation, nil,
					"done", result, "", "", map[string]string{}, time0()); err != nil {
					t.Fatalf("finalize %s: %v", run, err)
				}
			}
			const resultA = `{"owner":"A"}`
			const resultB = `{"owner":"B"}`
			finish(childA, resultA)
			finish(childB, resultB)

			// The known-positive. A parent reading its OWN child must work, or
			// the assertion below is being made by a broken fixture and would
			// report "restricted" for a store that simply cannot read anything.
			own, err := childStore.GetChildResult(ctx, childA)
			if err != nil {
				t.Fatalf("reading own child: %v", err)
			}
			if !own.Completed || own.Result != resultA {
				t.Fatalf("known-positive failed: a parent could not read its own child: "+
					"got (%q, completed=%v), want (%q, true)", own.Result, own.Completed, resultA)
			}

			// The contract: A reads B's child, which it did not spawn.
			foreign, err := childStore.GetChildResult(ctx, childB)
			if err != nil {
				t.Fatalf("reading an unrelated run: %v", err)
			}
			if !foreign.Completed || foreign.Result != resultB {
				t.Errorf("GetChildResult is scoped by PARENTAGE, but PollChild is documented "+
					"as taking any run id in the tenant.\n\n"+
					"reading an unrelated run returned (%q, completed=%v), want (%q, true).\n\n"+
					"If a parentage restriction was added deliberately, this test is the "+
					"contract it changes: update HostCalls.PollChild's doc comment and the "+
					"PollChild section of docs/reference/sdk-api.md, and note that every "+
					"workflow observing a run it did not spawn stops working. See cleat#1120.",
					foreign.Result, foreign.Completed, resultB)
			}
		})
	}
}
