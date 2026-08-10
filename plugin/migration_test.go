package plugin

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Fake driver for testing RunMigrations / RegisterPluginTables without a real DB
// ---------------------------------------------------------------------------

type migrationTestDriver struct{}

func (d *migrationTestDriver) Open(name string) (driver.Conn, error) {
	return &migrationTestConn{}, nil
}

type migrationTestConnector struct {
	conn driver.Conn
	drv  driver.Driver
}

func (c *migrationTestConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return c.conn, nil
}

func (c *migrationTestConnector) Driver() driver.Driver { return c.drv }

type migrationTestResult struct {
	rowsAffected int64
}

func (r *migrationTestResult) LastInsertId() (int64, error) { return 0, nil }
func (r *migrationTestResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

type execCall struct {
	query string
}

type migrationTestConn struct {
	execCalls  []execCall
	lockCalls  []string // pg_advisory_lock / pg_advisory_unlock statements
	queryCalls []string
	execErr    error
	queryErr   error
	beginErr   error

	// execFailAfter: if > 0, exec calls succeed until this many have
	// completed; the next call returns execErr. Used to defer errors past
	// the CREATE TABLE statement.
	execFailAfter int
	execCallCount int

	// For QueryRowContext — the bool returned by EXISTS checks.
	existsResult bool

	// Transaction tracking.
	txActive    bool
	txExecCalls []execCall
	txCommitErr error
	txRollback  bool
}

func (c *migrationTestConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("Prepare not implemented in test driver")
}

func (c *migrationTestConn) Close() error { return nil }

func (c *migrationTestConn) Begin() (driver.Tx, error) {
	if c.beginErr != nil {
		return nil, c.beginErr
	}
	c.txActive = true
	return &migrationTestTx{conn: c}, nil
}

// BeginTx implements driver.ConnBeginTx.
func (c *migrationTestConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Begin()
}

// ExecContext implements driver.ExecerContext.
func (c *migrationTestConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	// Session setup -- the advisory lock and the search_path pin -- is
	// transport for RunMigrations, not one of the migration statements the
	// assertions below are about. Recording it in execCalls would shift every
	// index and count in this file and say nothing extra; it is tracked
	// separately instead, and TestRunMigrations_PostgresSessionSetup asserts
	// on it.
	if strings.Contains(query, "pg_advisory_") || strings.Contains(query, "search_path") {
		c.lockCalls = append(c.lockCalls, query)
		return &migrationTestResult{rowsAffected: 1}, nil
	}
	if c.execFailAfter > 0 {
		if c.execCallCount >= c.execFailAfter && c.execErr != nil {
			return nil, c.execErr
		}
		c.execCallCount++
	} else if c.execErr != nil {
		return nil, c.execErr
	}
	if c.txActive {
		c.txExecCalls = append(c.txExecCalls, execCall{query: query})
	} else {
		c.execCalls = append(c.execCalls, execCall{query: query})
	}
	return &migrationTestResult{rowsAffected: 1}, nil
}

// QueryContext implements driver.QueryerContext.
func (c *migrationTestConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.queryCalls = append(c.queryCalls, query)
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	return &singleBoolRow{value: c.existsResult}, nil
}

type singleBoolRow struct {
	value bool
	done  bool
}

func (r *singleBoolRow) Columns() []string { return []string{"exists"} }
func (r *singleBoolRow) Close() error      { return nil }
func (r *singleBoolRow) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}

type migrationTestTx struct {
	conn *migrationTestConn
}

func (tx *migrationTestTx) Commit() error {
	tx.conn.txActive = false
	if tx.conn.txCommitErr != nil {
		return tx.conn.txCommitErr
	}
	return nil
}

func (tx *migrationTestTx) Rollback() error {
	tx.conn.txRollback = true
	tx.conn.txActive = false
	return nil
}

func newTestMigrationDB(t *testing.T) (*sql.DB, *migrationTestConn) {
	t.Helper()
	conn := &migrationTestConn{}
	drv := &migrationTestDriver{}
	db := sql.OpenDB(&migrationTestConnector{conn: conn, drv: drv})
	t.Cleanup(func() { db.Close() })
	return db, conn
}

// ---------------------------------------------------------------------------
// splitStatements tests
// ---------------------------------------------------------------------------

func TestSplitStatements_Empty(t *testing.T) {
	stmts := splitStatements("")
	if len(stmts) != 0 {
		t.Errorf("expected 0 statements, got %d", len(stmts))
	}
}

func TestSplitStatements_Single(t *testing.T) {
	stmts := splitStatements("SELECT 1")
	if len(stmts) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(stmts))
	}
	if stmts[0] != "SELECT 1" {
		t.Errorf("got %q, want 'SELECT 1'", stmts[0])
	}
}

