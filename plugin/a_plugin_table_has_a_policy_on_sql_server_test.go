package plugin

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"

	// testutil.TestDB opens "sqlserver://...", and without this blank import
	// the package's test binary reports `unknown driver "sqlserver"` as
	// "configured mssql database is unreachable" -- naming the network rather
	// than the missing import.
	_ "github.com/microsoft/go-mssqldb"
)

// The SQL Server half of cleat#1277's plugin table isolation. cleat#1552.
//
// Driven through RunMigrations rather than by calling applyTenantScoping
// directly, because the defect that made this worth a test was NOT in the
// scoping function: it was in the runner deciding whether to call it at all.
// See TestADeclarationOnlyMigrationStillInstallsItsPolicyOnSQLServer.

const (
	msTableA = "cleat_1552_probe_a"
	msTableB = "cleat_1552_probe_b"
	msPlugin = "cleat-1552-probe"
)

const (
	msTenantA = "aaaaaaaa-1552-0000-0000-000000000001"
	msTenantB = "bbbbbbbb-1552-0000-0000-000000000002"
)

// mssqlScopedFixture migrates two tables through the real runner and returns a
// pool. Both tables are created by version 1; version 2 declares the SECOND one
// and carries no SQL at all.
func mssqlScopedFixture(t *testing.T) *sql.DB {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectMSSQL)
	ctx := context.Background()

	drop := []string{
		`IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'` + msTableA + `_tenant_isolation') DROP SECURITY POLICY ` + msTableA + `_tenant_isolation`,
		`IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'` + msTableB + `_tenant_isolation') DROP SECURITY POLICY ` + msTableB + `_tenant_isolation`,
		`IF OBJECT_ID('dbo.` + msTableA + `','U') IS NOT NULL DROP TABLE dbo.` + msTableA,
		`IF OBJECT_ID('dbo.` + msTableB + `','U') IS NOT NULL DROP TABLE dbo.` + msTableB,
	}
	clean := func() {
		for _, s := range drop {
			if _, err := db.ExecContext(context.Background(), s); err != nil {
				t.Logf("fixture teardown %q: %v", s, err)
			}
		}
		_, _ = db.ExecContext(context.Background(),
			`IF OBJECT_ID('plugin_migrations','U') IS NOT NULL DELETE FROM plugin_migrations WHERE plugin_name = @p1`, msPlugin)
	}
	clean()
	t.Cleanup(clean)

	p := loaded(msPlugin,
		Migration{
			Version: 1,
			UpMSSQL: `CREATE TABLE ` + msTableA + ` (tenant_id UNIQUEIDENTIFIER NOT NULL, k NVARCHAR(50) NOT NULL, PRIMARY KEY (tenant_id, k));
				CREATE TABLE ` + msTableB + ` (tenant_id UNIQUEIDENTIFIER NOT NULL, k NVARCHAR(50) NOT NULL, PRIMARY KEY (tenant_id, k));`,
			TenantScoped: []string{msTableA},
		},
		// THE cleat#1512 IDIOM: a later version that declares scoping for a
		// table an earlier version created, carrying no SQL at all. A recorded
		// migration never runs again, so an existing database can only be
		// protected by a NEW version, and that version has nothing to execute.
		Migration{
			Version:      2,
			TenantScoped: []string{msTableB},
		},
	)
	if err := RunMigrations(ctx, db, DialectMSSQL, nil, []*LoadedPlugin{p}); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	// PRECONDITION, not decoration: if the tables are absent every assertion
	// below reports success against nothing.
	for _, table := range []string{msTableA, msTableB} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sys.tables WHERE name = @p1`, table).Scan(&n); err != nil {
			t.Fatalf("precondition: %v", err)
		}
		if n != 1 {
			t.Fatalf("PRECONDITION FAILED: %s was not created, so nothing below means anything", table)
		}
	}

	// SEEDED PER TENANT, each row written by the tenant that owns it.
	//
	// The obvious fixture sets the cross_tenant key once and inserts
	// everything -- and it makes TestACrossTenantSweepSeesEveryTenantsPluginRows
	// unfalsifiable, because removing the sweep key from the predicate then
	// breaks the SEED and the test fails before reaching its assertion. A
	// fixture must not depend on the mechanism one of its tests is measuring.
	rows := []struct{ tenant, k string }{
		{msTenantA, "a1"}, {msTenantA, "a2"}, {msTenantB, "b1"},
	}
	for _, table := range []string{msTableA, msTableB} {
		for _, r := range rows {
			if err := withSessionKeys(t, db, map[string]string{"tenant_id": r.tenant}, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(context.Background(),
					`INSERT INTO `+table+` (tenant_id, k) VALUES (@p1, @p2)`, r.tenant, r.k)
				return err
			}); err != nil {
				t.Fatalf("seed %s/%s: %v", table, r.k, err)
			}
		}
	}
	return db
}

// withSessionKeys runs fn inside a transaction carrying the given
// SESSION_CONTEXT keys, the way engine.beginTenantTx does at runtime.
func withSessionKeys(t *testing.T, db *sql.DB, keys map[string]string, fn func(*sql.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for k, v := range keys {
		if _, err := tx.ExecContext(ctx,
			`EXEC sp_set_session_context @key = @p1, @value = @p2`, k, v); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func mssqlCountAs(t *testing.T, db *sql.DB, keys map[string]string, table string) int {
	t.Helper()
	var n int
	if err := withSessionKeys(t, db, keys, func(tx *sql.Tx) error {
		return tx.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&n)
	}); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestAPluginTableIsFilteredToItsTenantOnSQLServer is the behaviour the whole
// change exists for.
func TestAPluginTableIsFilteredToItsTenantOnSQLServer(t *testing.T) {
	db := mssqlScopedFixture(t)

	for _, table := range []string{msTableA, msTableB} {
		t.Run(table, func(t *testing.T) {
			if got := mssqlCountAs(t, db, map[string]string{"tenant_id": msTenantA}, table); got != 2 {
				t.Errorf("tenant A saw %d rows, want 2", got)
			}
			if got := mssqlCountAs(t, db, map[string]string{"tenant_id": msTenantB}, table); got != 1 {
				t.Errorf("tenant B saw %d rows, want 1", got)
			}
			// No tenant reads as an empty table rather than raising. That is
			// the documented asymmetry with PostgreSQL's assert_tenant_set(),
			// asserted here so that it is a decision on the record rather than
			// something a reader has to discover. cleat#1552.
			if got := mssqlCountAs(t, db, nil, table); got != 0 {
				t.Errorf("with no tenant set the table showed %d rows, want 0", got)
			}
		})
	}
}

// TestADeclarationOnlyMigrationStillInstallsItsPolicyOnSQLServer is the
// regression test for the defect this change found.
//
// The runner skips a migration with no SQL for the current dialect, records the
// version as applied, and continues BEFORE the scoping call. The cleat#1512
// idiom -- declare TenantScoped in a new version with an empty Up -- therefore
// installed nothing on SQL Server while marking itself done, so the table never
// got another chance.
//
// Measured on a freshly migrated database before the fix: 25 declared tables,
// 2 policies. The other 23 came from 17 declaration-only migrations.
func TestADeclarationOnlyMigrationStillInstallsItsPolicyOnSQLServer(t *testing.T) {
	db := mssqlScopedFixture(t)
	ctx := context.Background()

	for _, table := range []string{msTableA, msTableB} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sys.security_policies WHERE name = @p1`,
			table+"_tenant_isolation").Scan(&n); err != nil {
			t.Fatalf("read sys.security_policies: %v", err)
		}
		if n != 1 {
			t.Errorf("%s has %d security policies, want 1", table, n)
		}
	}

	// And the predicates, because a policy with only a filter is the shape
	// that lets a tenant-less INSERT through. Three: one FILTER, two BLOCK.
	var preds int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sys.security_predicates sp
		JOIN sys.security_policies p ON p.object_id = sp.object_id
		WHERE p.name = @p1`, msTableB+"_tenant_isolation").Scan(&preds); err != nil {
		t.Fatalf("read sys.security_predicates: %v", err)
	}
	if preds != 3 {
		t.Errorf("%s carries %d predicates, want 3 (one FILTER, two BLOCK)", msTableB, preds)
	}
}

// TestAWriteOutsideItsTenantIsRefusedOnSQLServer covers the half a FILTER
// predicate does not.
//
// A filter predicate hides rows from reads and says nothing about writes.
// Measured on a table carrying only a filter: an INSERT with no session context
// SUCCEEDS and the row is then invisible to the writer. PostgreSQL has no such
// hole, because FOR ALL ... USING defaults its WITH CHECK to the USING
// expression -- measured as a NOSUPERUSER NOBYPASSRLS role, since a superuser
// bypasses row-level security unconditionally and reports every write as
// permitted.
func TestAWriteOutsideItsTenantIsRefusedOnSQLServer(t *testing.T) {
	db := mssqlScopedFixture(t)

	// The control: a write INSIDE the tenant is allowed, so a blanket refusal
	// would not pass this test.
	if err := withSessionKeys(t, db, map[string]string{"tenant_id": msTenantA}, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(),
			`INSERT INTO `+msTableA+` (tenant_id, k) VALUES ('`+msTenantA+`','a3')`)
		return err
	}); err != nil {
		t.Fatalf("a tenant writing its OWN row was refused: %v", err)
	}

	for _, tc := range []struct {
		name string
		keys map[string]string
		stmt string
	}{
		{
			"insert naming another tenant",
			map[string]string{"tenant_id": msTenantA},
			`INSERT INTO ` + msTableA + ` (tenant_id, k) VALUES ('` + msTenantB + `','b9')`,
		},
		{
			"insert with no tenant at all",
			nil,
			`INSERT INTO ` + msTableA + ` (tenant_id, k) VALUES ('` + msTenantB + `','b8')`,
		},
		{
			"update moving a row to another tenant",
			map[string]string{"tenant_id": msTenantA},
			`UPDATE ` + msTableA + ` SET tenant_id = '` + msTenantB + `' WHERE k = 'a1'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := withSessionKeys(t, db, tc.keys, func(tx *sql.Tx) error {
				_, e := tx.ExecContext(context.Background(), tc.stmt)
				return e
			})
			if err == nil {
				t.Fatal("the write was allowed. A FILTER predicate alone does not refuse " +
					"writes -- the policy needs ADD BLOCK PREDICATE ... AFTER INSERT and " +
					"AFTER UPDATE, or SQL Server accepts rows the writer cannot read back")
			}
			if !strings.Contains(err.Error(), "block predicate") {
				t.Fatalf("refused, but not by the block predicate: %v", err)
			}
		})
	}
}

