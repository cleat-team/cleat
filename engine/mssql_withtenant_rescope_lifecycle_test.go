package engine

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestMSSQLStore_WithTenantRescopedStoreReadsTheLifecycleMethods extends
// TestMSSQLStore_WithTenantRescopedStoreReadsTheNewTenantsData's coverage
// (mssql_withtenant_rescope_test.go) to the 3 mssql_lifecycle.go methods
// converted in this PR -- cleat#2210. Same mechanism, same proof shape: open
// a store for tenant A, re-scope it to tenant B with WithTenant, and check
// each converted method reads/writes B's own data, not an empty result
// produced by the pool's original tenant-A SESSION_CONTEXT.
func TestMSSQLStore_WithTenantRescopedStoreReadsTheLifecycleMethods(t *testing.T) {
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

	defName := "wt-resc-life-def-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		VALUES (@p1, 1, 0x0061736d, 1, 1, @p2)`,
		defName, tenantB); err != nil {
		t.Fatalf("seed workflow_def: %v", err)
	}

	// runnableWfID: ready, no task_queue routing complications, for
	// CountRunnableWorkflows.
	runnableWfID := "wt-resc-life-runnable-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, @p2, 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3)`,
		runnableWfID, defName, tenantB); err != nil {
		t.Fatalf("seed runnable workflow_instance: %v", err)
	}

	// deadLetteredWfID: dead_lettered, for RetryWorkflow.
	deadLetteredWfID := "wt-resc-life-dl-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id,
		                                 assigned_to, heartbeat_at, error_msg, error_code, error_op)
		VALUES (@p1, @p2, 1, 'dead_lettered', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3,
		        'some-worker', SYSUTCDATETIME(), 'boom', 'E1', 'call')`,
		deadLetteredWfID, defName, tenantB); err != nil {
		t.Fatalf("seed dead_lettered workflow_instance: %v", err)
	}

	// parentWfID + childWfID: a TERMINATE-policy child still running, for
	// childrenClosedByTerminate.
	parentWfID := "wt-resc-life-parent-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, @p2, 1, 'running', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3)`,
		parentWfID, defName, tenantB); err != nil {
		t.Fatalf("seed parent workflow_instance: %v", err)
	}
	childWfID := "wt-resc-life-child-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id,
		                                 parent_workflow_id, parent_close_policy)
		VALUES (@p1, @p2, 1, 'running', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3, @p4, 'TERMINATE')`,
		childWfID, defName, tenantB, parentWfID); err != nil {
		t.Fatalf("seed child workflow_instance: %v", err)
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
	rescoped.taskQueues = []string{"default"}

	t.Run("CountRunnableWorkflows", func(t *testing.T) {
		n, err := rescoped.CountRunnableWorkflows(ctx)
		if err != nil {
			t.Fatalf("CountRunnableWorkflows: %v", err)
		}
		if n < 1 {
			t.Fatalf("CountRunnableWorkflows on a store WithTenant-rescoped from %s to %s = %d, want >= 1 "+
				"-- cleat#2210: a bare s.db read runs under the pool's original tenant's SESSION_CONTEXT, "+
				"not s.tenantID, so it silently reports the new tenant as having no runnable work",
				tenantA, tenantB, n)
		}
	})

	t.Run("RetryWorkflow", func(t *testing.T) {
		if err := rescoped.RetryWorkflow(ctx, deadLetteredWfID); err != nil {
			t.Fatalf("RetryWorkflow: %v", err)
		}
		var status string
		if err := mssqlQueryRowAsTenant(ctx, t, adminDB, tenantB,
			`SELECT status FROM workflow_instances WHERE id = @p1`, deadLetteredWfID,
		).Scan(&status); err != nil {
			t.Fatalf("verify status: %v", err)
		}
		if status != "ready" {
			t.Fatalf("RetryWorkflow on a store WithTenant-rescoped from %s to %s left status = %q, want "+
				"\"ready\" -- cleat#2210: under the bug this UPDATE's WHERE tenant_id = @p2 (now B) conjoins "+
				"with the RLS filter predicate (still A) to a contradiction and silently updates 0 rows",
				tenantA, tenantB, status)
		}
	})

	t.Run("childrenClosedByTerminate", func(t *testing.T) {
		ids, err := rescoped.childrenClosedByTerminate(ctx, parentWfID)
		if err != nil {
			t.Fatalf("childrenClosedByTerminate: %v", err)
		}
		for _, id := range ids {
			if id == childWfID {
				return
			}
		}
		t.Fatalf("childrenClosedByTerminate on a store WithTenant-rescoped from %s to %s did not return %q "+
			"(got %v) -- cleat#2210: a bare s.db read runs under the pool's original tenant's "+
			"SESSION_CONTEXT, not s.tenantID", tenantA, tenantB, childWfID, ids)
	})
}
