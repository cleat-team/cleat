package engine

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestMSSQLStore_WithTenantRescopedStoreReadsTheEventMethods extends
// TestMSSQLStore_WithTenantRescopedStoreReadsTheNewTenantsData's coverage
// (mssql_withtenant_rescope_test.go) to the 5 mssql_events.go methods
// converted in this PR -- cleat#2210. Same mechanism, same proof shape: open
// a store for tenant A, re-scope it to tenant B with WithTenant, and check
// each converted method reads B's own data, not an empty result produced by
// the pool's original tenant-A SESSION_CONTEXT.
func TestMSSQLStore_WithTenantRescopedStoreReadsTheEventMethods(t *testing.T) {
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

	// defName/wfID: the shared fixture. event_history has an FK to
	// workflow_instances, so a workflow row must exist first.
	defName := "wt-resc-evt-def-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		VALUES (@p1, 1, 0x0061736d, 1, 1, @p2)`,
		defName, tenantB); err != nil {
		t.Fatalf("seed workflow_def: %v", err)
	}
	wfID := "wt-resc-evt-wf-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id, history_swept_at)
		VALUES (@p1, @p2, 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3, SYSUTCDATETIME())`,
		wfID, defName, tenantB); err != nil {
		t.Fatalf("seed workflow_instance: %v", err)
	}

	// Three events, steps 0-2, enough to exercise LoadEventHistory's
	// ordering, LoadEventHistoryPaginated's OFFSET/FETCH, and
	// StreamEventHistory's paging (pageSize=1 below forces 3 pages).
	const numEvents = 3
	for step := 0; step < numEvents; step++ {
		if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
			INSERT INTO event_history (workflow_id, step, event_type, service, operation, tenant_id)
			VALUES (@p1, @p2, 'call', 'svc', 'op', @p3)`,
			wfID, step, tenantB); err != nil {
			t.Fatalf("seed event_history step %d: %v", step, err)
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

	t.Run("LoadEventHistory", func(t *testing.T) {
		history, err := rescoped.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		if len(history) != numEvents {
			t.Fatalf("LoadEventHistory on a store WithTenant-rescoped from %s to %s returned %d event(s), "+
				"want %d -- cleat#2210: a bare s.db read runs under the pool's original tenant's "+
				"SESSION_CONTEXT, not s.tenantID, so it silently reports the new tenant as having no history",
				tenantA, tenantB, len(history), numEvents)
		}
	})

	t.Run("LoadEventHistoryPaginated", func(t *testing.T) {
		page, err := rescoped.LoadEventHistoryPaginated(ctx, wfID, 0, 1000)
		if err != nil {
			t.Fatalf("LoadEventHistoryPaginated: %v", err)
		}
		if len(page) != numEvents {
			t.Fatalf("LoadEventHistoryPaginated on a store WithTenant-rescoped from %s to %s returned %d "+
				"event(s), want %d", tenantA, tenantB, len(page), numEvents)
		}
	})

	t.Run("CountEventHistory", func(t *testing.T) {
		count, err := rescoped.CountEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("CountEventHistory: %v", err)
		}
		if count != numEvents {
			t.Fatalf("CountEventHistory on a store WithTenant-rescoped from %s to %s = %d, want %d",
				tenantA, tenantB, count, numEvents)
		}
	})

	t.Run("IsHistorySwept", func(t *testing.T) {
		swept, err := rescoped.IsHistorySwept(ctx, wfID)
		if err != nil {
			t.Fatalf("IsHistorySwept: %v", err)
		}
		if !swept {
			t.Fatalf("IsHistorySwept on a store WithTenant-rescoped from %s to %s = false, want true "+
				"(history_swept_at was seeded non-NULL) -- cleat#2210: under the bug this misses the row "+
				"(sql.ErrNoRows) and IsHistorySwept's own no-rows branch reports false, silently hiding "+
				"that the row -- and the sweep -- exist",
				tenantA, tenantB)
		}
	})

	t.Run("StreamEventHistory", func(t *testing.T) {
		eventCh, errCh := rescoped.StreamEventHistory(ctx, wfID, 1) // pageSize=1: forces 3 pages
		var got int
		for range eventCh {
			got++
		}
		if err := <-errCh; err != nil {
			t.Fatalf("StreamEventHistory: %v", err)
		}
		if got != numEvents {
			t.Fatalf("StreamEventHistory on a store WithTenant-rescoped from %s to %s streamed %d "+
				"event(s), want %d -- cleat#2210: a bare s.db read per page runs under the pool's "+
				"original tenant's SESSION_CONTEXT, not s.tenantID", tenantA, tenantB, got, numEvents)
		}
	})
}
