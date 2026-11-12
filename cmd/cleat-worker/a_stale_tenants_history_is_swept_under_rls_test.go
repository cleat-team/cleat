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

// cleat#3572. The retention arms scope themselves to their store's tenant (an
// explicit tenant_id predicate, and row-level security besides) and retentionLoop
// ran them through the worker's own store only. Measured as cleat_app by
// WS-cleat-2: a completed run of a non-default tenant was never deleted by a
// worker whose own tenant was the default, while the default tenant's was. The
// event history of a failed run is the default-on case: --retention-days is 30
// unless an operator sets it, and that history was swept for one tenant only.
//
// # This is that scenario reduced to a test
//
// The survivor's own tenant is the default one. A tenant other than the default
// holds a FAILED run with an event row and a TERMINATED run, both older than the
// retention window (the sweep is given a clock two days ahead, so nothing is
// written into a timestamp column). One sweep with only --retention-days on must
// remove the failed run's event row and leave both runs; one with only
// --completed-workflow-retention-days on must remove both runs. The worker's own
// tenant, with the same steps, is the known-positive: the old code sweeps it, so
// the difference between the two subtests is whose history it is and nothing
// else. On PostgreSQL the connection is asserted not to bypass the policy.
//
// The second test is a measurement of a claim in docs/operations/admin-api.md:
// that POST /api/admin/retention/sweep deletes "every tenant's completed
// history". It runs the endpoint's sweep, runRetentionSweepWindow, through the
// same survivor and reports whose rows it removed.
func TestAnotherTenantsHistoryIsSweptUnderRLS(t *testing.T) {
	owner := testutil.SuiteTestDB(t, "cleat_worker_retention")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)
	var bypass, super bool
	if err := rlsDB.QueryRow(
		`SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass, &super); err != nil {
		t.Fatalf("reading the test role's attributes: %v", err)
	}
	if bypass || super {
		t.Fatalf("the RLS role has BYPASSRLS=%v SUPERUSER=%v; this test would measure nothing", bypass, super)
	}
	const other = "66666666-0000-4000-8000-000035720002"
	rig := postgresHBRig(t, owner, rlsDB, true, other)
	env := &historyEnv{
		rig: rig,
		runs: func(t *testing.T, tenant, prefix string) int {
			t.Helper()
			var n int
			if err := owner.QueryRow(`SELECT count(*) FROM workflow_instances WHERE tenant_id = $1 AND id LIKE $2`,
				tenant, prefix+"%").Scan(&n); err != nil {
				t.Fatalf("counting runs: %v", err)
			}
			return n
		},
		events: func(t *testing.T, tenant, prefix string) int {
			t.Helper()
			var n int
			if err := owner.QueryRow(`SELECT count(*) FROM event_history WHERE tenant_id = $1 AND workflow_id LIKE $2`,
				tenant, prefix+"%").Scan(&n); err != nil {
				t.Fatalf("counting event rows: %v", err)
			}
			return n
		},
		addEvent: func(t *testing.T, tenant, runID string) {
			t.Helper()
			if _, err := owner.Exec(`INSERT INTO event_history (workflow_id, step, tenant_id, event_type) VALUES ($1, 0, $2, 'call')`,
				runID, tenant); err != nil {
				t.Fatalf("seeding an event row: %v", err)
			}
		},
		clear: func(tenant, prefix string) {
			_, _ = owner.Exec(`DELETE FROM event_history WHERE tenant_id = $1 AND workflow_id LIKE $2`, tenant, prefix+"%")
			_, _ = owner.Exec(`DELETE FROM workflow_leases WHERE tenant_id = $1 AND id LIKE $2`, tenant, prefix+"%")
			_, _ = owner.Exec(`DELETE FROM workflow_payloads WHERE tenant_id = $1 AND id LIKE $2`, tenant, prefix+"%")
			_, _ = owner.Exec(`DELETE FROM workflow_instances WHERE tenant_id = $1 AND id LIKE $2`, tenant, prefix+"%")
		},
	}
	runHistoryTests(t, env, other)
}

// The SQL Server arm is its own test so that an unset CLEAT_TEST_MSSQL skips it
// without taking the PostgreSQL arm with it, and the other way round. SQL Server
// has no principal that bypasses its security policy, so every statement the test
// itself makes runs on a connection scoped to the tenant it is about.
func TestAnotherTenantsHistoryIsSweptUnderRLSOnSQLServer(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping the SQL Server cleat#3572 retention test")
	}
	const other = "88888888-0000-4000-8000-000035720002"
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
	count := func(t *testing.T, tenant, query, prefix string) int {
		t.Helper()
		var n int
		asTenant(t, tenant, func(c *sql.Conn) {
			if err := c.QueryRowContext(ctx, query, tenant, prefix+"%").Scan(&n); err != nil {
				t.Fatalf("%s: %v", query, err)
			}
		})
		return n
	}
	env := &historyEnv{
		rig: rig,
		runs: func(t *testing.T, tenant, prefix string) int {
			return count(t, tenant, `SELECT count(*) FROM workflow_instances WHERE tenant_id = @p1 AND id LIKE @p2`, prefix)
		},
		events: func(t *testing.T, tenant, prefix string) int {
			return count(t, tenant, `SELECT count(*) FROM event_history WHERE tenant_id = @p1 AND workflow_id LIKE @p2`, prefix)
		},
		addEvent: func(t *testing.T, tenant, runID string) {
			t.Helper()
			asTenant(t, tenant, func(c *sql.Conn) {
				if _, err := c.ExecContext(ctx, `INSERT INTO event_history (workflow_id, step, tenant_id, event_type) VALUES (@p1, 0, @p2, 'call')`,
					runID, tenant); err != nil {
					t.Fatalf("seeding an event row: %v", err)
				}
			})
		},
		clear: func(tenant, prefix string) {
			asTenant(nil, tenant, func(c *sql.Conn) {
				_, _ = c.ExecContext(ctx, `DELETE FROM event_history WHERE tenant_id = @p1 AND workflow_id LIKE @p2`, tenant, prefix+"%")
				_, _ = c.ExecContext(ctx, `DELETE FROM workflow_instances WHERE tenant_id = @p1 AND id LIKE @p2`, tenant, prefix+"%")
			})
		},
	}
	runHistoryTests(t, env, other)
}

// historyEnv is what the test needs from a dialect beyond the stores: counting a
// tenant's runs and event rows under an id prefix, seeding an event row, and
// clearing the test's rows.
type historyEnv struct {
	rig      *hbRig
	runs     func(t *testing.T, tenant, prefix string) int
	events   func(t *testing.T, tenant, prefix string) int
	addEvent func(t *testing.T, tenant, runID string)
	clear    func(tenant, prefix string)
}

const historyPrefix = "ret-3572-"

// seedHistory gives tenant a FAILED run with one event row and a TERMINATED run.
func seedHistory(t *testing.T, env *historyEnv, tenant string) {
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
	env.clear(tenant, historyPrefix)
	t.Cleanup(func() { env.clear(tenant, historyPrefix) })

	defName := historyPrefix + "def-" + tenant[:8]
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	start := func(label string) string {
		t.Helper()
		id := fmt.Sprintf("%s%s-%s-%d", historyPrefix, label, tenant[:8], time.Now().UnixNano())
		if _, _, err := store.StartNewRun(ctx, id, defName, 1, json.RawMessage(`{}`), "", tenant, 0); err != nil {
			t.Fatalf("starting %s: %v", label, err)
		}
		return id
	}

	failed := start("failed")
	wf := claimOne(t, ctx, store, "ret-3572-claimer", failed)
	if err := store.FailWorkflow(ctx, failed, "ret-3572-claimer", wf.Generation, "boom", "TEST", "test", nil); err != nil {
		t.Fatalf("FailWorkflow: %v", err)
	}
	env.addEvent(t, tenant, failed)

	terminated := start("terminated")
	if err := store.TerminateWorkflow(ctx, terminated, "ret-3572"); err != nil {
		t.Fatalf("TerminateWorkflow: %v", err)
	}
}

// survivor is a worker whose own tenant is the default one and which reaches
// every other tenant only through the factory.
func survivor(env *historyEnv) *Worker {
	w := newTestWorkerFromStore(env.rig.ownStore)
	w.id = "ret-3572-survivor"
	w.storeTenantID = engine.DefaultTenantUUID
	w.storeFactory = env.rig.factory
	w.claimAcrossTenants = true
	return w
}

func runHistoryTests(t *testing.T, env *historyEnv, other string) {
	// Two days ahead, so a run completed now is older than a one-day window.
	later := func() time.Time { return time.Now().Add(48 * time.Hour) }

	sweepOne := func(t *testing.T, tenant string) {
		t.Helper()
		seedHistory(t, env, tenant)
		if r, e := env.runs(t, tenant, historyPrefix), env.events(t, tenant, historyPrefix); r != 2 || e != 1 {
			t.Fatalf("tenant %s holds %d runs and %d event rows before the sweep, want 2 and 1: the premise of this test does not hold", tenant, r, e)
		}
		w := survivor(env)
		defer w.cancel()

		// Only --retention-days on: the failed run's event row goes, both runs stay.
		w.runRetentionSweepAt(later(), 1, 0, 0)
		if got := env.events(t, tenant, historyPrefix); got != 0 {
			t.Errorf("tenant %s still has %d event row(s) of a failed run after one sweep with --retention-days on. The event "+
				"arm scopes itself to its store's tenant and ran through the worker's own store only (cleat#3572)", tenant, got)
		}
		if got := env.runs(t, tenant, historyPrefix); got != 2 {
			t.Errorf("tenant %s has %d run(s) after a sweep with --completed-workflow-retention-days off, want 2: that arm was off", tenant, got)
		}

		// Only --completed-workflow-retention-days on: both runs go.
		w.runRetentionSweepAt(later(), 0, 1, 0)
		if got := env.runs(t, tenant, historyPrefix); got != 0 {
			t.Errorf("tenant %s still has %d run(s) after one sweep with --completed-workflow-retention-days on (cleat#3572)", tenant, got)
		}
	}

	t.Run("a non-default tenant's history, swept by a worker whose own tenant is the default", func(t *testing.T) {
		sweepOne(t, other)
	})
	t.Run("the worker's own tenant's history (the control: the old code sweeps this one too)", func(t *testing.T) {
		sweepOne(t, engine.DefaultTenantUUID)
	})
}

// The admin endpoint's sweep, measured. docs/operations/admin-api.md says it
// deletes "every tenant's completed history older than" the window it is given.
// It runs through the worker's own store, so under row-level security it should
// reach the worker's own tenant only; this reports what it does reach. The
// worker's own tenant is the known-positive.
func TestTheAdminRetentionSweepReachesTheWorkersOwnTenantOnly(t *testing.T) {
	owner := testutil.SuiteTestDB(t, "cleat_worker_retention_admin")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)
	const other = "66666666-0000-4000-8000-000035720003"
	rig := postgresHBRig(t, owner, rlsDB, true, other)
	count := func(tenant string) int {
		var n int
		if err := owner.QueryRow(`SELECT count(*) FROM workflow_instances WHERE tenant_id = $1 AND id LIKE $2`,
			tenant, historyPrefix+"%").Scan(&n); err != nil {
			t.Fatalf("counting runs: %v", err)
		}
		return n
	}
	env := &historyEnv{
		rig:  rig,
		runs: func(t *testing.T, tenant, prefix string) int { return count(tenant) },
		events: func(t *testing.T, tenant, prefix string) int {
			var n int
			_ = owner.QueryRow(`SELECT count(*) FROM event_history WHERE tenant_id = $1 AND workflow_id LIKE $2`, tenant, prefix+"%").Scan(&n)
			return n
		},
		addEvent: func(t *testing.T, tenant, runID string) {
			if _, err := owner.Exec(`INSERT INTO event_history (workflow_id, step, tenant_id, event_type) VALUES ($1, 0, $2, 'call')`, runID, tenant); err != nil {
				t.Fatalf("seeding an event row: %v", err)
			}
		},
		clear: func(tenant, prefix string) {
			_, _ = owner.Exec(`DELETE FROM event_history WHERE tenant_id = $1 AND workflow_id LIKE $2`, tenant, prefix+"%")
			_, _ = owner.Exec(`DELETE FROM workflow_leases WHERE tenant_id = $1 AND id LIKE $2`, tenant, prefix+"%")
			_, _ = owner.Exec(`DELETE FROM workflow_payloads WHERE tenant_id = $1 AND id LIKE $2`, tenant, prefix+"%")
			_, _ = owner.Exec(`DELETE FROM workflow_instances WHERE tenant_id = $1 AND id LIKE $2`, tenant, prefix+"%")
		},
	}
	seedHistory(t, env, other)
	seedHistory(t, env, engine.DefaultTenantUUID)
	if count(other) != 2 || count(engine.DefaultTenantUUID) != 2 {
		t.Fatalf("premise: other %d, own %d runs, want 2 each", count(other), count(engine.DefaultTenantUUID))
	}
	w := survivor(env)
	defer w.cancel()
	time.Sleep(50 * time.Millisecond)
	res := w.runRetentionSweepWindow(0, 1, 0, 10*time.Millisecond)
	if len(res.Errors) > 0 {
		t.Fatalf("the sweep reported errors: %v", res.Errors)
	}
	if got := count(engine.DefaultTenantUUID); got != 0 {
		t.Fatalf("the worker's own tenant still has %d run(s) after the admin sweep: the known-positive did not fire, so this test measures nothing", got)
	}
	if got := count(other); got != 2 {
		t.Errorf("the admin sweep removed %d of another tenant's 2 runs. docs/operations/admin-api.md says it reaches every tenant; "+
			"if this is the behaviour, the plan to leave it own-tenant (cleat#3572) is wrong and the document is right", 2-got)
	}
}
