package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// SQL Server had no admin.drop_tenant, so `cleatctl drop-tenant` refused on
// this dialect and a tenant could not be deleted at all. cleat#1635, and
// migrations/mssql/074_a_dropped_tenants_rows_go_with_it.sql is the fix.
//
// The PostgreSQL sibling is engine/a_dropped_tenants_plugin_rows_go_with_it_test.go
// and the shape of the proof is the same -- seed two tenants, drop one, and
// carry three numbers rather than one, because a table that was never seeded
// and a table that was correctly emptied both count zero (cleat#1265).
//
// WHAT DOES NOT CARRY OVER IS THE OBSERVER. That test counts through a
// superuser connection, where PostgreSQL's RLS is bypassed unconditionally, so
// a policy that merely HIDES a row cannot make it pass. SQL Server has no
// such principal: a security policy applies to sysadmin, db_owner and dbo
// alike (measured in migrations/mssql/012_admin_role.sql, and again here --
// see TestADeleteWithoutTheTenantKeyRemovesNothingOnSQLServer below, where
// sa reads IS_SRVROLEMEMBER('sysadmin') = 1 and still sees nothing).
//
// So every count here is taken on a connection with BOTH session keys set:
// the counted tenant's own tenant_id, which is what dbo.fn_tenant_filter
// admits on core tables, and cross_tenant, which is what
// dbo.fn_plugin_tenant_filter admits on plugin tables. That is the widest
// view of a given tenant's rows this dialect offers, so a row that is not
// counted is a row that is not there.

type mssqlDropTenantProbePlugin struct{}

func (mssqlDropTenantProbePlugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{Name: "mssql-drop-tenant-probe", Version: "1"}
}

func (mssqlDropTenantProbePlugin) Init(context.Context, *plugin.Environment) error { return nil }

func (mssqlDropTenantProbePlugin) Migrations() []plugin.Migration {
	return []plugin.Migration{{
		Version: 1,
		// Declared on both dialects because Migration.Up is required reading
		// for the PostgreSQL arm even on a database that will never see it;
		// only UpMSSQL is executed here.
		Up: `CREATE TABLE IF NOT EXISTS mssql_drop_tenant_probe (
			tenant_id UUID NOT NULL,
			k TEXT NOT NULL,
			PRIMARY KEY (tenant_id, k))`,
		UpMSSQL: `IF OBJECT_ID('dbo.mssql_drop_tenant_probe', 'U') IS NULL
			CREATE TABLE dbo.mssql_drop_tenant_probe (
				tenant_id UNIQUEIDENTIFIER NOT NULL,
				k NVARCHAR(100) NOT NULL,
				CONSTRAINT pk_mssql_drop_tenant_probe PRIMARY KEY (tenant_id, k))`,
		// The whole point: this is what earns the table the security policy
		// whose predicate a naive DELETE cannot get past.
		TenantScoped: []string{"mssql_drop_tenant_probe"},
	}}
}

