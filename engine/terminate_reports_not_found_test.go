package engine

// IMPROVEMENT-PLAN 3.92, the root rather than the symptom.
//
// terminateWorkflowOnce exec'd its UPDATE, ignored how many rows it touched,
// and then ran enforceParentClosePolicy unconditionally. While the UPDATE
// matched on id alone that was harmless -- a terminate that hit nothing was a
// terminate of a workflow that did not exist, and its children did not exist
// either. 3.86 changed that by adding `AND tenant_id`: the terminate stopped
// matching, and the cascade went on to close a parent's CHILDREN anyway,
// because the close-policy statements key on parent_workflow_id.
//
// 3.92 scoped those statements, which stops the damage. This is the thing they
// were the symptom-level twin of: DO NOT CASCADE FOR A WORKFLOW YOU DID NOT
// TERMINATE. adminForceResolve has always worked this way -- check
// RowsAffected, return not-found, never reach the cascade -- and this brings
// terminate into line with it.
//
// WHAT THIS TEST DOES NOT PROVE, said out loud because the first draft of it
// claimed otherwise. It asserted that an unmatched terminate leaves a real
// child alone -- and that assertion CANNOT FAIL. Removing the guard on any
// dialect turned this test red on the error, never on the child, because
// within one tenant an unmatched terminate is an id no row carries: the
// cascade's UPDATEs key on parent_workflow_id and match nothing, and
// releaseWorkflowResources' two calls match nothing either. There is no
// observable difference. The assertion was removed rather than kept as
// decoration.
//
// The cross-tenant case, where the id IS a real parent and the cascade would
// have closed its children, is covered by
// engine/mssql_admin_login_cascade_tenant_test.go -- and 3.92's predicates
// already block it there, which is the honest reason this change is defence in
// depth rather than a fix for a live defect. Its observable contribution is the
// ERROR CONTRACT: the call no longer tells a caller it terminated something
// when it terminated nothing.
//
// The error is deliberately one error and not two: ErrWorkflowNotFound does not
// distinguish "no such workflow" from "another tenant's", for the reason its own
// doc comment gives. Splitting them would rebuild the existence oracle 3.101
// closed at the HTTP layer.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestTerminateReportsNotFoundForAWorkflowItDidNotTerminate(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)

			// A real parent with a TERMINATE child, which must survive a
			// terminate aimed at a DIFFERENT, absent id. If the cascade runs
			// for an unmatched terminate it will not touch this child either --
			// so the child exists to prove the positive control below, and the
			// absent id proves the negative.
			const parentID = "term-nf-parent"
			if _, _, err := store.StartNewRun(ctx, parentID, "test-workflow", 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun(parent): %v", err)
			}
			childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{}`, 1, "TERMINATE", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}

			// The absent id. Not a tenant trick -- just an id no row carries,
			// which is the same zero-rows outcome a cross-tenant id produces
			// and the one this test can reach on every dialect.
			err = store.TerminateWorkflow(ctx, "term-nf-no-such-workflow", "nobody")
			if !errors.Is(err, ErrWorkflowNotFound) {
				t.Errorf("terminating an id that does not exist returned %v, want ErrWorkflowNotFound -- "+
					"returning nil tells the caller a workflow was terminated when none was", err)
			}

			// Positive control. Terminating the parent for real must still
			// cascade, or this change has disabled the close policy rather
			// than scoping it -- reintroducing 3.79, whose whole subject is a
			// terminated parent leaving its TERMINATE children running.
			if err := store.TerminateWorkflow(ctx, parentID, "for real"); err != nil {
				t.Fatalf("terminating the parent: %v", err)
			}
			wf, err := store.GetWorkflowByID(ctx, childID)
			if err != nil {
				t.Fatalf("GetWorkflowByID(child, after): %v", err)
			}
			if wf == nil || wf.Status != "failed" {
				var got string
				if wf != nil {
					got = wf.Status
				}
				t.Errorf("terminating the parent did NOT close its TERMINATE child: status=%q, "+
					"want \"failed\" -- the close policy is no longer being enforced at all", got)
			}

			// And a terminate of an ALREADY terminated workflow still succeeds:
			// the UPDATE carries no status filter, so this stays idempotent and
			// only a genuinely absent row reports not-found.
			if err := store.TerminateWorkflow(ctx, parentID, "again"); err != nil {
				t.Errorf("re-terminating an already terminated workflow returned %v, want nil -- "+
					"terminate is idempotent and this change must not have made it a one-shot", err)
			}
		})
	}
}
