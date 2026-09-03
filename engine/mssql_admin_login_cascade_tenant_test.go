package engine

// IMPROVEMENT-PLAN 3.92. §3.86 scoped the terminate and left the cascade.
//
// terminateWorkflowOnce calls enforceParentClosePolicy UNCONDITIONALLY after
// its commit, and never looks at how many rows the terminate touched. So once
// §3.86 put `AND tenant_id` on the terminate itself, a cross-tenant terminate
// stopped matching the parent -- and went on to close that parent's CHILDREN
// anyway, because both close-policy statements and childrenClosedByTerminate
// keyed on parent_workflow_id alone.
//
// Measured before the fix, on a dbo.cleat_admin pool:
//
//     childrenClosedByTerminate(B on A's parent) -> [006f8661-...]
//     AFTER: tenant A's child status="failed" error_msg="parent workflow terminated"
//
// THE RESULTING STATE IS WORSE THAN A PLAIN UNAUTHORISED WRITE, which is why
// this is asserted on the child rather than only on the cascade's return value.
// Tenant A's parent is untouched and still running -- §3.86 did its half
// correctly -- while A's child is failed with `parent workflow terminated`. The
// error names a cause that did not happen, and nothing in A's own history
// explains it. An operator debugging that has no thread to pull.
//
// WHY THIS WAS MISSED BY #616, which fixed the terminate itself: that PR
// asserted on the PARENT, and the parent was correct. The child is a different
// row reached by a different statement after the transaction has already
// committed, and nothing in the terminate's own test could see it.
//
// A NOTE ON THE FIX THIS IS NOT. adminForceResolve reaches the same cascade and
// is safe, because it checks RowsAffected and returns adminNotFound first --
// "do not cascade for a workflow you did not terminate" is the real bug, and
// the predicates here are its symptom-level twin. That fix changes what the
// HTTP layer returns for an unknown id (today: 200), so it is deliberately not
// bundled in. See the comment on enforceParentClosePolicy.

import (
	"context"
	"encoding/json"
	"testing"
)

const cascadeDefName = "cascade-close-policy"

// seedParentWithChild gives one tenant a parent and one child of the given
// close policy, and returns the child's generated id.
func seedParentWithChild(t *testing.T, s *MSSQLStore, tenant, parentID, policy string) string {
	t.Helper()
	ctx := context.Background()
	if err := s.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: cascadeDefName, Version: 1,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy for %s: %v", tenant, err)
	}
	if _, _, err := s.StartNewRun(ctx, parentID, cascadeDefName, 1,
		json.RawMessage(`{}`), "", tenant, 0); err != nil {
		t.Fatalf("start parent %s: %v", parentID, err)
	}
	childID, err := s.StartChildWorkflow(ctx, parentID, cascadeDefName, `{}`, 1, policy, 0)
	if err != nil {
		t.Fatalf("start %s child of %s: %v", policy, parentID, err)
	}
	return childID
}

func instanceStatus(t *testing.T, s *MSSQLStore, id, tenant string) (status, errMsg string) {
	t.Helper()
	var st, em *string
	if err := s.db.QueryRow(
		`SELECT status, error_msg FROM workflow_instances WHERE id = @p1 AND tenant_id = @p2`,
		id, tenant).Scan(&st, &em); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	if st != nil {
		status = *st
	}
	if em != nil {
		errMsg = *em
	}
	return status, errMsg
}

func TestAdminLoginTerminateCascadeReachesOnlyTheCallersOwnChildren(t *testing.T) {
	storeA, storeB := adminLoginStores(t)
	ctx := context.Background()

	t.Run("TERMINATE children", func(t *testing.T) {
		childA := seedParentWithChild(t, storeA, unscopedTenantA, "casc-parent-a", "TERMINATE")
		childB := seedParentWithChild(t, storeB, unscopedTenantB, "casc-parent-b", "TERMINATE")

		if err := storeB.TerminateWorkflow(ctx, "casc-parent-a", "not yours"); err != nil {
			t.Fatalf("cross-tenant TerminateWorkflow: %v", err)
		}
		if status, msg := instanceStatus(t, storeA, childA, unscopedTenantA); status == "failed" {
			t.Errorf("tenant B terminated a parent it does not own and tenant A's CHILD was "+
				"closed: status=%q error_msg=%q -- and A's parent is untouched, so that "+
				"message names a cause that did not happen", status, msg)
		}

		// Positive control: the cascade must still close the caller's OWN
		// children, or the predicate has disabled parent-close-policy rather
		// than scoping it -- which would leave orphans running forever and is
		// exactly what 3.79 added this cascade to prevent.
		if err := storeB.TerminateWorkflow(ctx, "casc-parent-b", "mine"); err != nil {
			t.Fatalf("own TerminateWorkflow: %v", err)
		}
		if status, msg := instanceStatus(t, storeB, childB, unscopedTenantB); status != "failed" {
			t.Errorf("tenant B's own TERMINATE child was NOT closed by its parent's terminate: "+
				"status=%q error_msg=%q -- the close policy is not being enforced at all",
				status, msg)
		}
	})

	// childrenClosedByTerminate is asserted separately because it is a read
	// that runs BEFORE the updates, and it is what decides whose concurrency
	// keys and sticky-worker assignments get released. Scoping the two UPDATEs
	// and leaving this one would release another tenant's resources while
	// correctly declining to touch their rows.
	t.Run("childrenClosedByTerminate", func(t *testing.T) {
		childA := seedParentWithChild(t, storeA, unscopedTenantA, "casc-list-a", "TERMINATE")

		got, err := storeB.childrenClosedByTerminate(ctx, "casc-list-a")
		if err != nil {
			t.Fatalf("childrenClosedByTerminate: %v", err)
		}
		for _, id := range got {
			if id == childA {
				t.Errorf("tenant B listed tenant A's child %s as closed by A's terminate; "+
					"its concurrency keys and sticky assignment would be released by B", id)
			}
		}

		own, err := storeA.childrenClosedByTerminate(ctx, "casc-list-a")
		if err != nil {
			t.Fatalf("childrenClosedByTerminate for the owner: %v", err)
		}
		var found bool
		for _, id := range own {
			if id == childA {
				found = true
			}
		}
		if !found {
			t.Errorf("tenant A could not list its OWN child %s: got %v -- the predicate has "+
				"broken the listing rather than scoped it, and those resources would be held "+
				"until TTL", childA, own)
		}
	})

	t.Run("REQUEST_CANCEL children", func(t *testing.T) {
		childA := seedParentWithChild(t, storeA, unscopedTenantA, "casc-rc-a", "REQUEST_CANCEL")
		childB := seedParentWithChild(t, storeB, unscopedTenantB, "casc-rc-b", "REQUEST_CANCEL")

		if err := storeB.TerminateWorkflow(ctx, "casc-rc-a", "not yours"); err != nil {
			t.Fatalf("cross-tenant TerminateWorkflow: %v", err)
		}
		if got := instanceField(t, storeA, "cancellation_requested", childA, unscopedTenantA); got != "0" {
			t.Errorf("tenant B flagged tenant A's REQUEST_CANCEL child cancelled: "+
				"cancellation_requested=%q", got)
		}

		if err := storeB.TerminateWorkflow(ctx, "casc-rc-b", "mine"); err != nil {
			t.Fatalf("own TerminateWorkflow: %v", err)
		}
		if got := instanceField(t, storeB, "cancellation_requested", childB, unscopedTenantB); got != "1" {
			t.Errorf("tenant B's own REQUEST_CANCEL child was NOT flagged: "+
				"cancellation_requested=%q -- the close policy is not being enforced", got)
		}
	})
}