// mssqlDropTenantDB brings up the shipped schema and the probe plugin's table.
func mssqlDropTenantDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}
	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	ctx := context.Background()

	// The bookkeeping row outlives the table: drop the table alone and
	// RunMigrations sees version 1 as applied, skips it, and every assertion
	// runs against a table that does not exist. The PostgreSQL sibling was
	// caught exactly that way.
	//
	// The security policy has to go with the table, and first: a policy is
	// schema-bound to it, so DROP TABLE fails while the policy stands.
	for _, stmt := range []string{
		`IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'mssql_drop_tenant_probe_tenant_isolation')
			DROP SECURITY POLICY mssql_drop_tenant_probe_tenant_isolation`,
		`DROP TABLE IF EXISTS dbo.mssql_drop_tenant_probe`,
		`IF OBJECT_ID('dbo.plugin_migrations', 'U') IS NOT NULL
			DELETE FROM dbo.plugin_migrations WHERE plugin_name = 'mssql-drop-tenant-probe'`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("reset probe plugin: %v\n%s", err, stmt)
		}
	}
	if err := plugin.RunMigrations(ctx, db, plugin.DialectMSSQL, nil,
		[]*plugin.LoadedPlugin{{Plugin: mssqlDropTenantProbePlugin{}, Healthy: true}}); err != nil {
		t.Fatalf("run plugin migrations: %v", err)
	}

	// And take it away again, which is not tidiness.
	//
	// TestEveryShippedTenantPolicyExistsInTheBuiltDatabase compares the
	// policies the migrations bind against the policies the database actually
	// has, in both directions, and a probe table left behind fails it:
	//
	//   the database has a tenant filter predicate on [mssql_drop_tenant_probe],
	//   which no migration binds one to
	//
	// That guard is right to fail. A tested schema carrying protection the
	// shipped schema does not have is how a real gap becomes invisible, and it
	// caught this on the first full-suite run of this file.
	//
	// Registered here, before the per-test row cleanups, so it runs LAST --
	// t.Cleanup is LIFO, and dropping the table out from under a cleanup that
	// deletes rows from it would turn a teardown into a failure.
	t.Cleanup(func() {
		for _, stmt := range []string{
			`IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'mssql_drop_tenant_probe_tenant_isolation')
				DROP SECURITY POLICY mssql_drop_tenant_probe_tenant_isolation`,
			`DROP TABLE IF EXISTS dbo.mssql_drop_tenant_probe`,
			`IF OBJECT_ID('dbo.plugin_migrations', 'U') IS NOT NULL
				DELETE FROM dbo.plugin_migrations WHERE plugin_name = 'mssql-drop-tenant-probe'`,
		} {
			if _, err := db.ExecContext(context.Background(), stmt); err != nil {
				t.Logf("teardown: %v\n%s", err, stmt)
			}
		}
	})
	return db, ctx
}

// mssqlTenantView pins a connection and points both security-policy keys at
// one tenant, so everything that tenant owns is visible on it.
//
// A *sql.Conn and not the pool: sp_set_session_context is per-connection, and
// go-mssqldb resets session state when a connection returns to the pool, so a
// key set through *sql.DB is set on whichever connection happened to serve
// that statement and gone by the next one.
func mssqlTenantView(t *testing.T, ctx context.Context, db *sql.DB, tenant string) *sql.Conn {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("open connection: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	for _, kv := range [][2]string{
		{"tenant_id", tenant},
		{"cross_tenant", "engine test: counting one tenant's rows across core and plugin tables"},
	} {
		if _, err := conn.ExecContext(ctx,
			`EXEC sp_set_session_context @key = @p1, @value = @p2`, kv[0], kv[1]); err != nil {
			t.Fatalf("set session key %s: %v", kv[0], err)
		}
	}
	return conn
}

