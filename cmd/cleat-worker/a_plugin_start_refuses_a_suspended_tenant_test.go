package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// cleat#3594. A suspended tenant starts nothing, and that has to hold for the
// callers that are not an HTTP request: the scheduler, event-triggers and the job
// queue all reach the engine through startPluginWorkflow. handleStart refused a
// suspended tenant; this path did not, so those three kept creating runs that
// sat `ready` and unclaimed (the rotating claim omits a suspended tenant).

// startPluginAgainst starts a run through startPluginWorkflow on a mock store
// and reports whether the store was asked to create one.
func startPluginAgainst(t *testing.T, st *suspendableStore) (created int, err error) {
	t.Helper()
	st.mockStore.startNewRunFn = func(context.Context, string, string, int, json.RawMessage, string, string, int) (string, bool, error) {
		created++
		return "run-1", false, nil
	}
	_, err = startPluginWorkflow(context.Background(), st, plugin.StartRequest{
		DefName: "d", Input: json.RawMessage(`{}`), IdempotencyKey: "k", TenantID: uuid.NewString(),
	})
	return created, err
}

func TestAPluginStartIsRefusedForASuspendedTenantAndCreatesNothing(t *testing.T) {
	// The control comes first and is the same call with only the flag changed.
	// Without it the refusal below passes against a function that refuses everything.
	t.Run("control: not suspended starts normally", func(t *testing.T) {
		st := &suspendableStore{mockStore: &mockStore{}, suspended: false}
		created, err := startPluginAgainst(t, st)
		if err != nil {
			t.Fatalf("an unsuspended tenant's plugin start failed: %v", err)
		}
		if created != 1 || st.asked != 1 {
			t.Errorf("created=%d asked=%d; want 1 and 1 (one run, and the flag read once)", created, st.asked)
		}
	})

	t.Run("suspended is refused and the store is never asked to create a run", func(t *testing.T) {
		st := &suspendableStore{mockStore: &mockStore{}, suspended: true}
		created, err := startPluginAgainst(t, st)
		if err == nil {
			t.Fatal("a suspended tenant's plugin start succeeded")
		}
		if !strings.Contains(err.Error(), "suspended") {
			t.Errorf("the refusal does not say why: %v", err)
		}
		if created != 0 {
			t.Errorf("StartNewRun was called %d time(s) for a suspended tenant", created)
		}
	})

	t.Run("an unreadable flag is a refusal, not a pass", func(t *testing.T) {
		boom := errors.New("admin.tenants unreachable")
		st := &suspendableStore{mockStore: &mockStore{}, err: boom}
		created, err := startPluginAgainst(t, st)
		if !errors.Is(err, boom) {
			t.Fatalf("got %v; want the read error", err)
		}
		if created != 0 {
			t.Errorf("StartNewRun was called %d time(s) although the flag could not be read", created)
		}
	})
}

