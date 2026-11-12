package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3572. GetCompactionCandidates and CompactWorkflowHistory scope themselves
// to their store's tenant, and compactionLoop ran them through the worker's own
// store only, so a workflow of any other tenant never compacted however long its
// history grew: its replay cost and its event_history footprint were unbounded.
//
// # This is that scenario reduced to a test
//
// The survivor's own tenant is the default one. A tenant other than the default
// holds a running workflow whose history is longer than the worker's compaction
// threshold. The survivor runs one tick of the compaction loop; the history must
// be shorter. The worker's own tenant, with the same steps, is the known-positive:
// the old code compacts it, so the difference between the two subtests is whose
// workflow it is and nothing else. On PostgreSQL the connection is asserted not to
// bypass the policy.
func TestAnotherTenantsLongHistoryIsCompactedUnderRLS(t *testing.T) {
	owner := testutil.SuiteTestDB(t, "cleat_worker_compaction")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)
	var bypass, super bool
	if err := rlsDB.QueryRow(
		`SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass, &super); err != nil {
		t.Fatalf("reading the test role's attributes: %v", err)
	}
	if bypass || super {
		t.Fatalf("the RLS role has BYPASSRLS=%v SUPERUSER=%v; this test would measure nothing", bypass, super)
	}
	const other = "66666666-0000-4000-8000-000035720004"
	env := &compactEnv{
		rig: postgresHBRig(t, owner, rlsDB, true, other),
		events: func(t *testing.T, tenant, runID string) int {
			t.Helper()
			var n int
			if err := owner.QueryRow(`SELECT count(*) FROM event_history WHERE tenant_id = $1 AND workflow_id = $2`,
				tenant, runID).Scan(&n); err != nil {
				t.Fatalf("counting event rows: %v", err)
			}
			return n
		},
		clear: func(tenant, prefix string) {
			_, _ = owner.Exec(`DELETE FROM event_history WHERE tenant_id = $1 AND workflow_id LIKE $2`, tenant, prefix+"%")
			_, _ = owner.Exec(`DELETE FROM workflow_leases WHERE tenant_id = $1 AND id LIKE $2`, tenant, prefix+"%")
			_, _ = owner.Exec(`DELETE FROM workflow_payloads WHERE tenant_id = $1 AND id LIKE $2`, tenant, prefix+"%")
			_, _ = owner.Exec(`DELETE FROM workflow_instances WHERE tenant_id = $1 AND id LIKE $2`, tenant, prefix+"%")
		},
	}
	runCompactionTests(t, env, other)
}

// The SQL Server arm is its own test so that an unset CLEAT_TEST_MSSQL skips it
// without taking the PostgreSQL arm with it, and the other way round. SQL Server
// has no principal that bypasses its security policy, so every statement the test
// itself makes runs on a connection scoped to the tenant it is about.
func TestAnotherTenantsLongHistoryIsCompactedUnderRLSOnSQLServer(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping the SQL Server cleat#3572 compaction test")
	}
	const other = "88888888-0000-4000-8000-000035720004"
	rig := mssqlHBRig(t, other)
	ctx := context.Background()
	asTenant := func(t *testing.T, tenant string, fn func(c *sql.Conn)) {
		if t != nil {
			t.Helper()
		}
		c, err := rig.db.Conn(ctx)
		if err != nil {
			if t != nil {
				t.Fatal(err)
			}
			return
		}
		defer c.Close()
		if _, err := c.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant); err != nil {
			if t != nil {
				t.Fatalf("scoping the connection to %s: %v", tenant, err)
			}
			return
		}
		fn(c)
	}
	env := &compactEnv{
		rig: rig,
		events: func(t *testing.T, tenant, runID string) int {
			t.Helper()
			var n int
			asTenant(t, tenant, func(c *sql.Conn) {
				if err := c.QueryRowContext(ctx, `SELECT count(*) FROM event_history WHERE tenant_id = @p1 AND workflow_id = @p2`,
					tenant, runID).Scan(&n); err != nil {
					t.Fatalf("counting event rows: %v", err)
				}
			})
			return n
		},
		clear: func(tenant, prefix string) {
			asTenant(nil, tenant, func(c *sql.Conn) {
				_, _ = c.ExecContext(ctx, `DELETE FROM event_history WHERE tenant_id = @p1 AND workflow_id LIKE @p2`, tenant, prefix+"%")
				_, _ = c.ExecContext(ctx, `DELETE FROM workflow_instances WHERE tenant_id = @p1 AND id LIKE @p2`, tenant, prefix+"%")
			})
		},
	}
	runCompactionTests(t, env, other)
}

// compactEnv is what the test needs from a dialect beyond the stores: counting one
// run's event rows and clearing the test's rows.
type compactEnv struct {
	rig    *hbRig
	events func(t *testing.T, tenant, runID string) int
	clear  func(tenant, prefix string)
}

const compactPrefix = "compact-3572-"

func runCompactionTests(t *testing.T, env *compactEnv, other string) {
	run := func(t *testing.T, tenant string) {
		t.Helper()
		ctx := context.Background()
		rig := env.rig
		if tenant != engine.DefaultTenantUUID {
			rig.register(t, tenant)
		}
		store, closeStore, err := rig.factory.OpenStore(ctx, tenant, "default")
		if err != nil {
			t.Fatalf("opening the tenant's store: %v", err)
		}
		defer closeStore.Close()
		env.clear(tenant, compactPrefix)
		t.Cleanup(func() { env.clear(tenant, compactPrefix) })

		defName := compactPrefix + "def-" + tenant[:8]
		if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("DeployWorkflowDef: %v", err)
		}
		runID := fmt.Sprintf("%s%s-%d", compactPrefix, tenant[:8], time.Now().UnixNano())
		if _, _, err := store.StartNewRun(ctx, runID, defName, 1, json.RawMessage(`{}`), "", tenant, 0); err != nil {
			t.Fatalf("StartNewRun: %v", err)
		}
		const threshold, have = 4, 12
		for i := 0; i < have; i++ {
			if err := store.AppendEventHistory(ctx, runID, engine.EventRecord{
				Step: i, EventType: engine.EventTypeCall, Service: "svc", Op: fmt.Sprintf("op%d", i),
				Request: `{}`, Response: fmt.Sprintf(`{"step":%d}`, i),
			}); err != nil {
				t.Fatalf("AppendEventHistory %d: %v", i, err)
			}
		}
		if got := env.events(t, tenant, runID); got != have {
			t.Fatalf("%d event rows for %s before the tick, want %d: the premise of this test does not hold", got, tenant, have)
		}

		w := newTestWorkerFromStore(rig.ownStore)
		defer w.cancel()
		w.id = "compact-3572-survivor"
		w.storeTenantID = engine.DefaultTenantUUID
		w.storeFactory = rig.factory
		w.claimAcrossTenants = true
		w.compactionThreshold = threshold
		w.compactOnce()

		// The tail kept is half the threshold; everything before it is folded into
		// the compaction state on the run's row.
		if got := env.events(t, tenant, runID); got != threshold/2 {
			t.Fatalf("tenant %s has %d event rows of one run after one compaction tick, want %d. The candidate query and "+
				"CompactWorkflowHistory scope themselves to their store's tenant and ran through the worker's own store only (cleat#3572)",
				tenant, got, threshold/2)
		}
	}

	t.Run("a non-default tenant's long history, compacted by a worker whose own tenant is the default", func(t *testing.T) {
		run(t, other)
	})
	t.Run("the worker's own tenant's long history (the control: the old code compacts this one too)", func(t *testing.T) {
		run(t, engine.DefaultTenantUUID)
	})
}