func TestSplitStatements_Multiple(t *testing.T) {
	stmts := splitStatements("SELECT 1; SELECT 2; SELECT 3")
	if len(stmts) != 3 {
		t.Fatalf("expected 3 statements, got %d", len(stmts))
	}
	if stmts[0] != "SELECT 1" {
		t.Errorf("stmt[0] = %q", stmts[0])
	}
	if stmts[1] != "SELECT 2" {
		t.Errorf("stmt[1] = %q", stmts[1])
	}
	if stmts[2] != "SELECT 3" {
		t.Errorf("stmt[2] = %q", stmts[2])
	}
}

func TestSplitStatements_TrailingSemicolons(t *testing.T) {
	stmts := splitStatements("SELECT 1;;;")
	if len(stmts) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(stmts))
	}
	if stmts[0] != "SELECT 1" {
		t.Errorf("got %q", stmts[0])
	}
}

func TestSplitStatements_LeadingSemicolons(t *testing.T) {
	stmts := splitStatements(";;;SELECT 1")
	if len(stmts) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(stmts))
	}
	if stmts[0] != "SELECT 1" {
		t.Errorf("got %q", stmts[0])
	}
}

func TestSplitStatements_WhitespaceFragments(t *testing.T) {
	stmts := splitStatements("  ; \t ; \n ")
	if len(stmts) != 0 {
		t.Errorf("expected 0 statements from whitespace-only fragments, got %d", len(stmts))
	}
}

func TestSplitStatements_KeepInteriorWhitespace(t *testing.T) {
	stmts := splitStatements("SELECT\n  1; INSERT\n  INTO t VALUES (1)")
	if len(stmts) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(stmts))
	}
	if !strings.Contains(stmts[0], "SELECT") {
		t.Errorf("first stmt should contain SELECT: %q", stmts[0])
	}
	if !strings.Contains(stmts[1], "INSERT") {
		t.Errorf("second stmt should contain INSERT: %q", stmts[1])
	}
}

func TestSplitStatements_WhitespaceAroundStatements(t *testing.T) {
	stmts := splitStatements("  SELECT 1  ;  INSERT 2  ")
	if len(stmts) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(stmts))
	}
	if stmts[0] != "SELECT 1" {
		t.Errorf("stmt[0] = %q", stmts[0])
	}
	if stmts[1] != "INSERT 2" {
		t.Errorf("stmt[1] = %q", stmts[1])
	}
}

// ---------------------------------------------------------------------------
// createPluginMigrationsTableSQL tests
// ---------------------------------------------------------------------------

func TestCreatePluginMigrationsTableSQL_Postgres(t *testing.T) {
	sql := createPluginMigrationsTableSQL(DialectPostgres)
	if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS plugin_migrations") {
		t.Error("expected CREATE TABLE IF NOT EXISTS")
	}
	if !strings.Contains(sql, "TIMESTAMPTZ") {
		t.Error("expected TIMESTAMPTZ for Postgres")
	}
}

func TestCreatePluginMigrationsTableSQL_MySQL(t *testing.T) {
	sql := createPluginMigrationsTableSQL(DialectMySQL)
	if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS plugin_migrations") {
		t.Error("expected CREATE TABLE IF NOT EXISTS")
	}
	if !strings.Contains(sql, "VARCHAR(255)") {
		t.Error("expected VARCHAR(255) for MySQL")
	}
	if !strings.Contains(sql, "TIMESTAMP(6)") {
		t.Error("expected TIMESTAMP(6) for MySQL")
	}
}

func TestCreatePluginMigrationsTableSQL_MSSQL(t *testing.T) {
	sql := createPluginMigrationsTableSQL(DialectMSSQL)
	if !strings.Contains(sql, "IF NOT EXISTS (SELECT 1 FROM sys.tables") {
		t.Error("expected sys.tables check for MSSQL")
	}
	if !strings.Contains(sql, "NVARCHAR(255)") {
		t.Error("expected NVARCHAR(255) for MSSQL")
	}
	if !strings.Contains(sql, "DATETIMEOFFSET") {
		t.Error("expected DATETIMEOFFSET for MSSQL")
	}
}

func TestCreatePluginMigrationsTableSQL_Unknown(t *testing.T) {
	sql := createPluginMigrationsTableSQL("unknown-dialect")
	if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS plugin_migrations") {
		t.Error("expected default Postgres-style SQL for unknown dialect")
	}
	if !strings.Contains(sql, "TIMESTAMPTZ") {
		t.Error("expected TIMESTAMPTZ for default")
	}
}

// ---------------------------------------------------------------------------
// checkPluginMigrationSQL tests
// ---------------------------------------------------------------------------

func TestCheckPluginMigrationSQL_Postgres(t *testing.T) {
	sql := checkPluginMigrationSQL(DialectPostgres)
	if !strings.Contains(sql, "$1") && !strings.Contains(sql, "$2") {
		t.Error("expected PostgreSQL positional params $1, $2")
	}
}

