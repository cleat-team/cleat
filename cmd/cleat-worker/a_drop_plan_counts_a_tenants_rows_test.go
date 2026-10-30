package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/internal/tenantdrop"
	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#3552, B3b-1b. The drop plan counts what dropping a tenant would remove, and the number a drop will
// gate on, UNSETTLED RUNS, comes from a different table on each dialect (PostgreSQL keeps a run's status on
// workflow_leases, SQL Server on workflow_instances). So the property worth pinning is not "the plan
// returns something" but "the plan's unsettled count is NON-ZERO for a tenant that has unsettled runs, on
// BOTH dialects": a fixture that never creates a row in the table the dialect reads would let an arm pass
// by counting zero. Each arm therefore starts runs, asserts the count is the number started, then settles
// them and asserts it falls to zero, so the same query is shown to see both states.

func rowsOf(counts []tenantdrop.RowCount, table string) (int64, bool) {
	for _, c := range counts {
		if c.Table == table {
			return c.Rows, true
		}
	}
	return 0, false
}

func mustRows(t *testing.T, plan tenantdrop.Plan, table string) int64 {
	t.Helper()
	n, ok := rowsOf(plan.Tables, table)
	if !ok {
		t.Fatalf("the plan does not list %s at all (it lists %d tables)", table, len(plan.Tables))
	}
	return n
}