// TestACrossTenantSweepSeesEveryTenantsPluginRows is the other side: the
// bypass has to work, or the fifteen plugins that declare AcrossAllTenants
// start returning nothing, silently.
func TestACrossTenantSweepSeesEveryTenantsPluginRows(t *testing.T) {
	db := mssqlScopedFixture(t)
	keys := map[string]string{"cross_tenant": "test: a sweep that names itself"}
	if got := mssqlCountAs(t, db, keys, msTableA); got != 3 {
		t.Errorf("a cross-tenant sweep saw %d rows, want all 3", got)
	}
	// And it grants no tenant: the engine's own predicate reads tenant_id and
	// nothing else, so this key cannot widen any of the thirteen core tables.
	var tid sql.NullString
	if err := withSessionKeys(t, db, keys, func(tx *sql.Tx) error {
		return tx.QueryRowContext(context.Background(),
			`SELECT CAST(SESSION_CONTEXT(N'tenant_id') AS NVARCHAR(64))`).Scan(&tid)
	}); err != nil {
		t.Fatalf("read tenant_id key: %v", err)
	}
	if tid.Valid {
		t.Errorf("a cross-tenant sweep set tenant_id = %q; it must set no tenant at all", tid.String)
	}
}

// TestApplyingTenantScopingTwiceIsANoOpOnSQLServer covers what RunMigrations
// cannot: it records a version and never re-runs it, so a second RunMigrations
// exercises the RECORD, not the DDL. A database restored from a backup, or a
// migration re-applied by hand, does exercise the DDL.
func TestApplyingTenantScopingTwiceIsANoOpOnSQLServer(t *testing.T) {
	db := mssqlScopedFixture(t)
	ctx := context.Background()
	if err := applyTenantScoping(ctx, db.ExecContext, DialectMSSQL, []string{msTableA, msTableB}); err != nil {
		t.Fatalf("re-applying the scoping failed: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sys.security_policies WHERE name IN (@p1, @p2)`,
		msTableA+"_tenant_isolation", msTableB+"_tenant_isolation").Scan(&n); err != nil {
		t.Fatalf("count policies: %v", err)
	}
	if n != 2 {
		t.Errorf("after re-applying there are %d policies, want 2", n)
	}
}

// TestTheSQLServerScopingStatementsSayWhatTheyMustSay asserts the DDL text.
//
// WHY A TEXT ASSERTION when there are behavioural tests above. Because the
// behavioural tests CANNOT falsify the predicate body, and I found that out by
// trying: mutating the function to drop its cross-tenant disjunct left every
// test above green.
//
// The reason is the guarded creation -- `IF OBJECT_ID(...) IS NULL` -- which is
// deliberate and correct, because a SECURITY POLICY holds a hard dependency on
// the function under it and CREATE OR ALTER fails while any policy references
// it. The consequence is that once the function exists in a database, a changed
// definition is never applied. On the test database that meant the mutation was
// never installed; 225 security predicates already bound the existing function,
// so no fixture can drop it either.
//
// THE SAME FACT IS AN OPERATIONAL LIMIT, and it belongs in the open: there is no
// upgrade path for this predicate. A database that has it keeps the version it
// first got. Changing it would need a migration that drops every plugin policy,
// alters the function and recreates them -- the shape migrations/mssql/001
// already uses for the engine's own predicate, and which cannot be written from
// inside a single plugin's migration because it does not know the others.
func TestTheSQLServerScopingStatementsSayWhatTheyMustSay(t *testing.T) {
	var emitted []string
	record := func(ctx context.Context, q string, args ...any) (sql.Result, error) {
		emitted = append(emitted, q)
		return nil, nil
	}
	if err := applyTenantScoping(context.Background(), record, DialectMSSQL, []string{"probe_table"}); err != nil {
		t.Fatalf("applyTenantScoping: %v", err)
	}
	if len(emitted) != 2 {
		t.Fatalf("emitted %d statements, want 2 (the function, then one policy)", len(emitted))
	}
	fn, policy := emitted[0], emitted[1]

	for _, want := range []struct{ what, substr, why string }{
		{"the tenant test", `SESSION_CONTEXT(N''tenant_id'')`,
			"without it the predicate admits nothing and every plugin read is empty"},
		{"the sweep bypass", `SESSION_CONTEXT(N''cross_tenant'')`,
			"without it the fifteen plugins that declare AcrossAllTenants silently see no rows"},
		{"a guarded create", `IF OBJECT_ID('dbo.fn_plugin_tenant_filter', 'IF') IS NULL`,
			"CREATE OR ALTER fails once any policy binds the function"},
	} {
		if !strings.Contains(fn, want.substr) {
			t.Errorf("the filter function is missing %s (%q): %s", want.what, want.substr, want.why)
		}
	}
	if strings.Contains(fn, "IS_ROLEMEMBER") {
		t.Error("the plugin predicate carries an IS_ROLEMEMBER disjunct. #1491 measured what that " +
			"costs -- a query with no tenant equality of its own loses its seek -- and cleat#1541 is " +
			"removing it from the engine. Plugin tables must not adopt it on the way past")
	}

	for _, want := range []struct{ what, substr, why string }{
		{"a filter predicate", "ADD FILTER PREDICATE dbo.fn_plugin_tenant_filter(tenant_id) ON dbo.probe_table",
			"reads would be unscoped"},
		{"an insert block", "ADD BLOCK PREDICATE dbo.fn_plugin_tenant_filter(tenant_id) ON dbo.probe_table AFTER INSERT",
			"a FILTER predicate does not refuse writes: measured, an INSERT with no session context succeeds and is then invisible to its writer"},
		{"an update block", "ADD BLOCK PREDICATE dbo.fn_plugin_tenant_filter(tenant_id) ON dbo.probe_table AFTER UPDATE",
			"a row could be moved to another tenant"},
		{"a guarded create", "IF NOT EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'probe_table_tenant_isolation')",
			"re-applying a migration by hand would fail on the second run"},
	} {
		if !strings.Contains(policy, want.substr) {
			t.Errorf("the policy is missing %s: %s\n  wanted substring: %s", want.what, want.why, want.substr)
		}
	}
	// Two-part qualified, measured: `ON probe_table` fails with "Cannot schema
	// bind security policy ... Names must be in two-part format".
	if strings.Contains(policy, "ON probe_table") {
		t.Error("the policy names the table unqualified; SQL Server requires two-part format there")
	}
}