func TestCheckPluginMigrationSQL_MySQL(t *testing.T) {
	sql := checkPluginMigrationSQL(DialectMySQL)
	if !strings.Contains(sql, "?") {
		t.Error("expected MySQL ? placeholders")
	}
}

func TestCheckPluginMigrationSQL_MSSQL(t *testing.T) {
	sql := checkPluginMigrationSQL(DialectMSSQL)
	if !strings.Contains(sql, "@p1") && !strings.Contains(sql, "@p2") {
		t.Error("expected MSSQL @p1, @p2 placeholders")
	}
}

func TestCheckPluginMigrationSQL_Unknown(t *testing.T) {
	sql := checkPluginMigrationSQL("unknown-dialect")
	if !strings.Contains(sql, "$1") {
		t.Error("expected default PostgreSQL $1 placeholder for unknown dialect")
	}
}

// ---------------------------------------------------------------------------
// insertPluginMigrationSQL tests
// ---------------------------------------------------------------------------

func TestInsertPluginMigrationSQL_Postgres(t *testing.T) {
	sql := insertPluginMigrationSQL(DialectPostgres)
	if !strings.Contains(sql, "$1") && !strings.Contains(sql, "$2") {
		t.Error("expected PostgreSQL positional params $1, $2")
	}
}

func TestInsertPluginMigrationSQL_MySQL(t *testing.T) {
	sql := insertPluginMigrationSQL(DialectMySQL)
	if !strings.Contains(sql, "?") {
		t.Error("expected MySQL ? placeholders")
	}
}

func TestInsertPluginMigrationSQL_MSSQL(t *testing.T) {
	sql := insertPluginMigrationSQL(DialectMSSQL)
	if !strings.Contains(sql, "@p1") && !strings.Contains(sql, "@p2") {
		t.Error("expected MSSQL @p1, @p2 placeholders")
	}
}

func TestInsertPluginMigrationSQL_Unknown(t *testing.T) {
	sql := insertPluginMigrationSQL("unknown-dialect")
	if !strings.Contains(sql, "$1") {
		t.Error("expected default PostgreSQL $1 placeholder for unknown dialect")
	}
}

// ---------------------------------------------------------------------------
// execSQLStatements tests
// ---------------------------------------------------------------------------

func TestExecSQLStatements_HappyPath(t *testing.T) {
	var executed []string
	execFn := func(ctx context.Context, query string, args ...any) (sql.Result, error) {
		executed = append(executed, query)
		return nil, nil
	}

	err := execSQLStatements(context.Background(), execFn, "SELECT 1; SELECT 2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(executed) != 2 {
		t.Fatalf("expected 2 exec calls, got %d", len(executed))
	}
	if executed[0] != "SELECT 1" {
		t.Errorf("first = %q", executed[0])
	}
	if executed[1] != "SELECT 2" {
		t.Errorf("second = %q", executed[1])
	}
}

func TestExecSQLStatements_Error(t *testing.T) {
	wantErr := errors.New("boom")
	execFn := func(ctx context.Context, query string, args ...any) (sql.Result, error) {
		return nil, wantErr
	}

	err := execSQLStatements(context.Background(), execFn, "SELECT 1; SELECT 2")
	if err != wantErr {
		t.Errorf("got %v, want %v", err, wantErr)
	}
}

func TestExecSQLStatements_ErrorOnSecond(t *testing.T) {
	var executed []string
	wantErr := errors.New("boom on second")
	execFn := func(ctx context.Context, query string, args ...any) (sql.Result, error) {
		executed = append(executed, query)
		if len(executed) >= 2 {
			return nil, wantErr
		}
		return nil, nil
	}

	err := execSQLStatements(context.Background(), execFn, "A; B; C")
	if err != wantErr {
		t.Errorf("got %v, want %v", err, wantErr)
	}
	if len(executed) != 2 {
		t.Errorf("expected 2 execs before error, got %d", len(executed))
	}
}

func TestExecSQLStatements_EmptySQL(t *testing.T) {
	called := false
	execFn := func(ctx context.Context, query string, args ...any) (sql.Result, error) {
		called = true
		return nil, nil
	}

	err := execSQLStatements(context.Background(), execFn, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("exec should not be called for empty SQL")
	}
}

func TestExecSQLStatements_OnlySemicolons(t *testing.T) {
	called := false
	execFn := func(ctx context.Context, query string, args ...any) (sql.Result, error) {
		called = true
		return nil, nil
	}

	err := execSQLStatements(context.Background(), execFn, ";;;")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("exec should not be called for semicolon-only SQL")
	}
}

// ---------------------------------------------------------------------------
// RegisterPluginTables tests
// ---------------------------------------------------------------------------

