package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// cleat#2187. startPluginWorkflow runs against the PROCESS-WIDE store,
// opened once for the default tenant for the worker's whole lifetime (see
// main()). Every non-test caller of plugin.StartRequest -- the scheduler,
// event-triggers, jobqueue -- derives req.TenantID from a durable row it
// already owns, so a plugin start naming a non-default tenant is routine,
// not an edge case. Before this fix, that request either failed outright
// (PostgreSQL: RLS's WITH CHECK compares the row being inserted against the
// SESSION's own scoped tenant, which was still the default) or succeeded
// scoped to the wrong session (SQL Server: cleat#2204, cleat#2205).
//
// These tests run the real startPluginWorkflow against a real store on each
// dialect -- not a mock -- because the defect is in what the STORE does with
// a foreign tenantID, and a mock cannot show that. See
// tenant_isolation_db_test.go for the identical reasoning about the HTTP
// layer's own tenant scoping.

// TestStartPluginWorkflow_PostgresScopesToTheDestinationTenant is the
// PostgreSQL half.
func TestStartPluginWorkflow_PostgresScopesToTheDestinationTenant(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed cleat#2187 test")
	}
	db := testutil.SuiteTestDB(t, "cleat_worker")

	// See tenant_isolation_db_test.go: a superuser connection bypasses RLS
	// unconditionally, and CLEAT_TEST_POSTGRES conventionally points at one.
	// Run the store under test against the RLS role or this test proves
	// nothing.
	rlsDB := testutil.OpenPostgresRLSTestDB(t, db)
	factory := engine.NewPostgresStoreFactory(rlsDB, "public")
	ctx := context.Background()

	tenantDefault := engine.DefaultTenantUUID
	tenantB := uuid.New().String()

	// The process-wide store: opened for the default tenant, exactly as
	// main() opens it.
	processStore, processCloser, err := factory.OpenStore(ctx, tenantDefault, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer processCloser.Close()

	// Deploy the SAME def name under BOTH tenants, at DIFFERENT versions --
	// this also exercises ListVersions, which startPluginWorkflow calls
	// before StartNewRun. Before this fix ListVersions ran unscoped too, so
	// it would have silently answered with the DEFAULT tenant's version list
	// rather than tenant B's.
	deployTo := func(tenant string, version int) {
		t.Helper()
		st, closer, err := factory.OpenStore(ctx, tenant, "default")
		if err != nil {
			t.Fatalf("OpenStore(%s): %v", tenant, err)
		}
		defer closer.Close()
		if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: "plugin-start-2187", Version: version, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("deploy def for %s at v%d: %v", tenant, version, err)
		}
	}
	deployTo(tenantDefault, 1)
	deployTo(tenantB, 2)

	// KNOWN-POSITIVE CONTROL: the default tenant's own start, through the
	// process store, must keep working -- this fix must not have narrowed
	// the store to ONLY foreign tenants.
	defaultRunID, err := startPluginWorkflow(ctx, processStore, plugin.StartRequest{
		DefName: "plugin-start-2187", Input: json.RawMessage(`{}`), IdempotencyKey: "default-key", TenantID: tenantDefault,
	})
	if err != nil {
		t.Fatalf("KNOWN-POSITIVE CONTROL FAILED: default tenant's own plugin start: %v", err)
	}
	if defaultRunID == "" {
		t.Fatalf("default tenant's plugin start returned an empty run id")
	}

	// THE FIX under test: a plugin start naming tenant B, through the
	// process store still opened for the default tenant.
	runID, err := startPluginWorkflow(ctx, processStore, plugin.StartRequest{
		DefName: "plugin-start-2187", Input: json.RawMessage(`{}`), IdempotencyKey: "tenant-b-key", TenantID: tenantB,
	})
	if err != nil {
		t.Fatalf("startPluginWorkflow for a non-default tenant failed: %v -- "+
			"cleat#2187 unfixed", err)
	}
	if runID == "" {
		t.Fatal("startPluginWorkflow for tenant B returned an empty run id")
	}

	// Correctly stamped AND correctly versioned: reading it back through a
	// store genuinely scoped to tenant B must show version 2 (tenant B's
	// own deployed version), not version 1 (the default tenant's) -- proof
	// that ListVersions, not just StartNewRun, ran under tenant B's scope.
	storeB, closerB, err := factory.OpenStore(ctx, tenantB, "default")
	if err != nil {
		t.Fatalf("OpenStore(B): %v", err)
	}
	defer closerB.Close()
	wf, err := storeB.GetWorkflowByID(ctx, runID)
	if err != nil {
		t.Fatalf("tenant B reading back its own run: %v", err)
	}
	if wf == nil {
		t.Fatal("tenant B cannot see the run started for it -- not stamped to tenant B")
	}
	if wf.DefVersion != 2 {
		t.Errorf("run started for tenant B used def version %d, want 2 (tenant B's own deployed "+
			"version) -- ListVersions answered from the wrong tenant's scope", wf.DefVersion)
	}

	// NEGATIVE CONTROL, independent of the fix: a store still scoped to the
	// default tenant, asked to write a row CLAIMING tenant B via the
	// tenantID PARAMETER alone (StartNewRun's own tenantID argument, not
	// startPluginWorkflow's scoping) must still be refused by RLS itself.
	// This is what makes the fix necessary in the first place, and it must
	// keep failing this way even after the fix -- the fix re-scopes the
	// SESSION before calling, it does not weaken what RLS checks.
	_, _, unscoped := processStore.StartNewRun(ctx, "", "plugin-start-2187", 2,
		json.RawMessage(`{}`), "tenant-a-into-b-key", tenantB, 0)
	if unscoped == nil {
		t.Error("a store scoped to the default tenant wrote a row claiming tenant B via the " +
			"tenantID parameter alone -- RLS's WITH CHECK should refuse this regardless of the fix")
	}
}

