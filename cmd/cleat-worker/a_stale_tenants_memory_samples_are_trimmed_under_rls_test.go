package main

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3572. RecordWorkflowMemory persists each sample to the store of the tenant
// the workflow belongs to, and memoryCleanupLoop trimmed the worker's own tenant's
// samples only, so every other tenant's workflow_memory_samples grew without
// bound. CleanupMemorySamples scopes itself to its store's tenant (an explicit
// tenant_id predicate, and row-level security besides), so the only way to trim a
// tenant's samples is through that tenant's own store.
//
// # This is that scenario reduced to a test
//
// The survivor's own tenant is the default one. A tenant other than the default
// holds more samples of one definition than the cap. The survivor runs one tick of
// the cleanup; the tenant must be left with the cap and no fewer. The worker's own
// tenant, with the same steps, is the known-positive: the old code trims it, so
// the difference between the two subtests is whose samples they are and nothing
// else. On PostgreSQL the connection is asserted not to bypass the policy.
func TestAnotherTenantsMemorySamplesAreTrimmedUnderRLS(t *testing.T) {
	owner := testutil.SuiteTestDB(t, "cleat_worker_memclean")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)
	var bypass, super bool
	if err := rlsDB.QueryRow(
		`SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass, &super); err != nil {
		t.Fatalf("reading the test role's attributes: %v", err)
	}
	if bypass || super {
		t.Fatalf("the RLS role has BYPASSRLS=%v SUPERUSER=%v; this test would measure nothing", bypass, super)
	}
	const other = "66666666-0000-4000-8000-000035720001"
	rig := postgresHBRig(t, owner, rlsDB, true, other)
	env := &memSampleEnv{
		rig: rig,
		count: func(t *testing.T, tenant, defName string) int {
			t.Helper()
			var n int
			if err := owner.QueryRow(`SELECT count(*) FROM workflow_memory_samples WHERE tenant_id = $1 AND def_name = $2`,
				tenant, defName).Scan(&n); err != nil {
				t.Fatalf("counting samples: %v", err)
			}
			return n
		},
		clear: func(_, defName string) {
			_, _ = owner.Exec(`DELETE FROM workflow_memory_samples WHERE def_name = $1`, defName)
			_, _ = owner.Exec(`DELETE FROM workflow_memory_stats WHERE def_name = $1`, defName)
		},
	}
	t.Run("a non-default tenant's samples, trimmed by a worker whose own tenant is the default", func(t *testing.T) {
		runMemorySamplesAreTrimmed(t, env, other)
	})
	t.Run("the worker's own tenant's samples (the control: the old code trims this one too)", func(t *testing.T) {
		runMemorySamplesAreTrimmed(t, env, engine.DefaultTenantUUID)
	})
}

// The SQL Server arm is its own test so that an unset CLEAT_TEST_MSSQL skips it
// without taking the PostgreSQL arm with it, and the other way round. SQL Server
// has no principal that bypasses its security policy, so every statement the test
// itself makes runs on a connection scoped to the tenant it is about.
func TestAnotherTenantsMemorySamplesAreTrimmedUnderRLSOnSQLServer(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping the SQL Server cleat#3572 memory-sample test")
	}
	const other = "88888888-0000-4000-8000-000035720001"
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
	env := &memSampleEnv{
		rig: rig,
		count: func(t *testing.T, tenant, defName string) int {
			t.Helper()
			var n int
			asTenant(t, tenant, func(c *sql.Conn) {
				if err := c.QueryRowContext(ctx, `SELECT count(*) FROM workflow_memory_samples WHERE tenant_id = @p1 AND def_name = @p2`,
					tenant, defName).Scan(&n); err != nil {
					t.Fatalf("counting samples: %v", err)
				}
			})
			return n
		},
		clear: func(tenant, defName string) {
			asTenant(nil, tenant, func(c *sql.Conn) {
				_, _ = c.ExecContext(ctx, `DELETE FROM workflow_memory_samples WHERE def_name = @p1`, defName)
				_, _ = c.ExecContext(ctx, `DELETE FROM workflow_memory_stats WHERE def_name = @p1`, defName)
			})
		},
	}
	t.Run("a non-default tenant's samples, trimmed by a worker whose own tenant is the default", func(t *testing.T) {
		runMemorySamplesAreTrimmed(t, env, other)
	})
	t.Run("the worker's own tenant's samples (the control: the old code trims this one too)", func(t *testing.T) {
		runMemorySamplesAreTrimmed(t, env, engine.DefaultTenantUUID)
	})
}

// memSampleEnv is what the test needs from a dialect beyond the stores: counting a
// tenant's sample rows of one definition and clearing the test's rows.
type memSampleEnv struct {
	rig   *hbRig
	count func(t *testing.T, tenant, defName string) int
	clear func(tenant, defName string)
}

func runMemorySamplesAreTrimmed(t *testing.T, env *memSampleEnv, tenant string) {
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

	defName := "mem-clean-3572-" + tenant[:8]
	clear := func() { env.clear(tenant, defName) }
	clear()
	t.Cleanup(clear)

	const have, keep = 8, 3
	for i := 0; i < have; i++ {
		if err := store.RecordWorkflowMemorySample(ctx, defName, int64(1_000_000*(i+1))); err != nil {
			t.Fatalf("recording sample %d: %v", i, err)
		}
	}
	if got := env.count(t, tenant, defName); got != have {
		t.Fatalf("%d sample rows for %s before the cleanup, want %d: the premise of this test does not hold", got, tenant, have)
	}

	// The survivor: its own store is the default tenant's, and it reaches the
	// other tenant only through the factory.
	w := newTestWorkerFromStore(rig.ownStore)
	defer w.cancel()
	w.id = "mem-clean-survivor"
	w.storeTenantID = engine.DefaultTenantUUID
	w.storeFactory = rig.factory
	w.claimAcrossTenants = true
	w.cleanupMemorySamplesOnce(keep)

	if got := env.count(t, tenant, defName); got != keep {
		t.Fatalf("tenant %s has %d sample rows of one definition after one tick of a worker's memory cleanup, want %d. "+
			"CleanupMemorySamples scopes itself to its store's tenant and ran through the worker's own store only (cleat#3572)",
			tenant, got, keep)
	}
}