func TestRegisterPluginTables_SingleTable(t *testing.T) {
	db, conn := newTestMigrationDB(t)

	err := RegisterPluginTables(context.Background(), db, "my-plugin", []string{"my_table"})
	if err != nil {
		t.Fatalf("RegisterPluginTables failed: %v", err)
	}
	if len(conn.execCalls) != 1 {
		t.Fatalf("expected 1 exec call, got %d", len(conn.execCalls))
	}
	if !strings.Contains(conn.execCalls[0].query, "INSERT INTO admin.plugin_tables") {
		t.Error("expected INSERT INTO admin.plugin_tables")
	}
	if !strings.Contains(conn.execCalls[0].query, "ON CONFLICT DO NOTHING") {
		t.Error("expected ON CONFLICT DO NOTHING")
	}
}

func TestRegisterPluginTables_MultipleTables(t *testing.T) {
	db, conn := newTestMigrationDB(t)

	err := RegisterPluginTables(context.Background(), db, "my-plugin", []string{"t1", "t2", "t3"})
	if err != nil {
		t.Fatalf("RegisterPluginTables failed: %v", err)
	}
	if len(conn.execCalls) != 3 {
		t.Fatalf("expected 3 exec calls, got %d", len(conn.execCalls))
	}
}

func TestRegisterPluginTables_EmptyList(t *testing.T) {
	db, conn := newTestMigrationDB(t)

	err := RegisterPluginTables(context.Background(), db, "my-plugin", nil)
	if err != nil {
		t.Fatalf("RegisterPluginTables failed: %v", err)
	}
	if len(conn.execCalls) != 0 {
		t.Errorf("expected 0 exec calls for empty list, got %d", len(conn.execCalls))
	}
}

func TestRegisterPluginTables_DBError(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.execErr = errors.New("connection refused")

	err := RegisterPluginTables(context.Background(), db, "my-plugin", []string{"t1"})
	if err == nil {
		t.Error("expected error from DB failure")
	}
	if !strings.Contains(err.Error(), "register plugin table") {
		t.Errorf("error should mention 'register plugin table', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// RunMigrations tests
// ---------------------------------------------------------------------------

func TestRunMigrations_NilDB(t *testing.T) {
	err := RunMigrations(context.Background(), nil, DialectPostgres, nil, nil)
	if err != nil {
		t.Fatalf("expected nil error for nil db, got: %v", err)
	}
}

func TestRunMigrations_CreatesMigrationTable(t *testing.T) {
	db, conn := newTestMigrationDB(t)

	err := RunMigrations(context.Background(), db, DialectPostgres, nil, nil)
	if err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	// First call should be the CREATE TABLE.
	if len(conn.execCalls) < 1 {
		t.Fatal("expected at least 1 exec call (create table)")
	}
	if !strings.Contains(conn.execCalls[0].query, "CREATE TABLE IF NOT EXISTS plugin_migrations") {
		t.Errorf("first exec should be CREATE TABLE, got: %s", conn.execCalls[0].query)
	}
}

// TestRunMigrations_PostgresSessionSetup pins the two things RunMigrations
// must do to its connection before applying anything, both of which were
// missing and both of which broke real deployments (see
// pluginMigrationSession):
//
//   - an advisory lock, because four workers start at once and killed each
//     other's migrations;
//   - search_path = public, because plugin DDL is unqualified and otherwise
//     landed in a schema named after the connecting role.
//
// Neither may be sent to MySQL or SQL Server, which have neither.
func TestRunMigrations_PostgresSessionSetup(t *testing.T) {
	for _, tc := range []struct {
		dialect Dialect
		wantPG  bool
	}{
		{DialectPostgres, true},
		{DialectMySQL, false},
		{DialectMSSQL, false},
	} {
		t.Run(string(tc.dialect), func(t *testing.T) {
			db, conn := newTestMigrationDB(t)
			if err := RunMigrations(context.Background(), db, tc.dialect, nil, nil); err != nil {
				t.Fatalf("RunMigrations: %v", err)
			}
			joined := strings.Join(conn.lockCalls, " | ")
			if !tc.wantPG {
				if len(conn.lockCalls) != 0 {
					t.Errorf("%s: sent PostgreSQL-only session setup: %s", tc.dialect, joined)
				}
				return
			}
			for _, want := range []string{
				"pg_advisory_lock",
				"SET search_path = public",
				"RESET search_path",
				"pg_advisory_unlock",
			} {
				if !strings.Contains(joined, want) {
					t.Errorf("session setup missing %q; got: %s", want, joined)
				}
			}
			// Released as well as taken: a lock held past the run, or a
			// search_path left set on a pooled connection, outlives the run
			// and affects unrelated queries.
			if !strings.Contains(conn.lockCalls[0], "pg_advisory_lock") {
				t.Errorf("lock must be taken first, got: %s", joined)
			}
		})
	}
}

func TestRunMigrations_RunsCoreMigrations(t *testing.T) {
	db, conn := newTestMigrationDB(t)

	coreMigrations := []Migration{
		{Version: 1, Up: "CREATE TABLE core_a (id INT)"},
		{Version: 2, Up: "CREATE TABLE core_b (id INT)"},
	}
	err := RunMigrations(context.Background(), db, DialectPostgres, coreMigrations, nil)
	if err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	// execCalls: [create_migrations_table, core_m1, core_m2]
	if len(conn.execCalls) != 3 {
		t.Fatalf("expected 3 exec calls, got %d", len(conn.execCalls))
	}
}

func TestRunMigrations_CoreMigrationError(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	// Let the CREATE TABLE succeed (exec 0), then fail on the core migration (exec 1).
	conn.execFailAfter = 1
	conn.execErr = fmt.Errorf("syntax error")

	coreMigrations := []Migration{
		{Version: 1, Up: "CREATE TABLE ok (id INT)"},
	}
	err := RunMigrations(context.Background(), db, DialectPostgres, coreMigrations, nil)
	if err == nil {
		t.Error("expected error from core migration failure")
	}
	if !strings.Contains(err.Error(), "core migration v1") {
		t.Errorf("error should mention core migration, got: %v", err)
	}
}

func TestRunMigrations_SkipsUnhealthyPlugin(t *testing.T) {
	db, conn := newTestMigrationDB(t)

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info:       PluginInfo{Name: "sick-plugin"},
			migrations: []Migration{{Version: 1, Up: "CREATE TABLE t (id INT)"}},
		},
		Healthy: false,
	}

	err := RunMigrations(context.Background(), db, DialectPostgres, nil, []*LoadedPlugin{plugin})
	if err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	// Only the create table exec call; no migration exec.
	if len(conn.execCalls) != 1 {
		t.Errorf("expected 1 exec call (create table only), got %d", len(conn.execCalls))
	}
}

func TestRunMigrations_SkipsNonMigrationPlugin(t *testing.T) {
	db, conn := newTestMigrationDB(t)

	plugin := &LoadedPlugin{
		Plugin:  &testPlugin{info: PluginInfo{Name: "no-migrations"}},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectPostgres, nil, []*LoadedPlugin{plugin})
	if err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	// Only the create table exec call.
	if len(conn.execCalls) != 1 {
		t.Errorf("expected 1 exec call (create table only), got %d", len(conn.execCalls))
	}
}

func TestRunMigrations_RunsPluginMigration(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.existsResult = false // migration not yet applied

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info:       PluginInfo{Name: "my-plugin"},
			migrations: []Migration{{Version: 1, Up: "CREATE TABLE my_table (id INT)"}},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectPostgres, nil, []*LoadedPlugin{plugin})
	if err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	// Check that a transaction was started, migration SQL was executed in the
	// transaction, the record was inserted, and the transaction committed.
	if len(conn.txExecCalls) != 2 {
		t.Fatalf("expected 2 tx exec calls (migration + record), got %d", len(conn.txExecCalls))
	}
	if conn.txRollback {
		t.Error("transaction should be committed, not rolled back")
	}
}

