package engine

// Regression coverage for DeleteCompletedWorkflows (engine/db.go), Finding
// S2: nothing else in this store ever deletes a workflow_instances row for a
// terminal ('done'/'failed'/'terminated') workflow, so the table grows
// forever, bounded by lifetime workflow count rather than active count.
//
// This mirrors dead_lettered_workflows_rls_test.go's two checks, which this
// stream read before writing anything (per its own instructions) because
// they are the same question for the same table:
//
//  1. workflow_instances carries FORCE ROW LEVEL SECURITY with a fail-closed
//     policy (cleat.assert_tenant_set()). A plain-pool DELETE issued by a
//     role that is neither a superuser nor the table owner -- the shape
//     cleat_app has in production -- does not silently affect zero rows: it
//     raises "cleat.tenant_id is not set". DeleteCompletedWorkflows must run
//     inside beginTxWithRLS to work under that connection shape.
//  2. event_history has no FK back to workflow_instances on PostgreSQL --
//     migrations/postgres/003_procedures.sql drops it deliberately, because
//     finalize_workflow_status() deletes a 'done'/'failed' workflow's events
//     itself. But TerminateWorkflow (the path to 'terminated') does not call
//     finalize_workflow_status, so a force-terminated workflow's events are
//     not deleted by any other path either. If DeleteCompletedWorkflows only
//     deleted the workflow_instances row, those event_history rows would be
//     orphaned permanently -- nothing else ever looks at them again.
//
// Per CLAUDE.md: every count below is taken from adminDB, a superuser/bypass
// connection, not from the RLS-scoped appDB connection under test -- an RLS
// policy hiding another tenant's rows from a query would make a sloppier
// version of this test pass for the wrong reason (this is exactly how
// TestCascadeDelete was burned: two of its three child-row assertions
// "passed" because the policy hid the rows, not because they were deleted).
import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestDeleteCompletedWorkflows_RLSEnforcedAndEventHistoryNotOrphaned(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	const tenantA = "d2000000-0000-4000-8000-00000000000a"
	const tenantB = "d2000000-0000-4000-8000-00000000000b"

	adminStore := NewPostgresStore(adminDB)
	const defName = "completed-rls-def"
	if err := adminStore.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	// seedTerminated creates one workflow for the given tenant (via the
	// superuser adminDB connection -- fixture setup, not the thing under
	// test), claims it, force-terminates it via TerminateWorkflow (the one
	// terminal path that does NOT go through finalize_workflow_status, so it
	// is the sharpest case for bug (2) above), and attaches one event_history
	// row directly, standing in for events the workflow generated before it
	// was killed. Returns the workflow ID.
	seedTerminated := func(t *testing.T, tenant, tag string) string {
		t.Helper()
		wfID := fmt.Sprintf("completed-rls-%s-%d", tag, time.Now().UnixNano())
		tenantStore := NewPostgresStore(adminDB).WithTenant(tenant)
		if _, _, err := tenantStore.StartNewRun(ctx, wfID, defName, 1, json.RawMessage(`{}`), "", tenant, 0); err != nil {
			t.Fatalf("StartNewRun(%s): %v", tenant, err)
		}
		if err := tenantStore.TerminateWorkflow(ctx, wfID, "force kill"); err != nil {
			t.Fatalf("TerminateWorkflow(%s): %v", tenant, err)
		}
		if _, err := adminDB.ExecContext(ctx, `
			INSERT INTO event_history (workflow_id, step, tenant_id, event_type)
			VALUES ($1, 0, $2, 'call')
		`, wfID, tenant); err != nil {
			t.Fatalf("seed event_history for %s: %v", tenant, err)
		}
		return wfID
	}

	wfA := seedTerminated(t, tenantA, "a")
	wfB := seedTerminated(t, tenantB, "b")

	// idColumn maps table -> the column that holds the workflow ID.
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
	// testutil.PostgresRLSTestRole, neither a superuser nor the owner of
	// these tables, so it is genuinely subject to FORCE ROW LEVEL SECURITY --
	// the same shape cleat_app has in production. adminDB above bypasses RLS
	// unconditionally and would hide bug (1).
	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()
	assertNotSuperuserBypass(t, appDB)

	appStoreA := NewPostgresStore(appDB).WithTenant(tenantA)
	deleted, err := appStoreA.DeleteCompletedWorkflows(ctx, time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("DeleteCompletedWorkflows under real RLS enforcement returned an error -- this is "+
			"exactly bug (1): a DELETE issued with no RLS context set is rejected outright by "+
			"workflow_instances' fail-closed policy: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("DeleteCompletedWorkflows returned %d, want 1 (tenant A's one terminated workflow)", deleted)
	}

	// Bug (2) check -- the orphan case. Count from adminDB (bypass
	// connection), not appDB: event_history's tenant_isolation_events policy
	// would make an orphaned row invisible to a tenant-scoped query even if
	// it were never deleted at all, which is exactly the "policy hid the
	// rows" failure mode CLAUDE.md calls out.
	if n := countRows(t, "workflow_instances", wfA); n != 0 {
		t.Errorf("tenant A's workflow_instances row still present after DeleteCompletedWorkflows (count=%d)", n)
	}
	if n := countRows(t, "event_history", wfA); n != 0 {
		t.Errorf("tenant A's event_history row still present after DeleteCompletedWorkflows (count=%d) -- "+
			"this is bug (2): event_history has no FK/CASCADE back to workflow_instances on PostgreSQL, "+
			"so it must be deleted explicitly or it is orphaned forever the moment the workflow_instances "+
			"row is gone", n)
	}

	// Tenant B's rows must be untouched -- both counted from adminDB, which
	// RLS cannot have hidden anything from.
	if n := countRows(t, "workflow_instances", wfB); n != 1 {
		t.Errorf("tenant B's workflow_instances row was affected by tenant A's delete (count=%d, want 1)", n)
	}
	if n := countRows(t, "event_history", wfB); n != 1 {
		t.Errorf("tenant B's event_history row was affected by tenant A's delete (count=%d, want 1)", n)
	}

	// Global orphan check: no event_history row anywhere should reference a
	// workflow_id that no longer has a workflow_instances row. This is the
	// assertion the task calls out specifically -- broader than "tenant A's
	// one row", it is the general shape bug (2) takes if the explicit
	// event_history delete is ever lost in a future refactor.
	var orphanCount int
	if err := adminDB.QueryRowContext(ctx, `
		SELECT count(*) FROM event_history e
		WHERE NOT EXISTS (SELECT 1 FROM workflow_instances w WHERE w.id = e.workflow_id)
	`).Scan(&orphanCount); err != nil {
		t.Fatalf("count orphaned event_history rows: %v", err)
	}
	if orphanCount != 0 {
		t.Errorf("found %d orphaned event_history row(s) (workflow_id with no matching workflow_instances row) "+
			"after DeleteCompletedWorkflows", orphanCount)
	}
}