// TestStartPluginWorkflow_MySQLIsSingleTenantOnly pins tiers.yaml's D1
// decision at the one place cleat#2187 touches it: a plugin start naming a
// tenant other than the store's own MUST keep failing on MySQL, and must
// fail with the specific mechanism (a foreign-key violation against
// workflow_defs, cleat#1580's tenant_id-scoped FK) rather than by silently
// writing into the wrong tenant's physical database.
//
// MySQL has no per-call re-scoping: isolation there is which PHYSICAL
// DATABASE a connection targets, fixed when the pool was opened, and mutating
// a Go-level tenantID field changes nothing about that. So unlike the
// PostgreSQL and SQL Server tests above, there is no "fix" here to
// demonstrate -- this test exists so a future change that tries to add one
// (e.g. calling MySQLStore.WithTenant before the write, which only changes an
// application-level filter) breaks loudly instead of silently misrouting a
// write into the wrong tenant's data.
func TestStartPluginWorkflow_MySQLIsSingleTenantOnly(t *testing.T) {
	dsn := os.Getenv("CLEAT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping database-backed cleat#2187 test")
	}
	masterDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open master connection: %v", err)
	}
	defer masterDB.Close()
	if err := masterDB.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_MYSQL is set but MySQL is unreachable: %v", err)
	}

	factory := engine.NewMySQLStoreFactory(masterDB, mysqlBaseDSN(dsn))
	ctx := context.Background()

	tenantDefault := engine.DefaultTenantUUID
	tenantB := uuid.New().String()

	defaultDB, err := factory.CreateTenantDatabase(ctx, tenantDefault)
	if err != nil {
		t.Fatalf("create default tenant database: %v", err)
	}
	testutil.SetupMySQLFullSchema(t, defaultDB)

	// Deliberately NOT creating/migrating tenant B's database -- a real
	// deployment never does, per the D1 decision: only the default tenant's
	// database is ever migrated (see startup, and tiers.yaml).
	processStore, closer, err := factory.OpenStore(ctx, tenantDefault, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer closer.Close()
	if err := processStore.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: "plugin-start-2187-mysql", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy def: %v", err)
	}

	// KNOWN-POSITIVE CONTROL: the default tenant's own start still works.
	if _, err := startPluginWorkflow(ctx, processStore, plugin.StartRequest{
		DefName: "plugin-start-2187-mysql", Input: json.RawMessage(`{}`), IdempotencyKey: "default-key", TenantID: tenantDefault,
	}); err != nil {
		t.Fatalf("KNOWN-POSITIVE CONTROL FAILED: default tenant's own plugin start: %v", err)
	}

	_, err = startPluginWorkflow(ctx, processStore, plugin.StartRequest{
		DefName: "plugin-start-2187-mysql", Input: json.RawMessage(`{}`), IdempotencyKey: "tenant-b-key", TenantID: tenantB,
	})
	if err == nil {
		t.Fatal("a plugin start naming a non-default tenant SUCCEEDED on MySQL -- " +
			"tiers.yaml's D1 decision says MySQL is single-tenant only; either the decision changed " +
			"and this test is stale, or something now silently routes cross-tenant")
	}
	t.Logf("plugin start for a non-default tenant correctly refused: %v", err)
}