func TestRunMigrations_SkipsAlreadyApplied(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.existsResult = true // migration already applied

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info:       PluginInfo{Name: "my-plugin"},
			migrations: []Migration{{Version: 1, Up: "CREATE TABLE my_table (id INT)"}},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectPostgres, nil, []*LoadedPlugin{plugin})
	if err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	// Should not have started a transaction.
	if len(conn.txExecCalls) != 0 {
		t.Errorf("expected 0 tx exec calls for already-applied migration, got %d", len(conn.txExecCalls))
	}
}

func TestRunMigrations_MultipleMigrations(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.existsResult = false // none applied yet

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info: PluginInfo{Name: "my-plugin"},
			migrations: []Migration{
				{Version: 1, Up: "CREATE TABLE t1 (id INT)"},
				{Version: 2, Up: "CREATE TABLE t2 (id INT)"},
			},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectPostgres, nil, []*LoadedPlugin{plugin})
	if err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	// Two migrations, each doing 2 tx exec calls (migration + record) = 4.
	if len(conn.txExecCalls) != 4 {
		t.Fatalf("expected 4 tx exec calls (2 migrations x 2), got %d", len(conn.txExecCalls))
	}
}

func TestRunMigrations_BeginTxError(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.existsResult = false
	conn.beginErr = errors.New("cannot begin transaction")

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info:       PluginInfo{Name: "my-plugin"},
			migrations: []Migration{{Version: 1, Up: "CREATE TABLE t (id INT)"}},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectPostgres, nil, []*LoadedPlugin{plugin})
	if err == nil {
		t.Error("expected error from begin failure")
	}
	if !strings.Contains(err.Error(), "begin") {
		t.Errorf("error should mention begin, got: %v", err)
	}
}

