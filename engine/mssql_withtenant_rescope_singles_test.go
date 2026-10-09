package engine

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestMSSQLStore_WithTenantRescopedStoreReadsTheRemainingSingleMethodBatch
// extends TestMSSQLStore_WithTenantRescopedStoreReadsTheNewTenantsData's
// coverage (mssql_withtenant_rescope_test.go) to the 6 single-method files
// converted in this PR -- cleat#2210's remaining standalone candidates. Same
// mechanism, same proof shape: open a store for tenant A, re-scope it to
// tenant B with WithTenant, and check each converted method reads/writes B's
// own data rather than an empty/no-op result produced by the pool's original
// tenant-A SESSION_CONTEXT.
func TestMSSQLStore_WithTenantRescopedStoreReadsTheRemainingSingleMethodBatch(t *testing.T) {
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

	// admin.tenants is never cleaned by CleanupMSSQLTestData (it's FK'd there
	// but not cleared), and dbo.tenant_settings FKs to it -- so this is
	// pre-cleaned rather than left to a prior run's leftovers, same pattern
	// as mssql_tenant_settings_test.go.
	if _, err := adminDB.ExecContext(ctx, `DELETE FROM dbo.tenant_settings WHERE tenant_id = @p1`, tenantB); err != nil {
		t.Fatalf("pre-clean tenant_settings: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx, `DELETE FROM admin.tenants WHERE tenant_id = @p1`, tenantB); err != nil {
		t.Fatalf("pre-clean admin.tenants: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO admin.tenants (tenant_id, name, display_name) VALUES (@p1, @p2, @p2)`,
		tenantB, "singles-batch-"+run); err != nil {
		t.Fatalf("seed admin.tenants: %v", err)
	}

	defName := "wt-resc-singles-def-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		VALUES (@p1, 1, 0x0061736d, 1, 1, @p2)`,
		defName, tenantB); err != nil {
		t.Fatalf("seed workflow_def: %v", err)
	}

	// cwID: carries a concurrency_keys row, for GetConcurrencyKeyHolder.
	cwID := "wt-resc-singles-cw-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, @p2, 1, 'running', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3)`,
		cwID, defName, tenantB); err != nil {
		t.Fatalf("seed cw workflow_instance: %v", err)
	}
	concurrencyKey := "wt-resc-singles-key-" + run
	keyHash := sha256.Sum256([]byte(concurrencyKey))
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
		VALUES (@p1, @p2, @p3, DATEADD(HOUR, 1, SYSUTCDATETIME()), @p4)`,
		keyHash[:], concurrencyKey, cwID, tenantB); err != nil {
		t.Fatalf("seed concurrency_keys: %v", err)
	}

	// runLimitsID: carries run_* limit overrides, for GetRunLimits.
	runLimitsID := "wt-resc-singles-runlim-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id,
		                                 run_wasm_wall_clock_ceiling_ms)
		VALUES (@p1, @p2, 1, 'running', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3, 3000)`,
		runLimitsID, defName, tenantB); err != nil {
		t.Fatalf("seed run-limits workflow_instance: %v", err)
	}

	// streamWfID: carries one plugin_call_stream_chunk event, for
	// LoadStreamChunksAfter.
	streamWfID := "wt-resc-singles-stream-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, @p2, 1, 'running', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3)`,
		streamWfID, defName, tenantB); err != nil {
		t.Fatalf("seed stream workflow_instance: %v", err)
	}
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO event_history (workflow_id, step, event_type, tenant_id)
		VALUES (@p1, 0, 'plugin_call_stream_chunk', @p2)`,
		streamWfID, tenantB); err != nil {
		t.Fatalf("seed stream chunk event: %v", err)
	}

	// parentRunID -> successorRunID: a ContinueAsNew chain, for successorOfRun.
	parentRunID := "wt-resc-singles-parent-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, @p2, 1, 'done', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3)`,
		parentRunID, defName, tenantB); err != nil {
		t.Fatalf("seed parent run: %v", err)
	}
	successorRunID := "wt-resc-singles-successor-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id,
		                                 continued_from)
		VALUES (@p1, @p2, 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3, @p4)`,
		successorRunID, defName, tenantB, parentRunID); err != nil {
		t.Fatalf("seed successor run: %v", err)
	}

	// dbo.tenant_settings row, for GetTenantSettings.
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO dbo.tenant_settings (tenant_id, wasm_wall_clock_ceiling_ms)
		VALUES (@p1, 5000)`, tenantB); err != nil {
		t.Fatalf("seed tenant_settings: %v", err)
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

	// Unlike the other 5 subtests below, this one does NOT regress if
	// beginTxWithContext is reverted: concurrency_keys carries no RLS policy
	// at all (grep `CREATE SECURITY POLICY` across migrations/mssql/ -- 14
	// policies, none naming this table), so the bare WHERE tenant_id = @p2
	// was already correctly scoped by s.tenantID regardless of which
	// tenant's SESSION_CONTEXT the connection carried. Measured directly:
	// reverting GetConcurrencyKeyHolder alone left this subtest passing.
	// Converted anyway for consistency with every other method in this
	// batch and as a no-cost precaution if RLS is ever added to this table
	// -- but the PR does not claim this one fixes a reachable bug.
	t.Run("GetConcurrencyKeyHolder", func(t *testing.T) {
		h, err := rescoped.GetConcurrencyKeyHolder(ctx, concurrencyKey)
		if err != nil {
			t.Fatalf("GetConcurrencyKeyHolder: %v", err)
		}
		if !h.Held || h.WorkflowID != cwID {
			t.Fatalf("GetConcurrencyKeyHolder on a store WithTenant-rescoped from %s to %s = %+v, want "+
				"Held=true WorkflowID=%q", tenantA, tenantB, h, cwID)
		}
	})

	t.Run("GetTenantSettings", func(t *testing.T) {
		settings, err := rescoped.GetTenantSettings(ctx)
		if err != nil {
			t.Fatalf("GetTenantSettings: %v", err)
		}
		if settings.WasmWallClockCeiling.Milliseconds() != 5000 {
			t.Fatalf("GetTenantSettings on a store WithTenant-rescoped from %s to %s returned "+
				"WasmWallClockCeiling=%v, want 5000ms -- cleat#2210: the stale doc comment reasoned "+
				"the pool's connector already sets SESSION_CONTEXT, which is true only for a store "+
				"never re-scoped via WithTenant", tenantA, tenantB, settings.WasmWallClockCeiling)
		}
	})

	t.Run("GetRunLimits", func(t *testing.T) {
		limits, err := rescoped.GetRunLimits(ctx, runLimitsID)
		if err != nil {
			t.Fatalf("GetRunLimits: %v", err)
		}
		if limits.WasmWallClockCeiling.Milliseconds() != 3000 {
			t.Fatalf("GetRunLimits on a store WithTenant-rescoped from %s to %s returned "+
				"WasmWallClockCeiling=%v, want 3000ms", tenantA, tenantB, limits.WasmWallClockCeiling)
		}
	})

	t.Run("LoadStreamChunksAfter", func(t *testing.T) {
		chunks, err := rescoped.LoadStreamChunksAfter(ctx, streamWfID, -1, 10)
		if err != nil {
			t.Fatalf("LoadStreamChunksAfter: %v", err)
		}
		if len(chunks) != 1 {
			t.Fatalf("LoadStreamChunksAfter on a store WithTenant-rescoped from %s to %s returned %d "+
				"chunk(s), want 1", tenantA, tenantB, len(chunks))
		}
	})

	t.Run("successorOfRun", func(t *testing.T) {
		id, err := rescoped.successorOfRun(ctx, parentRunID)
		if err != nil {
			t.Fatalf("successorOfRun: %v", err)
		}
		if id != successorRunID {
			t.Fatalf("successorOfRun on a store WithTenant-rescoped from %s to %s = %q, want %q",
				tenantA, tenantB, id, successorRunID)
		}
	})

	t.Run("CountWorkflows", func(t *testing.T) {
		n, err := rescoped.CountWorkflows(ctx, WorkflowFilter{})
		if err != nil {
			t.Fatalf("CountWorkflows: %v", err)
		}
		if n < 1 {
			t.Fatalf("CountWorkflows on a store WithTenant-rescoped from %s to %s = %d, want >= 1 "+
				"-- cleat#2210: a bare s.db read runs under the pool's original tenant's SESSION_CONTEXT, "+
				"not s.tenantID, so it silently reports the new tenant as having no workflows",
				tenantA, tenantB, n)
		}
	})
}
