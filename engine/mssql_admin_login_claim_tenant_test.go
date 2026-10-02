package engine

// IMPROVEMENT-PLAN 3.91. The ORDINARY claim path -- not the one whose name says
// "AcrossTenants" -- claimed every tenant's ready work on SQL Server.
//
// This is 3.86's mechanism reaching the statement that matters most, and it was
// missed by three hand audits and by the substring audit script that
// mssql_tenant_predicate_test.go has since replaced: it looked for `tenant_id`
// anywhere in the statement and found it, because the claim
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
//   SQL Server  no Go predicate AND no enforcement, once dbo.fn_tenant_filter
//               has been switched to the form that admits any dbo.cleat_admin
//               login (migrations/mssql/optional/cross_tenant_claim.sql --
//               the shipped predicate, migrations/mssql/003_procedures.sql,
//               has no such admission since cleat#1541).
//
// So SQL Server was the only dialect with nothing enforcing it, and the fix is
// to match MySQL rather than to make a judgement call.
//
// WHY THE GRANT WAS ALWAYS PRESENT WHERE IT MATTERED, AT THE TIME. This
// paragraph describes 3.91-era code: requireCleatAdminMembership,
// ClaimWorkflowsAcrossTenants and the --claim-strategy=global mechanism
// it names were all removed in #1926, which replaced the widened claim with
// unconditional per-tenant rotation. What the removal did not change is that
// any pool holding dbo.cleat_admin membership -- granted today for cleatctl
// or for cross-tenant test teardown, see engine/testutil/mssql_admin.go --
// shares that membership across every statement the pool issues, WithTenant
// included. That is the property this test still exercises.
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
// plainly belonged to tenant A. Every comparison here is case-insensitive.
//
// The same SHAPE appears in cmd/cleat-worker/setup.go:storeForTenant, which
// compares tenant strings with == -- but that one is NOT live, and the reason is
// worth writing down rather than re-deriving. The value it compares against is
// w.storeTenantID, which is always the all-zeros UUID (cmd/cleat-worker/main.go
// sets it from a constant, and no flag overrides that). The all-zeros UUID
// contains no letters, so its case cannot vary: `tenantID == w.storeTenantID` is
// true for the default tenant and false for every other, whichever case the
// projection produced. It is safe by the shape of the constant rather than by
// design, which is a different thing -- cleat#2983.

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
