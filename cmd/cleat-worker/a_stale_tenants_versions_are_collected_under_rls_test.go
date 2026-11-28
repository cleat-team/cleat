package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3708. versionGCLoop collected old definition versions through the
// worker's own store only. The statements behind it carry no tenant predicate and
// row-level security is what scopes them, so a worker whose own tenant is the
// default one never saw another tenant's definitions, and with
// --version-gc-interval on every other tenant kept every old version for ever.
//
// # This is that scenario reduced to a test
//
// The survivor's own tenant is the default one. A tenant other than the default
// holds four versions of one workflow, all eligible by name and (with a 1ms age
// bound, so no timestamp is written) by age, and a second workflow whose versions
// are NOT eligible. A run is started on version 2, so version 2 has an active
// instance. With the minimum kept at 1, one sweep must remove versions 1 and 3 and
// keep 2 (in use), 4 (newest) and both ineligible ones. The worker's own tenant,
// with the same rows, is the known-positive: the old code sweeps it too, so the
// difference between the two subtests is whose definitions they are and nothing
// else. The flag off is its own subtest: through the loop with the interval unset,
// nothing is removed for either tenant. On PostgreSQL the connection is asserted
// not to bypass the policy; SQL Server has no principal that does.
const versionPrefix = "gc-3708-"

type versionsEnv struct {
	rig *hbRig
	// versions lists the versions of the named workflow held by tenant, read
	// through a connection scoped to that tenant.
	versions func(t *testing.T, tenant, name string) []int
	clear    func(tenant string)
}