func TestRunMigrations_QueryRowError(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.queryErr = errors.New("table does not exist")

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info:       PluginInfo{Name: "my-plugin"},
			migrations: []Migration{{Version: 1, Up: "CREATE TABLE t (id INT)"}},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectPostgres, nil, []*LoadedPlugin{plugin})
	if err == nil {
		t.Error("expected error from query failure")
	}
	if !strings.Contains(err.Error(), "check") {
		t.Errorf("error should mention check, got: %v", err)
	}
}

// The two tests below were `t.Skip("requires more sophisticated fake ...")`
// over empty bodies. The fake already had what they needed: execFailAfter and
// execCallCount, whose own comment says "Used to defer errors past the CREATE
// TABLE statement", which is the whole of the difficulty they claimed. The
// capability was presumably added for one of the two callers at lines 621 and
// 933 and these were never revisited.
//
// The exec sequence they index into, measured rather than reasoned about, for
// one plugin with one migration on DialectPostgres:
//
//	exec 1  session  CREATE TABLE IF NOT EXISTS plugin_migrations (...)
//	exec 2  tx       the plugin's own migration SQL
//	exec 3  tx       INSERT INTO plugin_migrations (plugin_name, version)
//
// The advisory lock and the search_path pin are deliberately not counted --
// ExecContext routes them to lockCalls instead, so they do not shift these
// indices.
//
// One thing to know before trusting a green run here. Deleting the production
// `_ = tx.Rollback()` these tests assert on does NOT turn them red -- it hangs
// them, and `go test` reports a timeout with no failing assertion:
//
//	panic: test timed out after 25s
//	goroutine 1 [chan receive]:
//	database/sql.(*Conn).close(...)
//	  .../plugin/migration_test.go:829   <- the RunMigrations call
//
// The leaked *Tx still holds the pooled connection, so RunMigrations blocks in
// its own session.Close(). That is a real property of the production code
// rather than an artifact of the fake -- the rollback is not only about not
// leaving a half-applied migration committed, it is what lets the session
// close at all -- but it means a regression here costs a CI timeout instead of
// a message. The txRollback assertions below are still worth having: they are
// what names the cause when someone does read the log.
//
// So these were falsified the other way, by breaking the fixture rather than
// the fix: dropping conn.execErr makes RunMigrations succeed and both tests
// fail on "expected an error ...", which proves the injection is load-bearing.
// conn.txRollback has exactly one writer, migrationTestTx.Rollback.

// TestRunMigrations_ExecInTxError: the plugin's own migration SQL fails inside
// the transaction. RunMigrations must roll back and report which plugin and
// version, because the operator's next question is which migration to fix.
func TestRunMigrations_ExecInTxError(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.existsResult = false
	// Past the CREATE TABLE, so exec 2 -- the migration SQL -- is the one that
	// fails.
	conn.execFailAfter = 1
	conn.execErr = errors.New("migration sql failed")

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info:       PluginInfo{Name: "my-plugin"},
			migrations: []Migration{{Version: 1, Up: "CREATE TABLE t (id INT)"}},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectPostgres, nil, []*LoadedPlugin{plugin})
	if err == nil {
		t.Fatal("expected an error when the migration SQL fails inside the transaction")
	}
	// Naming the injected error, not just asserting non-nil: RunMigrations has
	// several other failure paths that also return non-nil here, so a bare
	// err != nil would pass on a fixture that never fired.
	if !strings.Contains(err.Error(), "migration sql failed") {
		t.Errorf("error = %v, want it to carry the injected \"migration sql failed\"", err)
	}
	if !strings.Contains(err.Error(), "my-plugin") || !strings.Contains(err.Error(), "v1") {
		t.Errorf("error = %v, want it to name the plugin and version", err)
	}
	if !conn.txRollback {
		t.Error("transaction was not rolled back after the migration SQL failed")
	}
	// The INSERT must not have run: recording a migration that did not apply
	// is what makes the next run skip it.
	for _, c := range conn.txExecCalls {
		if strings.Contains(c.query, "INSERT INTO plugin_migrations") {
			t.Errorf("the migration was recorded despite failing: %q", c.query)
		}
	}
}

