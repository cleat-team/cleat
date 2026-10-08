package engine

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestMSSQLStore_WithTenantRescopedStoreReadsTheNewTenantsData is the
// behavioural half of cleat#2210 that scripts/check-mssql-tenant-scoped-reads.py
// cannot provide: the guard is structural (it flags a method's SOURCE shape),
// this proves the shape it now requires actually behaves correctly against a
// real, RLS-enforcing SQL Server.
//
// MSSQLStoreFactory.OpenStore bakes tenant A's SESSION_CONTEXT into every
// physical connection in A's pool at connect time (tenantSessionConnector).
// WithTenant only copies the Go struct's tenantID field -- it shares that same
// pool. So a method that queries s.db directly, with no transaction of its
// own, runs under whichever tenant's SESSION_CONTEXT the borrowed connection
// happens to carry, not s.tenantID: the Go-level "WHERE tenant_id = @p1" (now
// B) and the RLS filter predicate "tenant_id = SESSION_CONTEXT('tenant_id')"
// (still A) conjoin to a contradiction, and the read silently comes back
// empty -- not an error, and not another tenant's data, but B's own row
// reported as absent.
//
// beginTxWithContext closes that gap by setting SESSION_CONTEXT explicitly to
// s.tenantID on every transaction it opens, regardless of which pooled
// connection was handed out. Both methods below were converted to it in this
// PR; this test re-scopes a live tenant-A store to tenant B with WithTenant
// and checks each one reads B's row, not an empty result.
func TestMSSQLStore_WithTenantRescopedStoreReadsTheNewTenantsData(t *testing.T) {
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

	// Everything below belongs to B alone, seeded on the admin connection
	// (no tenant context yet) before the policies go live.
	schedName := "wt-resc-sched-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_schedules (name, def_name, cron_expression, tenant_id)
		VALUES (@p1, @p2, '* * * * *', @p3)`,
		schedName, "wt-resc-def-"+run, tenantB); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	defName := "wt-resc-wfdef-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		VALUES (@p1, 1, 0x0061736d, 1, 1, @p2)`, defName, tenantB); err != nil {
		t.Fatalf("seed workflow_def: %v", err)
	}
	wfID := "wt-resc-wf-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, @p2, 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3)`,
		wfID, defName, tenantB); err != nil {
		t.Fatalf("seed workflow_instance: %v", err)
	}
	promiseID := "wt-resc-promise-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_promises (workflow_id, promise_id, promise_name, tenant_id, status, result)
		VALUES (@p1, @p2, 'done', @p3, 'resolved', '{"answer":"the-answer"}')`,
		wfID, promiseID, tenantB); err != nil {
		t.Fatalf("seed promise: %v", err)
	}
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		UPDATE workflow_instances SET query_state = '{"progress":"42"}' WHERE id = @p1`,
		wfID); err != nil {
		t.Fatalf("seed query_state: %v", err)
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

	t.Run("ListSchedules", func(t *testing.T) {
		schedules, err := rescoped.ListSchedules(ctx)
		if err != nil {
			t.Fatalf("ListSchedules: %v", err)
		}
		for _, sch := range schedules {
			if sch.Name == schedName {
				return
			}
		}
		t.Fatalf("ListSchedules on a store WithTenant-rescoped from %s to %s did not return %q "+
			"(got %d schedule(s)) -- cleat#2210: a bare s.db read runs under the pool's original "+
			"tenant's SESSION_CONTEXT, not s.tenantID, so it silently reports the new tenant as "+
			"having none of its own rows", tenantA, tenantB, schedName, len(schedules))
	})

	t.Run("GetPromise", func(t *testing.T) {
		status, result, _, err := rescoped.GetPromise(ctx, wfID, promiseID)
		if err != nil {
			t.Fatalf("GetPromise: %v", err)
		}
		const wantResult = `{"answer":"the-answer"}`
		if status != "resolved" || result != wantResult {
			t.Fatalf("GetPromise on a store WithTenant-rescoped from %s to %s = (%q, %q), "+
				"want (\"resolved\", %q) -- cleat#2210: under the bug this misses the row "+
				"(sql.ErrNoRows) and GetPromise's own no-rows branch reports a resolved promise "+
				"as merely \"pending\", silent data loss rather than an error",
				tenantA, tenantB, status, result, wantResult)
		}
	})

	// mssql_operations.go's three methods, converted alongside this test's
	// extension (cleat#2210's "now unblocked by #2751" batch).
	t.Run("GetQueryState", func(t *testing.T) {
		got, err := rescoped.GetQueryState(ctx, wfID, "progress")
		if err != nil {
			t.Fatalf("GetQueryState: %v", err)
		}
		if got != "42" {
			t.Fatalf("GetQueryState on a store WithTenant-rescoped from %s to %s = %q, want %q -- "+
				"cleat#2210: under the bug this misses the row (sql.ErrNoRows) and GetQueryState's "+
				"own no-rows branch returns \"\", indistinguishable from a key that was never published",
				tenantA, tenantB, got, "42")
		}
	})

	t.Run("ListQueryState", func(t *testing.T) {
		got, err := rescoped.ListQueryState(ctx, wfID)
		if err != nil {
			t.Fatalf("ListQueryState: %v", err)
		}
		if got["progress"] != "42" {
			t.Fatalf("ListQueryState on a store WithTenant-rescoped from %s to %s = %v, want "+
				"progress=%q -- cleat#2210: under the bug this misses the row (sql.ErrNoRows) and "+
				"ListQueryState's own no-rows branch returns an empty map, indistinguishable from a "+
				"run that published nothing", tenantA, tenantB, got, "42")
		}
	})

	t.Run("QueueDepth", func(t *testing.T) {
		got, err := rescoped.QueueDepth(ctx)
		if err != nil {
			t.Fatalf("QueueDepth: %v", err)
		}
		if got < 1 {
			t.Fatalf("QueueDepth on a store WithTenant-rescoped from %s to %s = %d, want >= 1 "+
				"(wfID %q is status='ready', task_queue='default', tenant B's own row) -- cleat#2210: "+
				"under the bug this counts under tenant A's SESSION_CONTEXT, which has none of B's rows",
				tenantA, tenantB, got, wfID)
		}
	})
}