func TestAnotherTenantsOldVersionsAreCollectedUnderRLS(t *testing.T) {
	owner := testutil.SuiteTestDB(t, "cleat_worker_versiongc")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)
	var bypass, super bool
	if err := rlsDB.QueryRow(
		`SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass, &super); err != nil {
		t.Fatalf("reading the test role's attributes: %v", err)
	}
	if bypass || super {
		t.Fatalf("the RLS role has BYPASSRLS=%v SUPERUSER=%v; this test would measure nothing", bypass, super)
	}
	const other = "aaaaaaaa-0000-4000-8000-000037080001"
	rig := postgresHBRig(t, owner, rlsDB, true, other)
	env := &versionsEnv{
		rig: rig,
		versions: func(t *testing.T, tenant, name string) []int {
			t.Helper()
			rows, err := owner.Query(`SELECT version FROM workflow_defs WHERE tenant_id = $1 AND name = $2 ORDER BY version`, tenant, name)
			if err != nil {
				t.Fatalf("listing versions: %v", err)
			}
			defer rows.Close()
			var vs []int
			for rows.Next() {
				var v int
				if err := rows.Scan(&v); err != nil {
					t.Fatal(err)
				}
				vs = append(vs, v)
			}
			return vs
		},
		clear: func(tenant string) {
			for _, q := range []string{
				`DELETE FROM workflow_leases WHERE tenant_id = $1 AND id LIKE $2`,
				`DELETE FROM workflow_payloads WHERE tenant_id = $1 AND id LIKE $2`,
				`DELETE FROM workflow_instances WHERE tenant_id = $1 AND id LIKE $2`,
				`DELETE FROM workflow_defs WHERE tenant_id = $1 AND name LIKE $2`,
			} {
				_, _ = owner.Exec(q, tenant, versionPrefix+"%")
			}
		},
	}
	runVersionTests(t, env, other)
}

// The SQL Server arm is its own test so that an unset CLEAT_TEST_MSSQL skips it
// without taking the PostgreSQL arm with it. Every statement the test itself makes
// runs on a connection scoped to the tenant it is about.
func TestAnotherTenantsOldVersionsAreCollectedUnderRLSOnSQLServer(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping the SQL Server cleat#3708 version-GC test")
	}
	const other = "bbbbbbbb-0000-4000-8000-000037080001"
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
	env := &versionsEnv{
		rig: rig,
		versions: func(t *testing.T, tenant, name string) []int {
			t.Helper()
			var vs []int
			asTenant(t, tenant, func(c *sql.Conn) {
				rows, err := c.QueryContext(ctx, `SELECT version FROM workflow_defs WHERE tenant_id = @p1 AND name = @p2 ORDER BY version`, tenant, name)
				if err != nil {
					t.Fatalf("listing versions: %v", err)
				}
				defer rows.Close()
				for rows.Next() {
					var v int
					if err := rows.Scan(&v); err != nil {
						t.Fatal(err)
					}
					vs = append(vs, v)
				}
			})
			return vs
		},
		clear: func(tenant string) {
			asTenant(nil, tenant, func(c *sql.Conn) {
				for _, q := range []string{
					`DELETE FROM workflow_instances WHERE tenant_id = @p1 AND id LIKE @p2`,
					`DELETE FROM workflow_defs WHERE tenant_id = @p1 AND name LIKE @p2`,
				} {
					_, _ = c.ExecContext(ctx, q, tenant, versionPrefix+"%")
				}
			})
		},
	}
	runVersionTests(t, env, other)
}

// seedVersions gives tenant four eligible versions of one workflow, two
// ineligible versions of another, and a run on version 2 of the first.
func seedVersions(t *testing.T, env *versionsEnv, tenant string) (eligible, ineligible string) {
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
	env.clear(tenant)
	t.Cleanup(func() { env.clear(tenant) })

	eligible = versionPrefix + "eligible-" + tenant[:8]
	ineligible = versionPrefix + "ineligible-" + tenant[:8]
	deploy := func(name string, version int, gc bool) {
		t.Helper()
		if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: name, Version: version, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1, GCEligible: gc,
		}); err != nil {
			t.Fatalf("DeployWorkflowDef %s v%d: %v", name, version, err)
		}
	}
	for v := 1; v <= 4; v++ {
		deploy(eligible, v, true)
	}
	deploy(ineligible, 1, false)
	deploy(ineligible, 2, false)

	// An instance still running on version 2: the version must be kept.
	id := fmt.Sprintf("%srun-%s-%d", versionPrefix, tenant[:8], time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, id, eligible, 2, json.RawMessage(`{}`), "", tenant, 0); err != nil {
		t.Fatalf("starting a run on version 2: %v", err)
	}
	return eligible, ineligible
}

func versionSurvivor(env *versionsEnv) *Worker {
	w := newTestWorkerFromStore(env.rig.ownStore)
	w.id = "gc-3708-survivor"
	w.storeTenantID = engine.DefaultTenantUUID
	w.storeFactory = env.rig.factory
	w.claimAcrossTenants = true
	// Keep only the newest version, and let a version that is a few milliseconds
	// old be old enough: no timestamp column is written.
	w.versionGCMinVersions = 1
	w.versionGCMaxAge = time.Millisecond
	return w
}

func runVersionTests(t *testing.T, env *versionsEnv, other string) {
	check := func(t *testing.T, tenant, eligible, ineligible string, wantEligible []int) {
		t.Helper()
		if got := env.versions(t, tenant, eligible); !reflect.DeepEqual(got, wantEligible) {
			t.Errorf("tenant %s holds versions %v of its eligible workflow, want %v", tenant, got, wantEligible)
		}
		if got := env.versions(t, tenant, ineligible); !reflect.DeepEqual(got, []int{1, 2}) {
			t.Errorf("tenant %s holds versions %v of its INELIGIBLE workflow, want [1 2]: GC may only collect what is armed for collection", tenant, got)
		}
	}

	sweepOne := func(t *testing.T, tenant string) {
		t.Helper()
		eligible, ineligible := seedVersions(t, env, tenant)
		// PREMISE, measured: through a store scoped to the tenant the four versions are there.
		if got := env.versions(t, tenant, eligible); !reflect.DeepEqual(got, []int{1, 2, 3, 4}) {
			t.Fatalf("tenant %s holds %v before the sweep, want [1 2 3 4]: the premise of this test does not hold", tenant, got)
		}
		w := versionSurvivor(env)
		defer w.cancel()
		time.Sleep(20 * time.Millisecond) // older than the 1ms bound

		w.runVersionGCSweep()

		// Collected: 1 and 3. Kept: 2 (an instance is running on it) and 4 (newest).
		check(t, tenant, eligible, ineligible, []int{2, 4})
	}

	t.Run("a non-default tenant's old versions, collected by a worker whose own tenant is the default", func(t *testing.T) {
		sweepOne(t, other)
	})
	t.Run("the worker's own tenant's old versions (the control: the old code collects these too)", func(t *testing.T) {
		sweepOne(t, engine.DefaultTenantUUID)
	})
	t.Run("the flag off: nothing is collected for either tenant", func(t *testing.T) {
		otherEligible, otherIneligible := seedVersions(t, env, other)
		ownEligible, ownIneligible := seedVersions(t, env, engine.DefaultTenantUUID)
		w := versionSurvivor(env)
		defer w.cancel()
		w.versionGCInterval = 0 // the flag default
		time.Sleep(20 * time.Millisecond)

		done := make(chan struct{})
		w.wg.Add(1)
		go func() { defer close(done); w.versionGCLoop() }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("versionGCLoop did not return with --version-gc-interval unset")
		}
		check(t, other, otherEligible, otherIneligible, []int{1, 2, 3, 4})
		check(t, engine.DefaultTenantUUID, ownEligible, ownIneligible, []int{1, 2, 3, 4})
	})
}