// mssqlTenantOwnedTables is the universe: every table carrying a tenant_id
// column.
//
// Derived rather than listed, and that IS the assertion. admin.drop_tenant on
// PostgreSQL deletes from a hand-maintained list, and that list has drifted
// three times -- tenant_settings, workflow_defs (cleat#1201), and
// workflow_memory_{stats,samples} (cleat#1644, open). A test written from the
// same list as the code under test cannot see what both of them missed; this
// one reads a different source and can disagree.
func mssqlTenantOwnedTables(t *testing.T, ctx context.Context, q *sql.Conn) []string {
	t.Helper()
	rows, err := q.QueryContext(ctx, `
		SELECT s.name, t.name
		FROM sys.tables t
		JOIN sys.schemas s ON s.schema_id = t.schema_id
		WHERE EXISTS (SELECT 1 FROM sys.columns c
		              WHERE c.object_id = t.object_id AND c.name = 'tenant_id')`)
	if err != nil {
		t.Fatalf("enumerate tenant-owned tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			t.Fatalf("scan tenant-owned table: %v", err)
		}
		out = append(out, schema+"."+name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("enumerate tenant-owned tables: %v", err)
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("no table in this database carries a tenant_id column, so every count below " +
			"would be zero for a reason that has nothing to do with drop_tenant")
	}
	return out
}

func mssqlCountIn(t *testing.T, ctx context.Context, q *sql.Conn, table, tenant string) int {
	t.Helper()
	var n int
	// The table name is from sys.tables, not from a request; bracketed so a
	// name needing quotes is a quoted identifier rather than a syntax error.
	stmt := fmt.Sprintf("SELECT count(*) FROM [%s].[%s] WHERE tenant_id = @p1",
		splitSchema(table), splitName(table))
	if err := q.QueryRowContext(ctx, stmt, tenant).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func splitSchema(qualified string) string {
	for i := 0; i < len(qualified); i++ {
		if qualified[i] == '.' {
			return qualified[:i]
		}
	}
	return "dbo"
}

func splitName(qualified string) string {
	for i := 0; i < len(qualified); i++ {
		if qualified[i] == '.' {
			return qualified[i+1:]
		}
	}
	return qualified
}

// The defect this whole migration exists for, pinned so it cannot quietly
// change: a filter predicate hides rows from DELETE exactly as it hides them
// from SELECT, so the cleanup an operator would type by hand reports success
// and removes nothing.
//
// This is a characterisation test, not a wish. It asserts what SQL Server
// does, and it is here because "(0 rows affected)" is the one outcome that is
// indistinguishable from "already clean" -- a reader who does not know this
// will conclude the rows are gone. If a future SQL Server makes that DELETE
// raise instead, this test goes red and the header of migration 074 needs
// rewriting; that is the intended signal, not a regression.
func TestADeleteWithoutTheTenantKeyRemovesNothingOnSQLServer(t *testing.T) {
	db, ctx := mssqlDropTenantDB(t)
	const tenant = "02d51635-0000-4000-8000-00000000f001"

	seed := mssqlTenantView(t, ctx, db, tenant)
	if _, err := seed.ExecContext(ctx,
		`INSERT INTO dbo.mssql_drop_tenant_probe (tenant_id, k) VALUES (@p1, 'secret')`, tenant); err != nil {
		t.Fatalf("seed probe row: %v", err)
	}
	t.Cleanup(func() {
		c := mssqlTenantView(t, ctx, db, tenant)
		c.ExecContext(ctx, `DELETE FROM dbo.mssql_drop_tenant_probe WHERE tenant_id = @p1`, tenant)
	})
	if got := mssqlCountIn(t, ctx, seed, "dbo.mssql_drop_tenant_probe", tenant); got != 1 {
		t.Fatalf("PRECONDITION FAILED: seeded %d rows, want 1 -- everything below would be "+
			"measuring an empty table", got)
	}

	// A separate connection with no session keys at all: the shape an operator
	// gets from sqlcmd or any client that has not been told about
	// sp_set_session_context.
	bare, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("open bare connection: %v", err)
	}
	defer bare.Close()

	// The control that makes the result mean something. Without it, "the
	// DELETE removed nothing" has a second explanation -- an unprivileged
	// connection -- and this one is the privileged case.
	var sysadmin int
	if err := bare.QueryRowContext(ctx, `SELECT IS_SRVROLEMEMBER('sysadmin')`).Scan(&sysadmin); err != nil {
		t.Fatalf("read sysadmin membership: %v", err)
	}
	if sysadmin != 1 {
		// Fatal, not Skip. CLEAT_TEST_MSSQL is sa in CI and in the default this
		// repo ships, so this is always satisfiable here -- and a skip would
		// hide the one thing this test exists to show: that PRIVILEGE is not
		// what gets you past the predicate. A connection that is merely
		// unprivileged would delete nothing for an ordinary reason, so a green
		// run under one would be meaningless. scripts/check-skips.sh case (c).
		t.Fatalf("this connection reads IS_SRVROLEMEMBER('sysadmin') = %d, want 1. "+
			"Point CLEAT_TEST_MSSQL at a sysadmin login (sa, as CI does): the point of "+
			"this test is that even sysadmin is subject to the security policy, and an "+
			"unprivileged connection cannot demonstrate that", sysadmin)
	}

	res, err := bare.ExecContext(ctx,
		`DELETE FROM dbo.mssql_drop_tenant_probe WHERE tenant_id = @p1`, tenant)
	if err != nil {
		t.Fatalf("the hand-typed DELETE raised rather than silently doing nothing -- which would "+
			"be an improvement, but migration 074's header says otherwise and needs updating: %v", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected: %v", err)
	}
	if affected != 0 {
		t.Errorf("a DELETE issued with no tenant key affected %d rows, want 0. If the filter "+
			"predicate no longer hides rows from DELETE, the trap migration 074 documents is "+
			"gone and its header is wrong", affected)
	}
	if got := mssqlCountIn(t, ctx, seed, "dbo.mssql_drop_tenant_probe", tenant); got != 1 {
		t.Errorf("after a DELETE that reported 0 rows affected, the row count is %d, want 1 -- "+
			"the two readings disagree, so one of them is lying", got)
	}
}

// The fix. Three numbers, following migration 066's own convention:
// the victim's rows are gone, the bystander's are not, and the victim's
// admin.tenants row is gone so the procedure is known to have run.
func TestATenantCanBeDroppedOnSQLServer(t *testing.T) {
	db, ctx := mssqlDropTenantDB(t)
	const (
		victim    = "02d51635-0000-4000-8000-0000000012a9"
		bystander = "02d51635-0000-4000-8000-0000000012b9"
	)

	for _, tn := range []string{victim, bystander} {
		if _, err := db.ExecContext(ctx,
			`IF NOT EXISTS (SELECT 1 FROM admin.tenants WHERE tenant_id = @p1)
			 INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`,
			tn, "mssql-drop-probe-"+tn[len(tn)-4:]); err != nil {
			t.Fatalf("seed tenant %s: %v", tn, err)
		}
		conn := mssqlTenantView(t, ctx, db, tn)
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO dbo.mssql_drop_tenant_probe (tenant_id, k) VALUES (@p1, 'secret')`, tn); err != nil {
			t.Fatalf("seed plugin row for %s: %v", tn, err)
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO dbo.workflow_memory_stats (tenant_id, def_name, mean_bytes, sample_count)
			 VALUES (@p1, 'mssql_drop_tenant_probe_def', 1048576, 1)`, tn); err != nil {
			t.Fatalf("seed workflow_memory_stats for %s: %v", tn, err)
		}
	}
	// The bystander is never dropped, so its rows have to be removed here or
	// the next run collides on the primary key.
	t.Cleanup(func() {
		c := mssqlTenantView(t, ctx, db, bystander)
		for _, stmt := range []string{
			`DELETE FROM dbo.mssql_drop_tenant_probe WHERE tenant_id = @p1`,
			`DELETE FROM dbo.workflow_memory_stats WHERE tenant_id = @p1`,
			`DELETE FROM admin.tenants WHERE tenant_id = @p1`,
		} {
			c.ExecContext(ctx, stmt, bystander)
		}
	})

	victimView := mssqlTenantView(t, ctx, db, victim)
	bystanderView := mssqlTenantView(t, ctx, db, bystander)

	// A table that was never seeded and a table that was correctly emptied
	// both count zero afterwards, so the seed is checked before anything is
	// deleted. cleat#1265 published "4 of 4 clean" off a run where two of six
	// seeds had failed, for exactly this reason.
	for _, probe := range []struct {
		view   *sql.Conn
		tenant string
		label  string
	}{{victimView, victim, "victim"}, {bystanderView, bystander, "bystander"}} {
		for _, table := range []string{"dbo.mssql_drop_tenant_probe", "dbo.workflow_memory_stats"} {
			if got := mssqlCountIn(t, ctx, probe.view, table, probe.tenant); got != 1 {
				t.Fatalf("PRECONDITION FAILED: the %s has %d rows in %s before the drop, want 1 -- "+
					"a zero count afterwards would mean nothing", probe.label, got, table)
			}
		}
	}

	if _, err := db.ExecContext(ctx, `EXEC admin.drop_tenant @tenant_id = @p1`, victim); err != nil {
		t.Fatalf("admin.drop_tenant(victim): %v", err)
	}

	// Did it run at all? A procedure that silently did nothing leaves the same
	// surviving rows and reads as the same bug.
	var tenantRow int
	if err := victimView.QueryRowContext(ctx,
		`SELECT count(*) FROM admin.tenants WHERE tenant_id = @p1`, victim).Scan(&tenantRow); err != nil {
		t.Fatalf("count admin.tenants: %v", err)
	}
	if tenantRow != 0 {
		t.Fatalf("PRECONDITION FAILED: admin.drop_tenant left the victim's admin.tenants row behind, "+
			"so it did not run to completion and every count below says nothing (count=%d)", tenantRow)
	}

	// The universe, read from sys.columns rather than from a list this test
	// shares with the procedure.
	var survived []string
	for _, table := range mssqlTenantOwnedTables(t, ctx, victimView) {
		if got := mssqlCountIn(t, ctx, victimView, table, victim); got != 0 {
			survived = append(survived, fmt.Sprintf("%s (%d rows)", table, got))
		}
	}
	if len(survived) > 0 {
		t.Errorf("admin.drop_tenant left the dropped tenant's rows in %v.\n\n"+
			"Every table listed carries a tenant_id column, so it holds rows owned by one "+
			"tenant, and migration 074's sweep is supposed to reach all of them. A table "+
			"appearing here is either a new one the sweep's predicate does not match or an "+
			"ordering failure -- check whether it has a foreign key, in which case it belongs "+
			"in the explicit list at the top of the procedure rather than in the sweep.", survived)
	}

	// And the other direction, which is the half that stops a fix becoming
	// "delete everything".
	for _, table := range []string{"dbo.mssql_drop_tenant_probe", "dbo.workflow_memory_stats"} {
		if got := mssqlCountIn(t, ctx, bystanderView, table, bystander); got != 1 {
			t.Errorf("dropping one tenant removed the bystander's rows in %s (count=%d, want 1)", table, got)
		}
	}
	var bystanderRow int
	if err := bystanderView.QueryRowContext(ctx,
		`SELECT count(*) FROM admin.tenants WHERE tenant_id = @p1`, bystander).Scan(&bystanderRow); err != nil {
		t.Fatalf("count admin.tenants for the bystander: %v", err)
	}
	if bystanderRow != 1 {
		t.Errorf("dropping one tenant removed the bystander's admin.tenants row")
	}
}

func TestTheSQLServerDropRefusesTheDefaultTenantAndLeavesTheSessionAlone(t *testing.T) {
	db, ctx := mssqlDropTenantDB(t)

	// Set a key first, so the restore path has something to restore that is
	// not NULL -- a test that starts from NULL cannot tell "restored" from
	// "never touched".
	const marker = "02d51635-0000-4000-8000-00000000ffff"
	conn := mssqlTenantView(t, ctx, db, marker)

	if _, err := conn.ExecContext(ctx,
		`EXEC admin.drop_tenant @tenant_id = @p1`, DefaultTenantUUID); err == nil {
		t.Fatal("admin.drop_tenant accepted the default tenant. Every single-tenant deployment " +
			"writes under it, so this must refuse")
	}

	var after string
	if err := conn.QueryRowContext(ctx,
		`SELECT CAST(SESSION_CONTEXT(N'tenant_id') AS NVARCHAR(4000))`).Scan(&after); err != nil {
		t.Fatalf("read session key: %v", err)
	}
	if after != marker {
		t.Errorf("after a refused drop the session tenant key is %q, want %q. "+
			"sp_set_session_context is not transactional, so a procedure that fails "+
			"part-way leaves the caller's connection pointed at whatever it last set",
			after, marker)
	}
}

// Dropping a tenant whose admin.tenants row is already gone is the state
// cleat#1635 describes -- deleted by hand, data left behind -- and it is the
// one the procedure most needs to work in. A "no such tenant" guard would
// refuse exactly the cleanup it exists to perform.
func TestTheSQLServerDropCleansUpAfterATenantRowDeletedByHand(t *testing.T) {
	db, ctx := mssqlDropTenantDB(t)
	const orphan = "02d51635-0000-4000-8000-00000000e001"

	view := mssqlTenantView(t, ctx, db, orphan)
	if _, err := view.ExecContext(ctx,
		`INSERT INTO dbo.mssql_drop_tenant_probe (tenant_id, k) VALUES (@p1, 'orphaned')`, orphan); err != nil {
		t.Fatalf("seed orphaned plugin row: %v", err)
	}
	t.Cleanup(func() {
		c := mssqlTenantView(t, ctx, db, orphan)
		c.ExecContext(ctx, `DELETE FROM dbo.mssql_drop_tenant_probe WHERE tenant_id = @p1`, orphan)
	})
	if got := mssqlCountIn(t, ctx, view, "dbo.mssql_drop_tenant_probe", orphan); got != 1 {
		t.Fatalf("PRECONDITION FAILED: seeded %d rows, want 1", got)
	}
	// No admin.tenants row was ever created for this tenant, which is the
	// end state of the hand-deletion the issue describes.

	if _, err := db.ExecContext(ctx, `EXEC admin.drop_tenant @tenant_id = @p1`, orphan); err != nil {
		t.Fatalf("admin.drop_tenant refused a tenant with no admin.tenants row, which is the "+
			"state it most needs to work in: %v", err)
	}
	if got := mssqlCountIn(t, ctx, view, "dbo.mssql_drop_tenant_probe", orphan); got != 0 {
		t.Errorf("the orphaned rows survived (count=%d, want 0)", got)
	}
}