// TestStartPluginWorkflow_MSSQLScopesTheWriteUnderTheDestinationTenantsSessionContext
// is the SQL Server half, and it answers the question the other two dialects
// don't need to ask: not just "was the row stamped with the right tenant_id"
// (true even unfixed, see cleat#2187's investigation -- MSSQL's security
// policies carry no BLOCK predicate, so nothing today stops an unscoped
// write), but "did the write happen under a SESSION_CONTEXT that matches the
// destination tenant". cleat#2205 will add BLOCK predicates that check
// exactly that, so this test adds one itself -- against a throwaway,
// per-test database, dropped at the end regardless of outcome -- and proves
// the fixed code path already satisfies it, and the unfixed one does not.
func TestStartPluginWorkflow_MSSQLScopesTheWriteUnderTheDestinationTenantsSessionContext(t *testing.T) {
	base := os.Getenv("CLEAT_TEST_MSSQL")
	if base == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping database-backed cleat#2187 test")
	}
	adminDB, err := sql.Open("sqlserver", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer adminDB.Close()
	if err := adminDB.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_MSSQL is set but SQL Server is unreachable at %s: %v",
			redactMSSQLDSN(base), err)
	}

	const dbName = "cleat_plugin_start_2187_test"
	ctx := context.Background()
	if _, err := adminDB.ExecContext(ctx,
		fmt.Sprintf("IF DB_ID('%s') IS NOT NULL BEGIN ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s; END",
			dbName, dbName, dbName)); err != nil {
		t.Fatalf("drop stale test database: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(),
			fmt.Sprintf("ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s",
				dbName, dbName))
	})

	connStr, err := mssqlConnStrForDB(base, dbName)
	if err != nil {
		t.Fatalf("derive test DSN: %v", err)
	}
	testDB, err := sql.Open("sqlserver", connStr)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer testDB.Close()

	// The REAL shipped migrations, not engine/testutil's hand-rolled schema
	// helper -- which defines none of the security policies. See
	// tenant_isolation_mssql_test.go for the full account of why that
	// distinction matters.
	if err := migration.NewRunner(testDB, migration.DialectMSSQL, "../../migrations").Run(ctx); err != nil {
		t.Fatalf("apply mssql migrations: %v", err)
	}

	// Add a real BLOCK PREDICATE AFTER INSERT on workflow_instances --
	// mirroring what cleat#2205 will add for real. The whole database is
	// dropped in t.Cleanup above, so nothing needs to remove this
	// separately.
	if _, err := testDB.ExecContext(ctx, `
		ALTER SECURITY POLICY dbo.TenantFilter_Instances
		ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_instances AFTER INSERT`); err != nil {
		t.Fatalf("add BLOCK PREDICATE: %v", err)
	}

	factory := engine.NewMSSQLStoreFactory(connStr)

	tenantDefault := engine.DefaultTenantUUID
	tenantB := uuid.New().String()

	deployTo := func(tenant string) {
		t.Helper()
		st, closer, err := factory.OpenStore(ctx, tenant, "default")
		if err != nil {
			t.Fatalf("OpenStore(%s): %v", tenant, err)
		}
		defer closer.Close()
		if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: "plugin-start-2187-mssql", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("deploy def for %s: %v", tenant, err)
		}
	}
	deployTo(tenantDefault)
	deployTo(tenantB)

	processStore, processCloser, err := factory.OpenStore(ctx, tenantDefault, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer processCloser.Close()

	// KNOWN-POSITIVE CONTROL: the default tenant's own start must still
	// succeed under the BLOCK PREDICATE -- proves the predicate isn't simply
	// rejecting everything.
	if _, err := startPluginWorkflow(ctx, processStore, plugin.StartRequest{
		DefName: "plugin-start-2187-mssql", Input: json.RawMessage(`{}`), IdempotencyKey: "default-key", TenantID: tenantDefault,
	}); err != nil {
		t.Fatalf("KNOWN-POSITIVE CONTROL FAILED: default tenant's own plugin start, under the BLOCK "+
			"PREDICATE: %v -- the predicate is misconfigured, nothing below is trustworthy", err)
	}

	// THE FIX under test: startPluginWorkflow, called exactly as
	// plugin.Environment.StartWorkflow calls it, for a non-default tenant.
	runID, err := startPluginWorkflow(ctx, processStore, plugin.StartRequest{
		DefName: "plugin-start-2187-mssql", Input: json.RawMessage(`{}`), IdempotencyKey: "tenant-b-key", TenantID: tenantB,
	})
	if err != nil {
		t.Fatalf("startPluginWorkflow for a non-default tenant was REJECTED by the BLOCK PREDICATE: "+
			"%v -- the write did not run under tenant B's own SESSION_CONTEXT", err)
	}
	if runID == "" {
		t.Fatal("startPluginWorkflow for tenant B returned an empty run id")
	}

	// NEGATIVE CONTROL, independent of the fix: StartNewRun called directly
	// on the still-default-scoped process store, claiming tenant B only
	// through the tenantID parameter, must still be refused by the BLOCK
	// PREDICATE -- proving the rejection above is real and not a
	// misconfigured predicate that happens to pass everything.
	_, _, unscoped := processStore.StartNewRun(ctx, "", "plugin-start-2187-mssql", 1,
		json.RawMessage(`{}`), "tenant-a-into-b-key", tenantB, 0)
	if unscoped == nil {
		t.Error("a store scoped to the default tenant wrote a row claiming tenant B via the " +
			"tenantID parameter alone -- the BLOCK PREDICATE should have refused this")
	}
}
