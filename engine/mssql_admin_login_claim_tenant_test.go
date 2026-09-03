package engine

// IMPROVEMENT-PLAN 3.91. The ORDINARY claim path -- not the one whose name says
// "AcrossTenants" -- claimed every tenant's ready work on SQL Server.
//
// This is 3.86's mechanism reaching the statement that matters most, and it was
// missed by three hand audits and by scripts/mssql-tenant-predicate-audit.py,
// which looks for `tenant_id` anywhere in the statement and finds it: the claim
// projects CONVERT(NVARCHAR(36), INSERTED.tenant_id) in its OUTPUT clause, and
// carries a long comment about that conversion. Neither scopes anything. A
// position-aware check -- does the column appear in a WHERE, an ON or a HAVING
// -- is what surfaced it.
//
// THE THREE DIALECTS DISAGREED, WHICH IS THE TELL:
//
//   MySQL       AND tenant_id = ? in the candidate SELECT. Explicit.
//   PostgreSQL  no Go predicate, but claims inside beginTxWithRLS, and the
//               application role does NOT hold BYPASSRLS -- so RLS really does
//               scope it. That is the documented design (3.86).
//   SQL Server  no Go predicate AND no enforcement, because dbo.fn_tenant_filter
//               is off for any dbo.cleat_admin login (012_admin_role.sql).
//
// So SQL Server was the only dialect with nothing enforcing it, and the fix is
// to match MySQL rather than to make a judgement call.
//
// WHY THE GRANT IS ALWAYS PRESENT WHERE IT MATTERS. requireCleatAdminMembership
// checks s.db -- the SAME POOL claimWorkflowsOnce uses. So on any deployment
// where ClaimWorkflowsAcrossTenants works at all, the ordinary claim was already
// unscoped, and the -claim-across-tenants flag was guarding a widening that had
// already happened unconditionally. The flag was decorative on this dialect.
//
// Measured before the fix, tenant B's ORDINARY ClaimWorkflows:
//
//     returned 2 instances:
//         id=probe-tenant-a-wf tenant=AAAAAAAA-AAAA-4AAA-AAAA-AAAAAAAAAAAA
//         id=probe-tenant-b-wf tenant=BBBBBBBB-BBBB-4BBB-BBBB-BBBBBBBBBBBB
//
// NOTE THE CASE, because the first probe of this passed while printing that.
// CONVERT(NVARCHAR(36), tenant_id) returns UPPERCASE and the fixture constants
// are lowercase, so `wf.TenantID == unscopedTenantA` was false for a row that
// plainly belonged to tenant A. Every comparison here is case-insensitive, and
// the same hazard is live in cmd/cleat-worker/setup.go:storeForTenant, which
// compares tenant strings with ==.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const claimDefName = "claim-scope-probe"

