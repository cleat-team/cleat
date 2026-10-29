package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3566. A run of a tenant other than the worker's own was reported lost by
// the worker's heartbeat at its FIRST beat, never had its heartbeat_at renewed,
// had its next durable call refused by the fence, and was released and re-claimed
// instead of finishing. Measured on develop and on the v0.5.0 tag with a tenant
// run that outlived a beat: the generation climbed 1 to 6 in about a minute and
// the run never reached a terminal state.
//
// # Why no existing suite saw it
//
// HeartbeatBatchFenced reads the lease state (workflow_leases on PostgreSQL,
// workflow_instances on SQL Server), and the tenant it sees is a property of the
// store instance: row-level security on PostgreSQL, a security policy bound to
// SESSION_CONTEXT on SQL Server. So the bug needs all three of: a principal the
// policy applies to, a run of a tenant that is not the worker's, and a run that is
// still executing when a beat lands. The suites that run a real worker on
// PostgreSQL connect as a superuser, which bypasses the policy, so the heartbeat
// saw every row; the ones that run another tenant use a recording store, which
// can show that a store was asked and not that it could see a row; and the one
// that ran this end to end (the integration-hub tenant sandbox scenario) ran
// guests that end within seconds, so it failed only when a claim landed just
// before a tick, once in thirty-one runs. SQL Server's own doc comment on
// HeartbeatBatchFenced said the heartbeat must carry no tenant predicate because
// it covers every tenant: the premise was that the SQL scopes it, when the policy
// does regardless of the SQL.
//
// # What this does, and why it is not phase-dependent
//
// A real guest (the defer fixture: two sequential durable calls) runs a tenant's
// run through a real worker. Inside the first call's HTTP handler, while the guest
// is still executing, the test forces THREE beats by calling
// heartbeatAndFenceInFlight itself, so the beats do not depend on when a ticker
// fires relative to the claim. It then asserts:
//
//   - heartbeat_at moved on every beat, which is the property the reaper rests on,
//     and is what "not fenced" alone would not show;
//   - the second durable call was made, which the fence refuses once a run is
//     marked lost;
//   - the run is done, not released.
//
// PostgreSQL runs it twice: as cleat_app, which the policy applies to, and as a
// superuser, which bypasses it. The superuser twin is the known-positive: the old
// code passed there too, so the difference is the role and not the fixture. SQL
// Server has no bypassing principal (the policy applies to sa and dbo too), so
// its rig has no twin; its premise probe plays that part. MySQL is not
// applicable: tenancy there is a database per tenant and it is single-tenant only
// by decision.
type hbRig struct {
	name          string
	policyApplies bool
	db            *sql.DB
	factory       engine.StoreFactory
	ownStore      engine.WorkflowStore
	register      func(t *testing.T, tenant string)
	// visibleRows counts the run's lease row through a connection whose tenant
	// context is set to tenant.
	visibleRows func(t *testing.T, tenant, wfID string) int
	// heartbeatAt reads the run's heartbeat_at as the given tenant.
	heartbeatAt func(t *testing.T, tenant, wfID string) time.Time
	tenant      string
}

