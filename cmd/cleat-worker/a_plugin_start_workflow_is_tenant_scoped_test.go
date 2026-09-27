package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/plugin"
	"github.com/go-sql-driver/mysql"
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

// TestStartPluginWorkflow_PostgresShardedStoreScopesToTheDestinationTenant
// is the sharded-store half. scopeToTenant's *engine.ShardedStore case
// (main.go) calls ShardedStore.WithTenant (sharded_store.go), which is a
// SEPARATE implementation from PostgresStore.WithTenant -- it loops over
// EVERY shard, not just the one a write would route to, because a fan-out
// read like ListVersions merges results from all of them. A single-shard
// test cannot exercise that loop meaningfully; this uses two.
func TestStartPluginWorkflow_PostgresShardedStoreScopesToTheDestinationTenant(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed cleat#2187 test")
	}
	db := testutil.SuiteTestDB(t, "cleat_worker")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, db)
	factory := engine.NewPostgresStoreFactory(rlsDB, "public")
	ctx := context.Background()

	tenantDefault := engine.DefaultTenantUUID
	tenantB := uuid.New().String()

	// Two shards, both backed by the SAME RLS-scoped connection --
	// OpenStore is a cheap struct allocation sharing one *sql.DB (see its
	// own doc comment), so two independent PostgresStore values here cost
	// nothing and are exactly as real as any other *PostgresStore this file
	// uses. What is under test is ShardedStore's own per-shard iteration,
	// which needs more than one shard entry to be exercised at all; a real
	// deployment would point each at a different database, and that
	// difference is invisible to WithTenant's own loop, which only ever
	// sees WorkflowStore values.
	shard0, _, err := factory.OpenStore(ctx, tenantDefault, "default")
	if err != nil {
		t.Fatalf("OpenStore(shard0, default): %v", err)
	}
	shard1, _, err := factory.OpenStore(ctx, tenantDefault, "default")
	if err != nil {
		t.Fatalf("OpenStore(shard1, default): %v", err)
	}
	ss, err := engine.NewShardedStore(
		[]engine.ShardConfig{{Name: "shard-0"}, {Name: "shard-1"}},
		[]engine.WorkflowStore{shard0, shard1},
		[]func() error{func() error { return nil }, func() error { return nil }},
	)
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}

	deployTo := func(tenant string, version int) {
		t.Helper()
		st, closer, err := factory.OpenStore(ctx, tenant, "default")
		if err != nil {
			t.Fatalf("OpenStore(%s): %v", tenant, err)
		}
		defer closer.Close()
		if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: "plugin-start-2187-sharded", Version: version, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("deploy def for %s at v%d: %v", tenant, version, err)
		}
	}
	deployTo(tenantDefault, 1)
	deployTo(tenantB, 2)

	// Idempotency keys are scoped to (tenant_id, key_hash), not to def name,
	// and this suite's tests share ONE database (testutil.SuiteTestDB) and
	// the SAME tenantDefault constant -- so a key already spent above by
	// the non-sharded test for a different def name would collide here with
	// ErrIdempotencyKeyDefMismatch. Suffixed to keep every key in this file
	// unique per test.

	// KNOWN-POSITIVE CONTROL, through the sharded store.
	defaultRunID, err := startPluginWorkflow(ctx, ss, plugin.StartRequest{
		DefName: "plugin-start-2187-sharded", Input: json.RawMessage(`{}`), IdempotencyKey: "default-key-sharded", TenantID: tenantDefault,
	})
	if err != nil {
		t.Fatalf("KNOWN-POSITIVE CONTROL FAILED: default tenant's own plugin start on a sharded "+
			"store: %v", err)
	}
	if defaultRunID == "" {
		t.Fatal("default tenant's plugin start on a sharded store returned an empty run id")
	}

	// THE FIX under test: a plugin start naming tenant B, through the SAME
	// sharded store still opened for the default tenant on every shard.
	runID, err := startPluginWorkflow(ctx, ss, plugin.StartRequest{
		DefName: "plugin-start-2187-sharded", Input: json.RawMessage(`{}`), IdempotencyKey: "tenant-b-key-sharded", TenantID: tenantB,
	})
	if err != nil {
		t.Fatalf("startPluginWorkflow for a non-default tenant on a sharded store failed: %v -- "+
			"cleat#2187 unfixed for ShardedStore.WithTenant", err)
	}
	if runID == "" {
		t.Fatal("startPluginWorkflow for tenant B on a sharded store returned an empty run id")
	}

	// Correctly stamped, correctly versioned, AND invisible to the default
	// tenant -- the three properties cleat-review confirmed live before
	// asking for this to be a permanent test.
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
		t.Fatal("tenant B cannot see the run started for it on a sharded store -- not stamped to tenant B")
	}
	if wf.DefVersion != 2 {
		t.Errorf("run started for tenant B on a sharded store used def version %d, want 2 -- "+
			"ListVersions answered from the wrong tenant's scope on at least one shard", wf.DefVersion)
	}

	storeDefaultOnly, closerDefault, err := factory.OpenStore(ctx, tenantDefault, "default")
	if err != nil {
		t.Fatalf("OpenStore(default, read-only check): %v", err)
	}
	defer closerDefault.Close()
	wfFromDefault, err := storeDefaultOnly.GetWorkflowByID(ctx, runID)
	if err != nil {
		t.Fatalf("default-tenant-scoped store reading tenant B's run: %v", err)
	}
	if wfFromDefault != nil {
		t.Error("tenant B's run, started on a sharded store, is visible to a store scoped to the " +
			"default tenant -- RLS is not isolating it on at least one shard")
	}

	// NEGATIVE CONTROL, independent of the fix: StartNewRun called directly
	// on the sharded store (still default-scoped on every shard), claiming
	// tenant B only through the tenantID parameter, must still be refused
	// by RLS -- whichever shard the generated run id happens to route to.
	//
	// A key unique to THIS test, not reused from the non-sharded test above:
	// idempotency keys are scoped to (tenant_id, key_hash), not to def name,
	// and both tests share tenantDefault and one database, so a reused key
	// would be rejected with ErrIdempotencyKeyDefMismatch instead of the
	// RLS violation this control means to prove -- passing the `err != nil`
	// check below for the wrong reason.
	_, _, unscoped := ss.StartNewRun(ctx, "", "plugin-start-2187-sharded", 2,
		json.RawMessage(`{}`), "tenant-a-into-b-key-sharded", tenantB, 0)
	if unscoped == nil {
		t.Error("a sharded store with every shard scoped to the default tenant wrote a row " +
			"claiming tenant B via the tenantID parameter alone -- RLS's WITH CHECK should refuse " +
			"this regardless of the fix")
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
	// The SPECIFIC mechanism, not just that something failed: err == nil
	// alone would also pass if this failed for an unrelated reason (a typo'd
	// DSN, a schema mismatch) that happens to reject every write, which
	// would prove nothing about D1 isolation. 1452 is MySQL's foreign-key
	// violation code; fk_instances_def (migrations/mysql/034) is the
	// specific constraint -- tenant B has no workflow_defs row for this def
	// name because its database was never created, which is what D1 single-
	// tenancy actually is on this dialect.
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1452 {
		t.Fatalf("plugin start for a non-default tenant failed, but not with MySQL error 1452 "+
			"(foreign key violation, fk_instances_def): %v -- this must be D1's mechanism, not an "+
			"unrelated failure that happens to also reject the write", err)
	}
	t.Logf("plugin start for a non-default tenant correctly refused by fk_instances_def: %v", err)
}