// TestRunMigrations_InsertRecordError: the migration applies but recording it
// fails. Same requirement in the other direction -- the transaction must roll
// back, so the applied-but-unrecorded state never reaches the database.
func TestRunMigrations_InsertRecordError(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.existsResult = false
	// Past the CREATE TABLE and past the migration SQL, so exec 3 -- the
	// INSERT -- is the one that fails.
	conn.execFailAfter = 2
	conn.execErr = errors.New("record insert failed")

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info:       PluginInfo{Name: "my-plugin"},
			migrations: []Migration{{Version: 1, Up: "CREATE TABLE t (id INT)"}},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectPostgres, nil, []*LoadedPlugin{plugin})
	if err == nil {
		t.Fatal("expected an error when recording the migration fails")
	}
	if !strings.Contains(err.Error(), "record insert failed") {
		t.Errorf("error = %v, want it to carry the injected \"record insert failed\"", err)
	}
	if !strings.Contains(err.Error(), "record") {
		t.Errorf("error = %v, want it to say the *record* step failed rather than the "+
			"migration itself -- they need different operator responses", err)
	}
	if !conn.txRollback {
		t.Error("transaction was not rolled back after the record insert failed")
	}
	// This is the assertion that makes the test worth having: the migration
	// SQL did run, and the rollback is the only thing that undoes it.
	sawMigration := false
	for _, c := range conn.txExecCalls {
		if strings.Contains(c.query, "CREATE TABLE t") {
			sawMigration = true
		}
	}
	if !sawMigration {
		t.Error("the migration SQL never ran, so this test did not reach the record step")
	}
}

func TestRunMigrations_CommitError(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.existsResult = false
	conn.txCommitErr = errors.New("commit failed")

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info:       PluginInfo{Name: "my-plugin"},
			migrations: []Migration{{Version: 1, Up: "CREATE TABLE t (id INT)"}},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectPostgres, nil, []*LoadedPlugin{plugin})
	if err == nil {
		t.Error("expected error from commit failure")
	}
	if !strings.Contains(err.Error(), "commit") {
		t.Errorf("error should mention commit, got: %v", err)
	}
}

func TestRunMigrations_MySQLDialect_WithMySQLSQL(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.existsResult = false

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info: PluginInfo{Name: "my-plugin"},
			migrations: []Migration{
				{Version: 1, Up: "PG SQL", UpMySQL: "MYSQL SQL"},
			},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectMySQL, nil, []*LoadedPlugin{plugin})
	if err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	if len(conn.txExecCalls) < 1 {
		t.Fatal("expected tx exec calls")
	}
	// The first tx exec should be the MySQL-specific SQL.
	if !strings.Contains(conn.txExecCalls[0].query, "MYSQL SQL") {
		t.Errorf("expected MySQL-specific SQL, got: %s", conn.txExecCalls[0].query)
	}
}

func TestRunMigrations_MySQLDialect_NoMySQLSQL(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.existsResult = false

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info: PluginInfo{Name: "my-plugin"},
			migrations: []Migration{
				{Version: 1, Up: "PG SQL"}, // No UpMySQL
			},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectMySQL, nil, []*LoadedPlugin{plugin})
	if err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	// Should record skip — one tx exec call for the insert.
	if len(conn.txExecCalls) != 1 {
		t.Fatalf("expected 1 tx exec call (skip record), got %d", len(conn.txExecCalls))
	}
	if !strings.Contains(conn.txExecCalls[0].query, "INSERT INTO plugin_migrations") {
		t.Errorf("expected INSERT to record skip, got: %s", conn.txExecCalls[0].query)
	}
}

func TestRunMigrations_MSSQLDialect_WithMSSQLSQL(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.existsResult = false

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info: PluginInfo{Name: "my-plugin"},
			migrations: []Migration{
				{Version: 1, Up: "PG SQL", UpMSSQL: "MSSQL SQL"},
			},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectMSSQL, nil, []*LoadedPlugin{plugin})
	if err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	if len(conn.txExecCalls) < 1 {
		t.Fatal("expected tx exec calls")
	}
	if !strings.Contains(conn.txExecCalls[0].query, "MSSQL SQL") {
		t.Errorf("expected MSSQL-specific SQL, got: %s", conn.txExecCalls[0].query)
	}
}

func TestRunMigrations_MSSQLDialect_NoMSSQLSQL(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.existsResult = false

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info: PluginInfo{Name: "my-plugin"},
			migrations: []Migration{
				{Version: 1, Up: "PG SQL"}, // No UpMSSQL
			},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectMSSQL, nil, []*LoadedPlugin{plugin})
	if err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	// Should record skip — one tx exec call for the insert.
	if len(conn.txExecCalls) != 1 {
		t.Fatalf("expected 1 tx exec call (skip record), got %d", len(conn.txExecCalls))
	}
	if !strings.Contains(conn.txExecCalls[0].query, "INSERT INTO plugin_migrations") {
		t.Errorf("expected INSERT to record skip, got: %s", conn.txExecCalls[0].query)
	}
}

func TestRunMigrations_MySQLSkip_InsertError(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.existsResult = false
	// Let CREATE TABLE succeed (exec 0), fail on the skip-record insert (exec 1).
	conn.execFailAfter = 1
	conn.execErr = errors.New("insert failed")

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info: PluginInfo{Name: "my-plugin"},
			migrations: []Migration{
				{Version: 1, Up: "PG SQL"}, // No UpMySQL
			},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectMySQL, nil, []*LoadedPlugin{plugin})
	if err == nil {
		t.Error("expected error from insert failure during skip")
	}
	if !strings.Contains(err.Error(), "record skip") {
		t.Errorf("error should mention 'record skip', got: %v", err)
	}
	if !conn.txRollback {
		t.Error("transaction should have been rolled back on error")
	}
}