func TestATenantsRunOutlivesItsHeartbeatsUnderRLS(t *testing.T) {
	owner := testutil.SuiteTestDB(t, "cleat_worker_hbtenant")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, owner)
	// PRECONDITION: the role this runs as neither bypasses the policy nor is a
	// superuser. Without it a pass says nothing about the heartbeat.
	var bypass, super bool
	if err := rlsDB.QueryRow(
		`SELECT rolbypassrls, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass, &super); err != nil {
		t.Fatalf("reading the test role's attributes: %v", err)
	}
	if bypass || super {
		t.Fatalf("the RLS role has BYPASSRLS=%v SUPERUSER=%v; this test would measure nothing", bypass, super)
	}
	t.Run("postgres as cleat_app, which the policy applies to", func(t *testing.T) {
		runATenantsRunThroughItsBeats(t, postgresHBRig(t, owner, rlsDB, true, "11111111-0000-4000-8000-000000003566"))
	})
	t.Run("postgres twin: as a superuser, which bypasses it (the old code passed here too)", func(t *testing.T) {
		runATenantsRunThroughItsBeats(t, postgresHBRig(t, owner, owner, false, "22222222-0000-4000-8000-000000003566"))
	})
}

// The SQL Server arm is its own test so that an unset CLEAT_TEST_MSSQL skips it
// without taking the PostgreSQL arm with it, and the other way round.
func TestATenantsRunOutlivesItsHeartbeatsUnderRLSOnSQLServer(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping the SQL Server cleat#3566 test")
	}
	runATenantsRunThroughItsBeats(t, mssqlHBRig(t, "33333333-0000-4000-8000-000000003566"))
}

func postgresHBRig(t *testing.T, owner, db *sql.DB, policyApplies bool, tenant string) *hbRig {
	t.Helper()
	return &hbRig{
		name: "postgres", policyApplies: policyApplies, db: db, tenant: tenant,
		factory:  engine.NewPostgresStoreFactory(db, "public"),
		ownStore: engine.NewPostgresStore(db),
		register: func(t *testing.T, tenant string) {
			t.Helper()
			if _, err := owner.Exec(
				`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, $2, false) ON CONFLICT (tenant_id) DO NOTHING`,
				tenant, "cleat-3566-"+tenant[:8]); err != nil {
				t.Fatalf("registering tenant: %v", err)
			}
			t.Cleanup(func() {
				_, _ = owner.Exec(`DELETE FROM workflow_instances WHERE tenant_id = $1`, tenant)
				_, _ = owner.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, tenant)
			})
		},
		visibleRows: func(t *testing.T, tenant, wfID string) int {
			t.Helper()
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec(`SELECT set_config('cleat.tenant_id', $1, true)`, tenant); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := tx.QueryRow(`SELECT count(*) FROM workflow_leases WHERE id = $1`, wfID).Scan(&n); err != nil {
				t.Fatalf("counting visible lease rows as tenant %s: %v", tenant, err)
			}
			return n
		},
		// Read from the superuser connection, which bypasses the policy in both
		// rigs, so the reading cannot itself be hidden.
		heartbeatAt: func(t *testing.T, _ string, wfID string) time.Time {
			t.Helper()
			var at time.Time
			if err := owner.QueryRow(`SELECT heartbeat_at FROM workflow_leases WHERE id = $1`, wfID).Scan(&at); err != nil {
				t.Fatalf("reading heartbeat_at: %v", err)
			}
			return at
		},
	}
}

func mssqlHBRig(t *testing.T, tenant string) *hbRig {
	t.Helper()
	base := os.Getenv("CLEAT_TEST_MSSQL")
	adminDB, err := sql.Open("sqlserver", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	if err := adminDB.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_MSSQL is set but SQL Server is unreachable at %s: %v", redactMSSQLDSN(base), err)
	}
	ctx := context.Background()
	const dbName = "cleat_hb_3566_test"
	if _, err := adminDB.ExecContext(ctx, fmt.Sprintf(
		"IF DB_ID('%s') IS NOT NULL BEGIN ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s; END",
		dbName, dbName, dbName)); err != nil {
		t.Fatalf("drop stale test database: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), fmt.Sprintf(
			"ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s", dbName, dbName))
	})
	connStr, err := mssqlConnStrForDB(base, dbName)
	if err != nil {
		t.Fatalf("derive test DSN: %v", err)
	}
	db, err := sql.Open("sqlserver", connStr)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// The REAL shipped migrations: the security policies are the thing under test.
	if err := migration.NewRunner(db, migration.DialectMSSQL, "../../migrations").Run(ctx); err != nil {
		t.Fatalf("apply mssql migrations: %v", err)
	}

	factory := engine.NewMSSQLStoreFactory(connStr)
	own, closeOwn, err := factory.OpenStore(ctx, engine.DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("opening the worker's own store: %v", err)
	}
	t.Cleanup(func() { _ = closeOwn.Close() })

	// asTenant runs fn on one connection whose SESSION_CONTEXT tenant is set, which
	// is what every statement of the store's own transactions does.
	asTenant := func(t *testing.T, tenant string, fn func(c *sql.Conn)) {
		t.Helper()
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant); err != nil {
			t.Fatalf("scoping the connection to %s: %v", tenant, err)
		}
		fn(c)
	}
	return &hbRig{
		name: "mssql", policyApplies: true, db: db, tenant: tenant, factory: factory, ownStore: own,
		register: func(t *testing.T, tenant string) {
			t.Helper()
			if _, err := db.ExecContext(ctx,
				`MERGE admin.tenants AS t USING (SELECT @p1 AS tenant_id, @p2 AS name, CAST(0 AS bit) AS suspended) AS s
				   ON t.tenant_id = s.tenant_id
				 WHEN NOT MATCHED THEN INSERT (tenant_id, name, suspended) VALUES (s.tenant_id, s.name, s.suspended);`,
				tenant, "cleat-3566-"+tenant[:8]); err != nil {
				t.Fatalf("registering tenant: %v", err)
			}
		},
		visibleRows: func(t *testing.T, tenant, wfID string) int {
			t.Helper()
			var n int
			asTenant(t, tenant, func(c *sql.Conn) {
				if err := c.QueryRowContext(ctx, `SELECT count(*) FROM workflow_instances WHERE id = @p1`, wfID).Scan(&n); err != nil {
					t.Fatalf("counting visible instances as tenant %s: %v", tenant, err)
				}
			})
			return n
		},
		// SQL Server has no bypassing principal, so the reading is taken scoped to the
		// run's own tenant, where the run is visible (the premise probe checks that).
		heartbeatAt: func(t *testing.T, tenant, wfID string) time.Time {
			t.Helper()
			var at time.Time
			asTenant(t, tenant, func(c *sql.Conn) {
				if err := c.QueryRowContext(ctx, `SELECT heartbeat_at FROM workflow_instances WHERE id = @p1`, wfID).Scan(&at); err != nil {
					t.Fatalf("reading heartbeat_at: %v", err)
				}
			})
			return at
		},
	}
}

func runATenantsRunThroughItsBeats(t *testing.T, rig *hbRig) {
	t.Helper()
	ctx := context.Background()
	tenant := rig.tenant
	rig.register(t, tenant)

	tenantStore, closeTenant, err := rig.factory.OpenStore(ctx, tenant, "default")
	if err != nil {
		t.Fatalf("opening the tenant's store: %v", err)
	}
	defer closeTenant.Close()

	wasmBytes := buildDeferFixture(t)
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	const defName = "hb-tenant-3566"
	if err := tenantStore.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef under the tenant: %v", err)
	}
	wfID := fmt.Sprintf("hb-tenant-3566-%d", time.Now().UnixNano())
	if _, _, err := tenantStore.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
		json.RawMessage(`{"__entry_point":"TwoSequentialCalls"}`), "", tenant, 0); err != nil {
		t.Fatalf("StartNewRun for the tenant: %v", err)
	}

	logs := &syncBuffer{}
	// The worker's OWN store is the default tenant's, as in a worker started with
	// no --tenant, and it reaches the tenant's run only through the factory.
	w := newRealDeferPhaseWorker(t, rig.db, rig.ownStore, logs)
	w.id = "hb-tenant-3566-worker"
	w.storeFactory = rig.factory
	w.egress = &engine.EgressGuard{
		AllowLoopback:  true,
		TenantOptional: func(context.Context) bool { return true },
	}

	claimed := claimOne(t, ctx, tenantStore, w.id, wfID)
	if claimed.TenantID != tenant {
		t.Fatalf("claimed run belongs to %q, want %q", claimed.TenantID, tenant)
	}
	w.inflight.Store(wfID, claimed)

	// PREMISE, measured, so a pass cannot be for the wrong reason: through a
	// connection scoped to the WORKER'S tenant the run's lease state is hidden when
	// the policy applies and visible when it does not; scoped to the run's own
	// tenant it is visible either way.
	if got := rig.visibleRows(t, engine.DefaultTenantUUID, wfID); rig.policyApplies && got != 0 || !rig.policyApplies && got != 1 {
		t.Fatalf("%s: scoped to the worker's tenant the run's lease state is visible %d time(s) (policy applies: %v); "+
			"the premise of this test does not hold on this database", rig.name, got, rig.policyApplies)
	}
	if got := rig.visibleRows(t, tenant, wfID); got != 1 {
		t.Fatalf("%s: scoped to the run's own tenant the lease state is visible %d time(s), want 1; "+
			"\"hidden by the policy\" cannot be told from \"not there\"", rig.name, got)
	}

	atClaim := rig.heartbeatAt(t, tenant, wfID)

	calls := &recordedCalls{}
	var mu sync.Mutex
	beats := []time.Time{atClaim}
	var fencedAfter, landed []bool
	svc := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		op := strings.TrimPrefix(r.URL.Path, "/call/")
		calls.add(op)
		if op == "work/first" {
			// The guest is executing right now. Force three beats, spaced so that
			// the clock differs, instead of waiting for a ticker whose phase against
			// the claim is not ours to choose.
			for i := 0; i < 3; i++ {
				time.Sleep(30 * time.Millisecond)
				ok := w.heartbeatAndFenceInFlight()
				mu.Lock()
				landed = append(landed, ok)
				beats = append(beats, rig.heartbeatAt(t, tenant, wfID))
				fencedAfter = append(fencedAfter, w.runIsFenced(wfID, claimed.Generation))
				mu.Unlock()
			}
		}
		rw.Header().Set("Content-Type", "application/json")
		fmt.Fprint(rw, `{"ok":true}`)
	}))
	defer svc.Close()
	old := *benchSvcURL
	*benchSvcURL = svc.URL
	defer func() { *benchSvcURL = old }()

	runExecuteWorkflow(w, claimed)

	mu.Lock()
	defer mu.Unlock()
	if len(beats) != 4 {
		t.Fatalf("recorded %d heartbeat reading(s), want the claim plus 3 (the guest never reached its first call; logs:\n%s)",
			len(beats), logs.String())
	}
	for i := 1; i < len(beats); i++ {
		if !landed[i-1] {
			t.Errorf("beat %d did not land", i)
		}
		if fencedAfter[i-1] {
			t.Errorf("beat %d fenced a run that is healthy and held by this worker. The heartbeat asked a store whose "+
				"tenant is not the run's, so the run's lease state was invisible to it (cleat#3566)", i)
		}
		if !beats[i].After(beats[i-1]) {
			t.Errorf("heartbeat_at did not move on beat %d (%s then %s): the reaper reads this column, so a run whose "+
				"heartbeat is never renewed is reclaimable while it is still executing", i, beats[i-1], beats[i])
		}
	}
	if !calls.has("work/second") {
		t.Errorf("the guest never made its second durable call: a run marked lost has its next call refused (logs:\n%s)", logs.String())
	}
	if got := statusOf(t, ctx, tenantStore, wfID); got != "done" {
		t.Errorf("status after the run = %q, want \"done\": a fenced run is released and re-claimed, not finished (logs:\n%s)",
			got, logs.String())
	}
}