// TestStartPluginWorkflow_MSSQLScopesTheWriteUnderTheDestinationTenantsSessionContext
// is the SQL Server half, and it answers the question the other two dialects
// don't need to ask: not just "was the row stamped with the right tenant_id"
// (true even unfixed, see cleat#2187's investigation -- before cleat#2205,
// MSSQL's security policies carried no BLOCK predicate, so nothing stopped an
// unscoped write), but "did the write happen under a SESSION_CONTEXT that
// matches the destination tenant". cleat#2205 (migration 103) added BLOCK
// predicates that check exactly that, applied here the same way a real
// deployment gets them -- by the real Runner, below -- against a throwaway,
// per-test database, dropped at the end regardless of outcome. This proves
// the fixed code path satisfies the real predicate, and the unfixed one does
// not.
//
// UNTIL cleat#2205 landed, this test ADDED that BLOCK PREDICATE ITSELF, by
// hand, to simulate a migration that did not exist yet. It no longer does:
// migration 103 is one of the migrations migration.NewRunner.Run applies
// below, so a second, manual ALTER SECURITY POLICY ... ADD BLOCK PREDICATE
// now collides with the one the Runner already added --
// "A BLOCK predicate for the same operation has already been defined ...
// (33262)" -- which is the right failure for a test whose own simulation has
// been overtaken by the real thing landing.
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

	// migration 103 (cleat#2205), applied above by the real Runner, already
	// added the AFTER INSERT BLOCK PREDICATE this test needs on
	// dbo.workflow_instances -- see the file-level comment on why this used
	// to add one itself and no longer does.

	factory := engine.NewMSSQLStoreFactory(connStr)

	tenantDefault := engine.DefaultTenantUUID
	tenantB := uuid.New().String()

	// DIFFERENT versions per tenant, as the PostgreSQL test above does: with
	// both tenants at the same version, ListVersions running unscoped (on
	// the default tenant's own session) would still happen to answer the
	// same version number, and only the BLOCK PREDICATE below would catch
	// a regression -- leaving ListVersions itself unfalsifiable here (a
	// mutation cleat-review found survives every test in an earlier
	// revision of this file: reverting MSSQLStore.ListVersions to a plain,
	// unscoped s.db query passed regardless).
	deployTo := func(tenant string, version int) {
		t.Helper()
		st, closer, err := factory.OpenStore(ctx, tenant, "default")
		if err != nil {
			t.Fatalf("OpenStore(%s): %v", tenant, err)
		}
		defer closer.Close()
		if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: "plugin-start-2187-mssql", Version: version, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("deploy def for %s at v%d: %v", tenant, version, err)
		}
	}
	deployTo(tenantDefault, 1)
	deployTo(tenantB, 2)

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
	//
	// The Fatalf below states what we WANT (success, under tenant B's own
	// SESSION_CONTEXT), not a diagnosis of why a failure happened: any
	// error here fails the test, but only the negative control below
	// confirms the BLOCK PREDICATE is what would have caught a real
	// regression, so this message must not claim that's the cause without
	// having checked.
	runID, err := startPluginWorkflow(ctx, processStore, plugin.StartRequest{
		DefName: "plugin-start-2187-mssql", Input: json.RawMessage(`{}`), IdempotencyKey: "tenant-b-key", TenantID: tenantB,
	})
	if err != nil {
		t.Fatalf("startPluginWorkflow for a non-default tenant failed: %v -- want it to succeed "+
			"under tenant B's own SESSION_CONTEXT, which the BLOCK PREDICATE permits", err)
	}
	if runID == "" {
		t.Fatal("startPluginWorkflow for tenant B returned an empty run id")
	}

	// GetWorkflowByID, not just the fact that StartNewRun succeeded -- and
	// through scopeToTenant(processStore, tenantB), NOT a freshly opened
	// factory store. MSSQLStoreFactory bakes SESSION_CONTEXT into the POOL
	// itself at connect time (getOrCreateTenantPool's tenantSessionConnector),
	// so a store the factory opens fresh for tenant B is correctly scoped
	// regardless of what GetWorkflowByID's own query looks like -- it would
	// read the right row even with cleat#2204's bug still in place, and
	// prove nothing. scopeToTenant is what startPluginWorkflow itself calls,
	// and what this assertion needs to go through: before cleat#2204, a
	// plain, non-transactional query on a store re-scoped this way never
	// asserted its own tenantID, so it ran under the ORIGINAL (default
	// tenant) pool's baked-in SESSION_CONTEXT -- reverting GetWorkflowByID
	// alone (leaving StartNewRun's own fix intact) passed every earlier
	// version of this test, and this exact read got a silent nil instead of
	// the row that had just been written.
	scopedToB := scopeToTenant(processStore, tenantB)
	wf, err := scopedToB.GetWorkflowByID(ctx, runID)
	if err != nil {
		t.Fatalf("tenant B reading back its own run via GetWorkflowByID: %v", err)
	}
	if wf == nil {
		t.Fatal("GetWorkflowByID(B's own run, scoped to B via scopeToTenant) returned nil -- cleat#2204 unfixed")
	}
	if wf.DefVersion != 2 {
		t.Errorf("run started for tenant B used def version %d, want 2 (tenant B's own deployed "+
			"version) -- ListVersions answered from the wrong tenant's scope", wf.DefVersion)
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
