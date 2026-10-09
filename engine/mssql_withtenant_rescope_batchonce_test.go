package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestMSSQLStore_WithTenantRescopedStoreSweepsTheNewTenantsStaleWorkflows
// extends TestMSSQLStore_WithTenantRescopedStoreReadsTheNewTenantsData's
// coverage (mssql_withtenant_rescope_test.go) to deleteWorkflowsBatchOnce --
// the last method cleat#2210 left deferred, because its no-transaction SELECT
// is deliberate (RCSI, cleat#2060/#2139's carefully-tuned lock-escalation
// avoidance) and needed its own empirical verification before converting,
// not a mechanical batch alongside the other 23. See the PR for the trial
// count (120: 30 each of DeleteCompletedWorkflows/DeleteDeadLetteredWorkflows
// under both RCSI_ON and RCSI_OFF, against
// TestMSSQLRetentionSweepsCauseNoLockEscalation) and the falsification that
// confirmed that test still catches an escalating chunk size on this code.
//
// Same mechanism, same proof shape as the rest of cleat#2210: open a store
// for tenant A, re-scope it to tenant B with WithTenant, and check the sweep
// deletes B's own stale workflow, not an empty result produced by the pool's
// original tenant-A SESSION_CONTEXT.
func TestMSSQLStore_WithTenantRescopedStoreSweepsTheNewTenantsStaleWorkflows(t *testing.T) {
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}

	ctx := context.Background()
	adminDB := testutil.MSSQLTestDB(t)
	t.Cleanup(func() { adminDB.Close() })
	testutil.SetupMSSQLFullSchema(t, adminDB)
	testutil.CleanupMSSQLTestData(t, adminDB)
	t.Cleanup(func() { testutil.CleanupMSSQLTestData(t, adminDB) })

	const (
		tenantA = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
		tenantB = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
	)
	run := uuid.New().String()[:8]
	olderThan := time.Now().Add(-24 * time.Hour)
	completedAt := time.Now().Add(-48 * time.Hour)

	defName := "wt-resc-batchonce-def-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		VALUES (@p1, 1, 0x0061736d, 1, 1, @p2)`,
		defName, tenantB); err != nil {
		t.Fatalf("seed workflow_def: %v", err)
	}

	completedWfID := "wt-resc-batchonce-completed-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, completed_at, tenant_id)
		VALUES (@p1, @p2, 1, 'done', @p3, @p4)`,
		completedWfID, defName, completedAt, tenantB); err != nil {
		t.Fatalf("seed completed workflow_instance: %v", err)
	}
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, tenant_id)
		VALUES (@p1, 0, 'call', 'svc', 'op', @p2)`,
		completedWfID, tenantB); err != nil {
		t.Fatalf("seed completed workflow's event_history: %v", err)
	}

	deadLetteredWfID := "wt-resc-batchonce-deadletter-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, completed_at, tenant_id)
		VALUES (@p1, @p2, 1, 'dead_lettered', @p3, @p4)`,
		deadLetteredWfID, defName, completedAt, tenantB); err != nil {
		t.Fatalf("seed dead_lettered workflow_instance: %v", err)
	}
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, tenant_id)
		VALUES (@p1, 0, 'call', 'svc', 'op', @p2)`,
		deadLetteredWfID, tenantB); err != nil {
		t.Fatalf("seed dead-lettered workflow's event_history: %v", err)
	}

	// compactWfID: running (compaction can happen on a live workflow, not
	// only a terminal one), generation defaults to 0, for compactHistoryOnce
	// -- found alongside deleteWorkflowsBatchOnce by the same audit (the
	// other method using bare s.db.BeginTx with no tenant_id predicate of
	// its own).
	compactWfID := "wt-resc-batchonce-compact-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, tenant_id)
		VALUES (@p1, @p2, 1, 'running', @p3)`,
		compactWfID, defName, tenantB); err != nil {
		t.Fatalf("seed compact workflow_instance: %v", err)
	}
	for step := 0; step < 3; step++ {
		if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
			INSERT INTO event_history (workflow_id, step, event_type, service, operation, tenant_id)
			VALUES (@p1, @p2, 'call', 'svc', 'op', @p3)`,
			compactWfID, step, tenantB); err != nil {
			t.Fatalf("seed compact workflow's event_history step %d: %v", step, err)
		}
	}

	enableMSSQLTenantPolicies(t, adminDB)

	factory := NewMSSQLStoreFactory(dsn)
	defer factory.Close()

	// Opening for A, not B, is the point: it is A's SESSION_CONTEXT this pool
	// carries. WithTenant below re-scopes only the Go struct.
	opened, closerA, err := factory.OpenStore(ctx, tenantA)
	if err != nil {
		t.Fatalf("OpenStore(A): %v", err)
	}
	defer closerA.Close()
	storeA, ok := opened.(*MSSQLStore)
	if !ok {
		t.Fatalf("OpenStore returned %T, want *MSSQLStore", opened)
	}
	rescoped := storeA.WithTenant(tenantB)

	t.Run("DeleteCompletedWorkflows", func(t *testing.T) {
		n, err := rescoped.DeleteCompletedWorkflows(ctx, olderThan)
		if err != nil {
			t.Fatalf("DeleteCompletedWorkflows: %v", err)
		}
		if n < 1 {
			t.Fatalf("DeleteCompletedWorkflows on a store WithTenant-rescoped from %s to %s deleted %d rows, "+
				"want >= 1 -- cleat#2210: deleteWorkflowsBatchOnce's SELECT ran under the pool's original "+
				"tenant's SESSION_CONTEXT, not s.tenantID, so the RLS filter predicate excluded every one "+
				"of the new tenant's own stale rows and the sweep silently swept nothing", tenantA, tenantB, n)
		}
		var count int
		if err := mssqlQueryRowAsTenant(ctx, t, adminDB, tenantB,
			`SELECT COUNT(*) FROM workflow_instances WHERE id = @p1`, completedWfID,
		).Scan(&count); err != nil {
			t.Fatalf("verify deletion: %v", err)
		}
		if count != 0 {
			t.Fatalf("completed workflow %q still exists after DeleteCompletedWorkflows on the rescoped store", completedWfID)
		}
		// Also the event_history row: deleteEventHistoryChunkOnceOnce is a
		// SEPARATE bare-s.db transaction from the one that deletes
		// workflow_instances, so a falsification that only checked the
		// parent row passed green with this half of the fix reverted --
		// the workflow_instances delete alone was enough to satisfy the
		// assertions above. This is the other half.
		var eventCount int
		if err := mssqlQueryRowAsTenant(ctx, t, adminDB, tenantB,
			`SELECT COUNT(*) FROM event_history WHERE workflow_id = @p1`, completedWfID,
		).Scan(&eventCount); err != nil {
			t.Fatalf("verify event_history deletion: %v", err)
		}
		if eventCount != 0 {
			t.Fatalf("completed workflow %q's event_history rows still exist after DeleteCompletedWorkflows "+
				"on the rescoped store -- deleteEventHistoryChunkOnceOnce ran under the wrong tenant's "+
				"SESSION_CONTEXT", completedWfID)
		}
	})

	t.Run("DeleteDeadLetteredWorkflows", func(t *testing.T) {
		n, err := rescoped.DeleteDeadLetteredWorkflows(ctx, olderThan)
		if err != nil {
			t.Fatalf("DeleteDeadLetteredWorkflows: %v", err)
		}
		if n < 1 {
			t.Fatalf("DeleteDeadLetteredWorkflows on a store WithTenant-rescoped from %s to %s deleted %d rows, "+
				"want >= 1 -- same cleat#2210 mechanism as DeleteCompletedWorkflows above", tenantA, tenantB, n)
		}
		var count int
		if err := mssqlQueryRowAsTenant(ctx, t, adminDB, tenantB,
			`SELECT COUNT(*) FROM workflow_instances WHERE id = @p1`, deadLetteredWfID,
		).Scan(&count); err != nil {
			t.Fatalf("verify deletion: %v", err)
		}
		if count != 0 {
			t.Fatalf("dead-lettered workflow %q still exists after DeleteDeadLetteredWorkflows on the rescoped store", deadLetteredWfID)
		}
		var eventCount int
		if err := mssqlQueryRowAsTenant(ctx, t, adminDB, tenantB,
			`SELECT COUNT(*) FROM event_history WHERE workflow_id = @p1`, deadLetteredWfID,
		).Scan(&eventCount); err != nil {
			t.Fatalf("verify event_history deletion: %v", err)
		}
		if eventCount != 0 {
			t.Fatalf("dead-lettered workflow %q's event_history rows still exist after DeleteDeadLetteredWorkflows "+
				"on the rescoped store -- deleteEventHistoryChunkOnceOnce ran under the wrong tenant's "+
				"SESSION_CONTEXT", deadLetteredWfID)
		}
	})

	t.Run("CompactHistory", func(t *testing.T) {
		cs := CompactionState{Version: 1, CompactedStep: 1, Events: []CompactedEvent{}}
		csJSON, err := json.Marshal(cs)
		if err != nil {
			t.Fatalf("json.Marshal CompactionState: %v", err)
		}
		if err := rescoped.CompactHistory(ctx, compactWfID, csJSON, 1, 2); err != nil {
			t.Fatalf("CompactHistory on a store WithTenant-rescoped from %s to %s: %v -- cleat#2210: "+
				"compactHistoryOnce's generation read ran under the pool's original tenant's SESSION_CONTEXT, "+
				"not s.tenantID, so the RLS filter predicate excluded the new tenant's own row entirely and "+
				"this read ErrNoRows, which compactHistoryOnce treats as \"workflow no longer exists\" and "+
				"silently reports success", tenantA, tenantB, err)
		}
		// Step 0 and 1 should be gone (step < keepStep=2); step 2 survives.
		var remaining int
		if err := mssqlQueryRowAsTenant(ctx, t, adminDB, tenantB,
			`SELECT COUNT(*) FROM event_history WHERE workflow_id = @p1`, compactWfID,
		).Scan(&remaining); err != nil {
			t.Fatalf("count remaining event_history: %v", err)
		}
		if remaining != 1 {
			t.Fatalf("CompactHistory on the rescoped store left %d event_history rows for %q, want 1 "+
				"(only step 2 should survive keepStep=2) -- the delete silently affected 0 rows under the "+
				"wrong tenant's SESSION_CONTEXT", remaining, compactWfID)
		}
		var state sql.NullString
		if err := mssqlQueryRowAsTenant(ctx, t, adminDB, tenantB,
			`SELECT CAST(compaction_state AS NVARCHAR(MAX)) FROM workflow_instances WHERE id = @p1`, compactWfID,
		).Scan(&state); err != nil {
			t.Fatalf("read compaction_state: %v", err)
		}
		if !state.Valid || state.String == "" {
			t.Fatalf("CompactHistory on the rescoped store left compaction_state unset for %q -- the UPDATE "+
				"silently affected 0 rows under the wrong tenant's SESSION_CONTEXT", compactWfID)
		}
	})
}