func TestTheDropPlanSeesUnsettledRunsOnPostgres(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed cleat#3552 test")
	}
	db := testutil.SuiteTestDB(t, "cleat_worker")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, db)
	factory := engine.NewPostgresStoreFactory(rlsDB, "public")
	ctx := context.Background()

	ts := auth.NewTenantStore(db)
	orgID, err := ts.CreateOrg(ctx, "drop-plan-"+uuid.NewString())
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	mk := func() uuid.UUID {
		id, err := ts.CreateTenant(ctx, "drop-plan-"+uuid.NewString(), "t", orgID)
		if err != nil {
			t.Fatalf("CreateTenant: %v", err)
		}
		return id
	}
	target, other := mk(), mk()

	processStore, pc, err := factory.OpenStore(ctx, engine.DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer pc.Close()
	deploy := func(tenant uuid.UUID) {
		st, c, err := factory.OpenStore(ctx, tenant.String(), "default")
		if err != nil {
			t.Fatalf("OpenStore(%s): %v", tenant, err)
		}
		defer c.Close()
		if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: "drop-plan-def", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("deploy for %s: %v", tenant, err)
		}
	}
	deploy(target)
	deploy(other)
	start := func(tenant uuid.UUID, key string) {
		t.Helper()
		if id, err := startPluginWorkflow(ctx, processStore, plugin.StartRequest{
			DefName: "drop-plan-def", Input: json.RawMessage(`{}`), IdempotencyKey: key, TenantID: tenant.String(),
		}); err != nil || id == "" {
			t.Fatalf("start a run for %s: id=%q err=%v", tenant, id, err)
		}
	}
	start(target, "t1")
	start(target, "t2")
	// A run in ANOTHER tenant. Counted into the target's plan it would make 3, and that is the failure this
	// guards: a count that is not confined to the tenant's own scope.
	start(other, "o1")

	// A registered plugin table, with a row for the target and one for the other tenant, plus a registry row
	// naming a table that does not exist. The suite database is shared, so everything is removed again.
	//
	// plugTable is readable by the tenant role, as a real plugin's table is (its migration grants it).
	// deniedTable is the same but never granted: the plan must list it as uncounted, not as zero and not
	// as a failure of the whole plan.
	const plugTable, ghostTable, deniedTable = "zz3552_plugin_rows", "zz3552_ghost", "zz3552_denied"
	var rlsRole string
	if err := rlsDB.QueryRowContext(ctx, `SELECT current_user`).Scan(&rlsRole); err != nil {
		t.Fatalf("read the RLS role: %v", err)
	}
	for _, tb := range []string{plugTable, deniedTable} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE public.%s (tenant_id uuid NOT NULL, v int NOT NULL DEFAULT 0)`, tb)); err != nil {
			t.Fatalf("create %s: %v", tb, err)
		}
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`GRANT SELECT ON public.%s TO %q`, plugTable, rlsRole)); err != nil {
		t.Fatalf("grant on the plugin table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO admin.plugin_tables (plugin_name, table_name, schema_name, tenant_scoped) VALUES ('zz3552', $1, 'public', true), ('zz3552', $2, 'public', true), ('zz3552', $3, 'public', true)`, plugTable, ghostTable, deniedTable); err != nil {
		t.Fatalf("register the plugin tables: %v", err)
	}
	t.Cleanup(func() {
		// The OTHER tenant's run is still `ready` here, and the suite database is shared. A later test in
		// this package that claims "the next ready workflow" (TestADryRunReportsWithoutDeleting does) claims
		// this one instead of its own, and fails on a fixture that looks correct -- deterministically, 4 runs
		// of 4 on a fresh suite database, when this test ran immediately before it. The target tenant is
		// dropped by the test body; this drops the other, and with it that run.
		_, _ = db.ExecContext(context.Background(), `SELECT admin.drop_tenant($1::uuid, 'public')`, other.String())
		_, _ = db.ExecContext(context.Background(), `DELETE FROM admin.plugin_tables WHERE plugin_name = 'zz3552'`)
		_, _ = db.ExecContext(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS public.%s, public.%s`, plugTable, deniedTable))
	})
	for _, tid := range []uuid.UUID{target, other} {
		for _, tb := range []string{plugTable, deniedTable} {
			if _, err := db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO public.%s (tenant_id) VALUES ($1)`, tb), tid.String()); err != nil {
				t.Fatalf("seed %s: %v", tb, err)
			}
		}
	}

	plan := func() tenantdrop.Plan {
		t.Helper()
		header, err := ts.DropPlanHeader(ctx, target)
		if err != nil {
			t.Fatalf("DropPlanHeader: %v", err)
		}
		facts, err := ts.DropPlanFacts(ctx, target)
		if err != nil {
			t.Fatalf("DropPlanFacts: %v", err)
		}
		st, c, err := factory.OpenStore(ctx, target.String(), "default")
		if err != nil {
			t.Fatalf("OpenStore(target): %v", err)
		}
		defer c.Close()
		p, err := buildDropPlan(ctx, st.(engine.TenantRowCounter), st, target.String(), header, facts)
		if err != nil {
			t.Fatalf("buildDropPlan: %v", err)
		}
		return p
	}

	p := plan()
	if p.UnsettledRuns != 2 {
		t.Fatalf("unsettled_runs = %d; want 2 (two runs started for the target, one for another tenant). "+
			"0 here would mean the count reads a table the fixture never wrote", p.UnsettledRuns)
	}
	if got := mustRows(t, p, "workflow_instances"); got != 2 {
		t.Errorf("workflow_instances = %d; want 2", got)
	}
	if got := mustRows(t, p, "workflow_leases"); got != 2 {
		t.Errorf("workflow_leases = %d; want 2 -- the table the unsettled count reads", got)
	}
	if got := mustRows(t, p, "admin.tenants"); got != 1 {
		t.Errorf("admin.tenants = %d; want 1 (counted on the admin connection)", got)
	}
	// The admin.* tables the tenant's scope may not read must be PRESENT, not silently absent.
	for _, want := range []string{"admin.tenant_api_keys", "admin.tenant_roles", "admin.tenant_egress_allow"} {
		if _, ok := rowsOf(p.Tables, want); !ok {
			t.Errorf("the plan does not list %s", want)
		}
	}
	if got, ok := rowsOf(p.PluginTables, "public."+plugTable); !ok || got != 1 {
		t.Errorf("plugin table %s = %d (listed=%v); want 1 -- the target's row, not the other tenant's too", plugTable, got, ok)
	}
	if len(p.SkippedPluginTables) != 1 || p.SkippedPluginTables[0] != "public."+ghostTable {
		t.Errorf("skipped plugin tables = %v; want [public.%s] (a registry row naming a table that does not exist)", p.SkippedPluginTables, ghostTable)
	}
	if len(p.UncountedPluginTables) != 1 || p.UncountedPluginTables[0] != "public."+deniedTable {
		t.Errorf("uncounted plugin tables = %v; want [public.%s] (registered, exists, not readable by the tenant role)", p.UncountedPluginTables, deniedTable)
	}
	if _, listed := rowsOf(p.PluginTables, "public."+deniedTable); listed {
		t.Errorf("public.%s is listed with a count although it could not be read; an unreadable table must not read as a number", deniedTable)
	}
	total := int64(0)
	for _, c := range p.Tables {
		total += c.Rows
	}
	for _, c := range p.PluginTables {
		total += c.Rows
	}
	if total != p.TotalRows {
		t.Errorf("total_rows = %d; the listed tables sum to %d", p.TotalRows, total)
	}

	// KNOWN-POSITIVE for the other direction: settle the target's runs and the SAME query must read zero
	// while the run rows are all still there.
	if _, err := db.ExecContext(ctx, `UPDATE workflow_leases SET status = 'done' WHERE tenant_id = $1::uuid`, target.String()); err != nil {
		t.Fatalf("settle the runs: %v", err)
	}
	p = plan()
	if p.UnsettledRuns != 0 {
		t.Errorf("unsettled_runs = %d after settling; want 0", p.UnsettledRuns)
	}
	if got := mustRows(t, p, "workflow_instances"); got != 2 {
		t.Errorf("workflow_instances = %d after settling; want 2 (settling deletes nothing)", got)
	}

	// PLANNED AND DROPPED: everything the plan reported with rows is gone after the drop, the registered
	// plugin table included, and the other tenant's row in it is not.
	before := p
	if _, err := db.ExecContext(ctx, `SELECT admin.drop_tenant($1::uuid, 'public')`, target.String()); err != nil {
		t.Fatalf("admin.drop_tenant: %v", err)
	}
	var plugLeft, otherLeft int
	_ = db.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM public.%s WHERE tenant_id = $1::uuid`, plugTable), target.String()).Scan(&plugLeft)
	_ = db.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM public.%s WHERE tenant_id = $1::uuid`, plugTable), other.String()).Scan(&otherLeft)
	if plugLeft != 0 {
		t.Errorf("the registered plugin table still holds %d row(s) for the dropped tenant although the plan listed it", plugLeft)
	}
	if otherLeft != 1 {
		t.Errorf("the other tenant has %d row(s) in the plugin table after the drop; want 1", otherLeft)
	}
	for _, c := range append(append([]tenantdrop.RowCount{}, before.Tables...), before.PluginTables...) {
		if c.Rows == 0 {
			continue
		}
		if c.Table == "admin.tenants" {
			var n int
			_ = db.QueryRowContext(ctx, `SELECT count(*) FROM admin.tenants WHERE tenant_id = $1::uuid`, target.String()).Scan(&n)
			if n != 0 {
				t.Errorf("admin.tenants still has the dropped tenant")
			}
		}
	}
}

