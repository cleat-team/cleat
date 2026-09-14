package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// A child that has reached a terminal status must stop counting against its
// parent's child-workflow quota. cleat#1153 groundwork.
//
// WHY THIS IS A QUOTA TEST AND NOT A COUNTING TEST. GetChildCount has exactly
// one production caller -- engine/children.go's childWorkflowWithVersion, which
// compares it against maxQuotaChildren before creating a child and refuses with
// "child workflow quota exceeded" when it is reached. So a status the count
// wrongly includes is not a cosmetic miscount: it is quota a parent never gets
// back. A long-lived parent that spawns and disposes of children in a loop
// eventually cannot spawn at all, and the children holding its budget are
// finished.
//
// WHAT THIS FOUND, and it is why the case list is all four rather than one:
// three of the four terminal statuses were excluded and 'terminated' was not,
// identically on all three dialects --
//
//	engine/store_children.go       GetChildCount (postgres)
//	engine/mysql_ops.go            GetChildCount (mysql)
//	engine/mssql_signals_promises.go GetChildCount (mssql)
//
// each reading `status NOT IN ('done', 'failed', 'dead_lettered')` under a doc
// comment that says "Terminal statuses are excluded". The three that pass are
// the control: they show the predicate understands the question and misses
// exactly one value, which is a stronger statement than a single failing case.
// Same shape as cleat#1227, where an incomplete terminal list let a parent
// overwrite a dead-lettered child.
//
// 'terminating' is deliberately NOT a case here. A child mid-shutdown is still
// running its defer phase and SHOULD hold quota -- it can still do work. The
// list this guards is "settled", not "no longer runs guest code", and those are
// two different questions that engine/ and cmd/cleat-worker/ answer separately.
func TestATerminalChildDoesNotHoldItsParentsQuota(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			cases := []struct {
				status string
				drive  func(t *testing.T, childID string)
			}{
				{"done", func(t *testing.T, id string) {
					wf := claimChildOrFail(t, ctx, store, id, "w-done")
					if err := store.CompleteWorkflow(ctx, id, "w-done", wf.Generation, `{"ok":true}`, nil); err != nil {
						t.Fatalf("CompleteWorkflow: %v", err)
					}
				}},
				{"failed", func(t *testing.T, id string) {
					wf := claimChildOrFail(t, ctx, store, id, "w-failed")
					if err := store.FailWorkflow(ctx, id, "w-failed", wf.Generation, "boom", "E_TEST", "op", nil); err != nil {
						t.Fatalf("FailWorkflow: %v", err)
					}
				}},
				{"dead_lettered", func(t *testing.T, id string) {
					wf := claimChildOrFail(t, ctx, store, id, "w-dlq")
					if err := store.MoveToDeadLetterQueue(ctx, id, "w-dlq", wf.Generation, "retries exhausted", "E_RETRY", "op"); err != nil {
						t.Fatalf("MoveToDeadLetterQueue: %v", err)
					}
				}},
				{"terminated", func(t *testing.T, id string) {
					// No claim: TerminateWorkflow does not require the worker
					// identity. A child with no registered defers goes straight
					// to 'terminated' rather than through a defer phase, which
					// the status assertion below confirms rather than assumes.
					if err := store.TerminateWorkflow(ctx, id, "terminated by test"); err != nil {
						t.Fatalf("TerminateWorkflow: %v", err)
					}
				}},
			}

			for _, tc := range cases {
				tc := tc
				t.Run(tc.status, func(t *testing.T) {
					parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
						json.RawMessage(`{}`), "quota-parent-"+tc.status, DefaultTenantUUID, 0)
					if err != nil {
						t.Fatalf("StartNewRun (parent): %v", err)
					}

					childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{}`, 1, "abandon", 0)
					if err != nil {
						t.Fatalf("StartChildWorkflow: %v", err)
					}

					// CONTROL. Without this, "0 afterwards" is satisfied by a
					// count that is always 0 -- a broken query, the wrong
					// parent id, or a child that was never created would all
					// pass the assertion this test is actually about.
					before, err := store.GetChildCount(ctx, parentID)
					if err != nil {
						t.Fatalf("GetChildCount (before): %v", err)
					}
					if before != 1 {
						t.Fatalf("before: count = %d, want 1 -- the child was not counted while "+
							"ACTIVE, so this case cannot say anything about it going terminal", before)
					}

					tc.drive(t, childID)

					// The transition really happened. TerminateWorkflow in
					// particular may leave a workflow in 'terminating' when it
					// owes defers, and a test asserting a quota release off a
					// status that was never reached proves nothing.
					got, err := store.GetWorkflowByID(ctx, childID)
					if err != nil {
						t.Fatalf("GetWorkflowByID: %v", err)
					}
					if got == nil {
						t.Fatalf("child %s disappeared", childID)
					}
					if got.Status != tc.status {
						t.Fatalf("child status = %q, want %q -- the case did not reach the "+
							"state it is named for, so the count below measures something else",
							got.Status, tc.status)
					}

					after, err := store.GetChildCount(ctx, parentID)
					if err != nil {
						t.Fatalf("GetChildCount (after): %v", err)
					}
					if after != 0 {
						t.Errorf("a child in terminal status %q still counts against its parent's "+
							"quota: GetChildCount = %d, want 0.\n\n"+
							"GetChildCount's own comment says \"Terminal statuses are excluded\", and "+
							"its one production caller (engine/children.go) compares this against "+
							"maxQuotaChildren. So this is quota the parent never gets back.\n\n"+
							"The predicate is hand-written once per dialect. Check all three:\n"+
							"  engine/store_children.go, engine/mysql_ops.go, engine/mssql_signals_promises.go",
							tc.status, after)
					}
				})
			}
		})
	}
}

// claimChildOrFail claims a specific child and fails the test if it cannot,
// rather than returning an error every caller would have to unwrap identically.
func claimChildOrFail(t *testing.T, ctx context.Context, store WorkflowStore, id, worker string) *WorkflowInstance {
	t.Helper()
	wf, err := claimSpecific(t, ctx, store, id, worker)
	if err != nil {
		t.Fatalf("claim child %s as %s: %v", id, worker, err)
	}
	return wf
}
