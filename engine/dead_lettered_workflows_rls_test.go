package engine

// Regression coverage for two bugs found in DeleteDeadLetteredWorkflows
// (engine/db.go) while working Stream I / Finding S3 (tenant deletion):
// its FK-graph question -- "which child tables actually cascade when a
// workflow_instances row is deleted" -- is the same question tenant
// deletion has to answer for the same table.
//
//  1. The old implementation ran its DELETE on s.db directly, the plain
//     pool, with no RLS context set. workflow_instances carries FORCE ROW
//     LEVEL SECURITY with a fail-closed policy, so under a genuine
//     RLS-enforcing connection (any role that is neither a superuser nor
//     the table owner -- the shape cleat_app has in production) the old
//     query did not silently do nothing: it raised
//     "cleat.tenant_id is not set" and the call errored outright.
//  2. event_history has no foreign key back to workflow_instances --
//     migrations/postgres/003_procedures.sql drops it deliberately, because
//     finalize_workflow_status() deletes a workflow's events itself on
//     'done'/'failed'. MoveToDeadLetterQueue does not go through
//     finalize_workflow_status, so a dead-lettered workflow's event_history
//     rows survive it, and the old doc comment's claim that they are
//     "automatically deleted via ON DELETE CASCADE" was false for this one
//     table. Once workflow_instances' row is gone, those event_history
//     rows are orphaned permanently -- nothing else ever looks at them
//     again.
//
// Per CLAUDE.md: this test counts rows from adminDB, a superuser/bypass
// connection, to confirm actual deletion rather than mere invisibility --
// an RLS policy hiding tenant B's rows from a query would make a sloppier
// version of this test pass for the wrong reason.
import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestDeleteDeadLetteredWorkflows_RLSEnforcedAndEventHistoryNotOrphaned(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	const tenantA = "d1000000-0000-4000-8000-00000000000a"
	const tenantB = "d1000000-0000-4000-8000-00000000000b"

	adminStore := NewPostgresStore(adminDB)
	const defName = "dlq-rls-def"
	if err := adminStore.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	// seedDeadLettered creates one workflow for the given tenant (via the
	// superuser adminDB connection -- fixture setup, not the thing under
	// test), claims it, moves it to dead_lettered, and attaches one
	// event_history row directly, standing in for events the workflow
	// generated before it died. Returns the workflow ID.
	seedDeadLettered := func(t *testing.T, tenant, tag string) string {
		t.Helper()
		wfID := fmt.Sprintf("dlq-rls-%s-%d", tag, time.Now().UnixNano())
		tenantStore := NewPostgresStore(adminDB).WithTenant(tenant)
		if _, _, err := tenantStore.StartNewRun(ctx, wfID, defName, 1, json.RawMessage(`{}`), "", tenant, 0); err != nil {
			t.Fatalf("StartNewRun(%s): %v", tenant, err)
		}
		wf, err := tenantStore.ClaimWorkflow(ctx, "worker-"+tag)
		if err != nil || wf == nil {
			t.Fatalf("ClaimWorkflow(%s): wf=%v err=%v", tenant, wf, err)
		}
		if err := tenantStore.MoveToDeadLetterQueue(ctx, wf.ID, "worker-"+tag, wf.Generation, "boom", "E_BOOM", "run"); err != nil {
			t.Fatalf("MoveToDeadLetterQueue(%s): %v", tenant, err)
		}
		// completed_at is set to now() by MoveToDeadLetterQueue; a future
		// cutoff below is what makes DeleteDeadLetteredWorkflows treat it
		// as eligible, matching the pattern store_test_groups_21_25_test.go's
		// TestDeleteDeadLetteredWorkflows already uses.
		if _, err := adminDB.ExecContext(ctx, `
			INSERT INTO event_history (workflow_id, step, tenant_id, event_type)
			VALUES ($1, 0, $2, 'call')
		`, wf.ID, tenant); err != nil {
			t.Fatalf("seed event_history for %s: %v", tenant, err)
		}
		return wf.ID
	}

	wfA := seedDeadLettered(t, tenantA, "a")
	wfB := seedDeadLettered(t, tenantB, "b")

	// idColumn maps table -> the column that holds the workflow ID.
	// workflow_instances' primary key is "id"; event_history's is
	// "workflow_id".
	idColumn := map[string]string{
		"workflow_instances": "id",
		"event_history":      "workflow_id",
	}
	countRows := func(t *testing.T, table, workflowID string) int {
		t.Helper()
		var n int
		if err := adminDB.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s = $1`, table, idColumn[table]), workflowID,
		).Scan(&n); err != nil {
			t.Fatalf("count %s for %s: %v", table, workflowID, err)
		}
		return n
	}

	if countRows(t, "workflow_instances", wfA) != 1 || countRows(t, "event_history", wfA) != 1 {
		t.Fatalf("fixture setup: tenant A's workflow/event not present before delete")
	}
	if countRows(t, "workflow_instances", wfB) != 1 || countRows(t, "event_history", wfB) != 1 {
		t.Fatalf("fixture setup: tenant B's workflow/event not present before delete")
	}

	// The operation under test runs on appDB: a connection authenticated as
	// testutil.PostgresRLSTestRole, which is neither a superuser nor the
	// owner of these tables, so it is genuinely subject to
	// FORCE ROW LEVEL SECURITY -- the same shape cleat_app has in
	// production. This is the connection the old bug (1) only fails on;
	// adminDB above bypasses RLS unconditionally and would hide that bug.
	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()
	assertNotSuperuserBypass(t, appDB)

	appStoreA := NewPostgresStore(appDB).WithTenant(tenantA)
	deleted, err := appStoreA.DeleteDeadLetteredWorkflows(ctx, time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("DeleteDeadLetteredWorkflows under real RLS enforcement returned an error -- this is "+
			"exactly bug (1): the old implementation ran its DELETE on the plain pool with no RLS "+
			"context set, and workflow_instances' fail-closed policy rejects any statement issued "+
			"without cleat.tenant_id set: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("DeleteDeadLetteredWorkflows returned %d, want 1 (tenant A's one dead-lettered workflow)", deleted)
	}

	// Bug (2) check: count from adminDB (bypass connection), not appDB.
	// event_history's tenant_isolation_events policy would make an orphaned
	// row invisible to a tenant-scoped query even if it were never deleted
	// -- exactly the "policy hid the rows" failure mode CLAUDE.md calls
	// out, so this must be counted from a connection RLS cannot filter.
	if n := countRows(t, "workflow_instances", wfA); n != 0 {
		t.Errorf("tenant A's workflow_instances row still present after DeleteDeadLetteredWorkflows (count=%d)", n)
	}
	if n := countRows(t, "event_history", wfA); n != 0 {
		t.Errorf("tenant A's event_history row still present after DeleteDeadLetteredWorkflows (count=%d) -- "+
			"this is bug (2): event_history has no FK/CASCADE back to workflow_instances "+
			"(migrations/postgres/003_procedures.sql drops it), so it must be deleted explicitly "+
			"or it is orphaned forever the moment the workflow_instances row is gone", n)
	}

	// Tenant B's rows must be untouched -- both present in adminDB's count,
	// which RLS cannot have hidden anything from.
	if n := countRows(t, "workflow_instances", wfB); n != 1 {
		t.Errorf("tenant B's workflow_instances row was affected by tenant A's delete (count=%d, want 1)", n)
	}
	if n := countRows(t, "event_history", wfB); n != 1 {
		t.Errorf("tenant B's event_history row was affected by tenant A's delete (count=%d, want 1)", n)
	}
}