func TestRunMigrations_MigrationsSorted(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.existsResult = false

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info: PluginInfo{Name: "my-plugin"},
			migrations: []Migration{
				{Version: 3, Up: "v3"},
				{Version: 1, Up: "v1"},
				{Version: 2, Up: "v2"},
			},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectPostgres, nil, []*LoadedPlugin{plugin})
	if err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	// Should execute in order: v1, v2, v3.
	if len(conn.txExecCalls) != 6 { // 3 migrations x 2 (migration + record)
		t.Fatalf("expected 6 tx exec calls, got %d", len(conn.txExecCalls))
	}
	// v1 exec call.
	if !strings.Contains(conn.txExecCalls[0].query, "v1") {
		t.Errorf("expected v1 first, got: %s", conn.txExecCalls[0].query)
	}
	// v2 exec call (call #2 is the record insert, call #3 is v2 migration).
	if !strings.Contains(conn.txExecCalls[2].query, "v2") {
		t.Errorf("expected v2 second, got: %s", conn.txExecCalls[2].query)
	}
	// v3 exec call (call #5 is v3 migration).
	if !strings.Contains(conn.txExecCalls[4].query, "v3") {
		t.Errorf("expected v3 third, got: %s", conn.txExecCalls[4].query)
	}
}

func TestRunMigrations_CreateTableError(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.execErr = errors.New("permission denied")

	err := RunMigrations(context.Background(), db, DialectPostgres, nil, nil)
	if err == nil {
		t.Error("expected error from create table failure")
	}
	if !strings.Contains(err.Error(), "create migrations table") {
		t.Errorf("error should mention 'create migrations table', got: %v", err)
	}
}

func TestRunMigrations_MultiplePlugins(t *testing.T) {
	db, conn := newTestMigrationDB(t)
	conn.existsResult = false

	plugins := []*LoadedPlugin{
		{
			Plugin: &testMigrationPlugin{
				info:       PluginInfo{Name: "plugin-a"},
				migrations: []Migration{{Version: 1, Up: "A_SQL"}},
			},
			Healthy: true,
		},
		{
			Plugin: &testMigrationPlugin{
				info:       PluginInfo{Name: "plugin-b"},
				migrations: []Migration{{Version: 1, Up: "B_SQL"}},
			},
			Healthy: true,
		},
	}

	err := RunMigrations(context.Background(), db, DialectPostgres, nil, plugins)
	if err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	// Each plugin has 1 migration x 2 exec calls (migration + record) = 4 total.
	if len(conn.txExecCalls) != 4 {
		t.Fatalf("expected 4 tx exec calls, got %d", len(conn.txExecCalls))
	}
	// Check both A_SQL and B_SQL were executed.
	foundA, foundB := false, false
	for _, c := range conn.txExecCalls {
		if strings.Contains(c.query, "A_SQL") {
			foundA = true
		}
		if strings.Contains(c.query, "B_SQL") {
			foundB = true
		}
	}
	if !foundA {
		t.Error("plugin-a migration not executed")
	}
	if !foundB {
		t.Error("plugin-b migration not executed")
	}
}

func TestRunMigrations_OnlyCoreMigrationsWithPlugins(t *testing.T) {
	db, conn := newTestMigrationDB(t)

	coreMigrations := []Migration{
		{Version: 1, Up: "core_v1"},
	}

	plugin := &LoadedPlugin{
		Plugin: &testMigrationPlugin{
			info:       PluginInfo{Name: "my-plugin"},
			migrations: []Migration{{Version: 1, Up: "plugin_v1"}},
		},
		Healthy: true,
	}

	err := RunMigrations(context.Background(), db, DialectPostgres, coreMigrations, []*LoadedPlugin{plugin})
	if err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	// execCalls: [create_table, core_v1]
	if len(conn.execCalls) != 2 {
		t.Fatalf("expected 2 non-tx exec calls, got %d", len(conn.execCalls))
	}
	if !strings.Contains(conn.execCalls[1].query, "core_v1") {
		t.Errorf("expected core migration, got: %s", conn.execCalls[1].query)
	}
}

// ---------------------------------------------------------------------------
// testMigrationPlugin implements Plugin + HasMigrations for testing
// ---------------------------------------------------------------------------

type testMigrationPlugin struct {
	info       PluginInfo
	migrations []Migration
}

func (p *testMigrationPlugin) Info() PluginInfo                                 { return p.info }
func (p *testMigrationPlugin) Init(ctx context.Context, env *Environment) error { return nil }
func (p *testMigrationPlugin) Migrations() []Migration                          { return p.migrations }