func TestTheDropPlanSeesUnsettledRunsOnSQLServer(t *testing.T) {
	base := os.Getenv("CLEAT_TEST_MSSQL")
	if base == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping database-backed cleat#3552 test")
	}
	adminDB, err := sql.Open("sqlserver", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer adminDB.Close()
	if err := adminDB.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_MSSQL is set but SQL Server is unreachable at %s: %v", redactMSSQLDSN(base), err)
	}
	const dbName = "cleat_drop_plan_3552_test"
	ctx := context.Background()
	if _, err := adminDB.ExecContext(ctx,
		fmt.Sprintf("IF DB_ID('%s') IS NOT NULL BEGIN ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s; END", dbName, dbName, dbName)); err != nil {
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
	orgID, err := ts.CreateOrg(ctx, "drop-plan-"+uuid.NewString())
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	mk := func() uuid.UUID {
		id, err := ts.CreateTenant(ctx, "drop-plan-"+uuid.NewString(), "t", orgID)
		if err != nil {
			t.Fatalf("CreateTenant: %v", err)
		}
		return id
	}
	target, other := mk(), mk()

	factory := engine.NewMSSQLStoreFactory(connStr)
	processStore, pc, err := factory.OpenStore(ctx, engine.DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer pc.Close()
	for _, tn := range []uuid.UUID{target, other} {
		st, c, err := factory.OpenStore(ctx, tn.String(), "default")
		if err != nil {
			t.Fatalf("OpenStore(%s): %v", tn, err)
		}
		if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: "drop-plan-def", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("deploy for %s: %v", tn, err)
		}
		c.Close()
	}
	start := func(tenant uuid.UUID, key string) {
		t.Helper()
		if id, err := startPluginWorkflow(ctx, processStore, plugin.StartRequest{
			DefName: "drop-plan-def", Input: json.RawMessage(`{}`), IdempotencyKey: key, TenantID: tenant.String(),
		}); err != nil || id == "" {
			t.Fatalf("start a run for %s: id=%q err=%v", tenant, id, err)
		}
	}
	start(target, "t1")
	start(target, "t2")
	start(other, "o1")

	plan := func() tenantdrop.Plan {
		t.Helper()
		header, err := ts.DropPlanHeader(ctx, target)
		if err != nil {
			t.Fatalf("DropPlanHeader: %v", err)
		}
		facts, err := ts.DropPlanFacts(ctx, target)
		if err != nil {
			t.Fatalf("DropPlanFacts: %v", err)
		}
		st, c, err := factory.OpenStore(ctx, target.String(), "default")
		if err != nil {
			t.Fatalf("OpenStore(target): %v", err)
		}
		defer c.Close()
		p, err := buildDropPlan(ctx, st.(engine.TenantRowCounter), st, target.String(), header, facts)
		if err != nil {
			t.Fatalf("buildDropPlan: %v", err)
		}
		return p
	}

	p := plan()
	if p.UnsettledRuns != 2 {
		t.Fatalf("unsettled_runs = %d; want 2 (two runs started for the target, one for another tenant). "+
			"0 here would mean the count reads a row the fixture never wrote, or reads without the tenant key", p.UnsettledRuns)
	}
	if got := mustRows(t, p, "dbo.workflow_instances"); got != 2 {
		t.Errorf("dbo.workflow_instances = %d; want 2", got)
	}
	// SQL Server counts every table, admin ones included, from the one derived list.
	if got := mustRows(t, p, "admin.tenants"); got != 1 {
		t.Errorf("admin.tenants = %d; want 1", got)
	}
	if _, ok := rowsOf(p.Tables, "admin.tenant_api_keys"); !ok {
		t.Errorf("the derived list does not include admin.tenant_api_keys")
	}

	// The same query, shown to see the other state: settle the target's runs under its own session key.
	conn, err := testDB.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, sql.Named("p1", target.String())); err != nil {
		t.Fatalf("set the session key: %v", err)
	}
	res, err := conn.ExecContext(ctx, `UPDATE workflow_instances SET status = 'done' WHERE tenant_id = @p1`, target.String())
	if err != nil {
		t.Fatalf("settle the runs: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Fatalf("settled %d rows; want 2 -- the fixture could not reach its own rows, so the zero below would mean nothing", n)
	}
	p = plan()
	if p.UnsettledRuns != 0 {
		t.Errorf("unsettled_runs = %d after settling; want 0", p.UnsettledRuns)
	}
	if got := mustRows(t, p, "dbo.workflow_instances"); got != 2 {
		t.Errorf("dbo.workflow_instances = %d after settling; want 2 (settling deletes nothing)", got)
	}
}