// The same property against a real store, where "creates nothing" is a row
// count rather than a mock's call counter, and where the two states are
// reached the way an operator reaches them (SetTenantSuspended).
func TestAPluginStartIsRefusedForASuspendedTenantOnAStoreWithRows(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed cleat#3594 test")
	}
	db := testutil.SuiteTestDB(t, "cleat_worker")
	settleRunsLeftBehind(t, db)
	rlsDB := testutil.OpenPostgresRLSTestDB(t, db)
	factory := engine.NewPostgresStoreFactory(rlsDB, "public")
	ctx := context.Background()

	ts := auth.NewTenantStore(db)
	orgID, err := ts.CreateOrg(ctx, "susp-start-"+uuid.NewString())
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	tid, err := ts.CreateTenant(ctx, "susp-start-"+uuid.NewString(), "t", orgID)
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	tenant := tid.String()

	tenantStore, closer, err := factory.OpenStore(ctx, tenant, "default")
	if err != nil {
		t.Fatalf("OpenStore(tenant): %v", err)
	}
	defer closer.Close()
	if err := tenantStore.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: "plugin-start-3594", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	processStore, pcloser, err := factory.OpenStore(ctx, engine.DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer pcloser.Close()

	// workflow_instances holds one row per run on every dialect; the superuser
	// connection counts them without RLS in the way.
	runs := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_instances WHERE tenant_id = $1::uuid`, tenant).Scan(&n); err != nil {
			t.Fatalf("count runs: %v", err)
		}
		return n
	}
	start := func(key string) (string, error) {
		return startPluginWorkflow(ctx, processStore, plugin.StartRequest{
			DefName: "plugin-start-3594", Input: json.RawMessage(`{}`), IdempotencyKey: key, TenantID: tenant,
		})
	}

	// CONTROL: unsuspended, the start creates exactly one row.
	if id, err := start("before"); err != nil || id == "" {
		t.Fatalf("CONTROL FAILED: an unsuspended tenant's plugin start: id=%q err=%v", id, err)
	}
	if got := runs(); got != 1 {
		t.Fatalf("CONTROL FAILED: %d run rows after one start; want 1", got)
	}

	if err := ts.SetTenantSuspended(ctx, tid, true); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if id, err := start("during"); err == nil {
		t.Fatalf("a suspended tenant's plugin start succeeded: run %q", id)
	}
	if got := runs(); got != 1 {
		t.Errorf("%d run rows after a refused start; want still 1", got)
	}

	// Resuming restores it: the refusal is the flag, not something the first
	// refusal left behind.
	if err := ts.SetTenantSuspended(ctx, tid, false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if id, err := start("after"); err != nil || id == "" {
		t.Fatalf("a resumed tenant's plugin start: id=%q err=%v", id, err)
	}
	if got := runs(); got != 2 {
		t.Errorf("%d run rows after resume and one more start; want 2", got)
	}
}

// The SQL Server half. Multi-tenant SQL Server is a supported configuration, and the
// check is a type assertion on the scoped store, so what this adds over the
// PostgreSQL test is that MSSQLStore.WithTenant returns a store that still answers
// IsTenantSuspended, and that the answer is read from the same admin.tenants row
// SetTenantSuspended writes. It builds its own database from the shipped
// migrations, as TestStartPluginWorkflow_MSSQLScopesTheWriteUnderTheDestinationTenantsSessionContext
// does, because engine/testutil's hand-rolled schema defines none of the security
// policies.
func TestAPluginStartIsRefusedForASuspendedTenantOnSQLServer(t *testing.T) {
	base := os.Getenv("CLEAT_TEST_MSSQL")
	if base == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping database-backed cleat#3594 test")
	}
	adminDB, err := sql.Open("sqlserver", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	// A cleanup and not a defer, and registered BEFORE the one that drops the database: cleanups run last in,
	// first out, and a deferred Close runs when the test function returns, which is before any cleanup, so the
	// DROP DATABASE below would find a closed connection and its error is not read.
	t.Cleanup(func() { _ = adminDB.Close() })
	if err := adminDB.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_MSSQL is set but SQL Server is unreachable at %s: %v", redactMSSQLDSN(base), err)
	}

	const dbName = "cleat_plugin_start_3594_test"
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
			fmt.Sprintf("ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s", dbName, dbName))
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
	if err := migration.NewRunner(testDB, migration.DialectMSSQL, "../../migrations").Run(ctx); err != nil {
		t.Fatalf("apply mssql migrations: %v", err)
	}

	ts, err := auth.NewTenantStoreForDialect(testDB, auth.DialectMSSQL)
	if err != nil {
		t.Fatalf("NewTenantStoreForDialect: %v", err)
	}
	orgID, err := ts.CreateOrg(ctx, "susp-start-"+uuid.NewString())
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	tid, err := ts.CreateTenant(ctx, "susp-start-"+uuid.NewString(), "t", orgID)
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	tenant := tid.String()

	factory := engine.NewMSSQLStoreFactory(connStr)
	tenantStore, closer, err := factory.OpenStore(ctx, tenant, "default")
	if err != nil {
		t.Fatalf("OpenStore(tenant): %v", err)
	}
	defer closer.Close()
	if err := tenantStore.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: "plugin-start-3594-mssql", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	processStore, pcloser, err := factory.OpenStore(ctx, engine.DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer pcloser.Close()

	start := func(key string) (string, error) {
		return startPluginWorkflow(ctx, processStore, plugin.StartRequest{
			DefName: "plugin-start-3594-mssql", Input: json.RawMessage(`{}`), IdempotencyKey: key, TenantID: tenant,
		})
	}
	// Whether a run exists is asked of the tenant's own scoped store by id, not by a
	// count over a table whose row-level policy may filter it for this connection. A
	// refused start returns no id, so "created nothing" is shown by the start
	// returning an error and, after resume, by the SAME key creating a run of its own:
	// had the refusal left a run behind under that key, the key would return it.
	exists := func(runID string) bool {
		t.Helper()
		wf, err := scopeToTenant(processStore, tenant).GetWorkflowByID(ctx, runID)
		if err != nil {
			t.Fatalf("GetWorkflowByID(%s): %v", runID, err)
		}
		return wf != nil
	}

	// CONTROL: unsuspended, the start creates a run the tenant can read back.
	first, err := start("before")
	if err != nil || first == "" {
		t.Fatalf("CONTROL FAILED: an unsuspended tenant's plugin start: id=%q err=%v", first, err)
	}
	if !exists(first) {
		t.Fatalf("CONTROL FAILED: the run %s just started cannot be read back, so this test cannot see a run", first)
	}

	if err := ts.SetTenantSuspended(ctx, tid, true); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if id, err := start("during"); err == nil {
		t.Fatalf("a suspended tenant's plugin start succeeded: run %q", id)
	}

	if err := ts.SetTenantSuspended(ctx, tid, false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// The refused key was never spent: the same key now creates a run, and it is a new one.
	// (A refusal that had created a run would fail at the "during" start above; this
	// half shows resuming restores the tenant, so the refusal was the flag.)
	after, err := start("during")
	if err != nil || after == "" {
		t.Fatalf("a resumed tenant's plugin start with the key that was refused: id=%q err=%v", after, err)
	}
	if after == first || !exists(after) {
		t.Errorf("resumed start returned %q (first run %q, readable=%v); want a new, readable run", after, first, exists(after))
	}
}