// armReadyWorkflow creates a workflow for one tenant and makes it claimable.
func armReadyWorkflow(t *testing.T, s *MSSQLStore, tenant, id, stickyWorker string) {
	t.Helper()
	ctx := context.Background()
	if err := s.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: claimDefName, Version: 1,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil && !strings.Contains(err.Error(), "PRIMARY KEY") {
		t.Fatalf("deploy for %s: %v", tenant, err)
	}
	if _, _, err := s.StartNewRun(ctx, id, claimDefName, 1, json.RawMessage(`{}`), "", tenant, 0); err != nil {
		t.Fatalf("StartNewRun(%s): %v", id, err)
	}
	// next_wake_at is NULL on a fresh run and the candidate SELECT requires
	// `next_wake_at <= SYSUTCDATETIME()`, so without this the fixture is
	// unclaimable and every assertion below passes by finding nothing.
	res, err := s.db.Exec(`
		UPDATE workflow_instances
		SET next_wake_at = SYSUTCDATETIME(), status = 'ready', sticky_worker_id = @p3
		WHERE id = @p1 AND tenant_id = @p2`, id, tenant, nullIfEmpty(stickyWorker))
	if err != nil {
		t.Fatalf("arm %s: %v", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("arming %s affected %d rows, want 1; the fixture is broken", id, n)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func claimedWorkflowIDs(wfs []*WorkflowInstance) []string {
	out := make([]string, 0, len(wfs))
	for _, wf := range wfs {
		out = append(out, wf.ID)
	}
	return out
}

// containsTenant answers case-insensitively, which is the whole point -- see
// the note on CONVERT in this file's header.
func containsTenant(wfs []*WorkflowInstance, tenant string) bool {
	for _, wf := range wfs {
		if strings.EqualFold(wf.TenantID, tenant) {
			return true
		}
	}
	return false
}

func TestAdminLoginOrdinaryClaimTakesOnlyTheCallersOwnWork(t *testing.T) {
	storeA, storeB := adminLoginStores(t)
	ctx := context.Background()

	t.Run("ClaimWorkflows", func(t *testing.T) {
		armReadyWorkflow(t, storeA, unscopedTenantA, "claim-a-plain", "")
		armReadyWorkflow(t, storeB, unscopedTenantB, "claim-b-plain", "")

		got, err := storeB.ClaimWorkflows(ctx, "worker-b", 10)
		if err != nil {
			t.Fatalf("ClaimWorkflows: %v", err)
		}
		if containsTenant(got, unscopedTenantA) {
			t.Errorf("tenant B's ORDINARY claim took tenant A's work: claimed %v", claimedWorkflowIDs(got))
		}
		// Positive control. Without it, a predicate naming the wrong column
		// would make the claim return nothing and the assertion above would
		// pass while the worker starved.
		if !containsTenant(got, unscopedTenantB) {
			t.Errorf("tenant B's claim did not return its OWN ready workflow: claimed %v", claimedWorkflowIDs(got))
		}
	})

	t.Run("ClaimStickyWorkflows", func(t *testing.T) {
		armReadyWorkflow(t, storeA, unscopedTenantA, "claim-a-sticky", "shared-worker")
		armReadyWorkflow(t, storeB, unscopedTenantB, "claim-b-sticky", "shared-worker")

		got, err := storeB.ClaimStickyWorkflows(ctx, "shared-worker", 10)
		if err != nil {
			t.Fatalf("ClaimStickyWorkflows: %v", err)
		}
		if containsTenant(got, unscopedTenantA) {
			t.Errorf("tenant B's sticky claim took tenant A's work: claimed %v", claimedWorkflowIDs(got))
		}
		if !containsTenant(got, unscopedTenantB) {
			t.Errorf("tenant B's sticky claim did not return its OWN workflow: claimed %v", claimedWorkflowIDs(got))
		}
	})

	// The must-not-break control, and the reason this file is not just two
	// assertions. ClaimWorkflowsAcrossTenants is SUPPOSED to see every tenant;
	// scoping the ordinary path must not scope this one too, or a multi-tenant
	// deployment's dispatch loop stops seeing the tenants it exists to serve
	// and every non-default tenant's workflows stop running.
	t.Run("ClaimWorkflowsAcrossTenants still sees both", func(t *testing.T) {
		armReadyWorkflow(t, storeA, unscopedTenantA, "claim-a-xt", "")
		armReadyWorkflow(t, storeB, unscopedTenantB, "claim-b-xt", "")

		got, err := storeB.ClaimWorkflowsAcrossTenants(ctx, "worker-xt", 10)
		if err != nil {
			t.Fatalf("ClaimWorkflowsAcrossTenants: %v", err)
		}
		for _, tenant := range []struct {
			name string
			id   string
		}{{"A", unscopedTenantA}, {"B", unscopedTenantB}} {
			if !containsTenant(got, tenant.id) {
				t.Errorf("the cross-tenant claim no longer sees tenant %s: claimed %v -- "+
					"scoping the ordinary claim must not scope this one",
					tenant.name, claimedWorkflowIDs(got))
			}
		}
	})
}
