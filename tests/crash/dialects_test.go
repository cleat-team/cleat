package crash

// cleat#2288: the shutdown scenarios run on MySQL and SQL Server, not only on
// PostgreSQL.
//
// Why this is its own file rather than a loop inside each scenario: the three
// dialects disagree about WHERE a workflow row lives, not just about how a
// statement is spelled, and that difference is not visible from the statement.
//
//   - PostgreSQL and SQL Server keep everything in one database. On SQL Server
//     unqualified table names are correct as sa, whose default schema is dbo --
//     the worker itself never writes `dbo.`.
//   - MySQL does not. The worker strips the database out of --db
//     (cmd/cleat-worker's mysqlBaseDSN) and serves a PER-TENANT database named
//     by MySQLTenantDatabaseName, because MySQL has no schemas and no RLS and
//     separates tenants by database instead. Measured 2026-09-25: pointed at a
//     fresh base database, workflow_instances in THAT database held 0 rows and
//     the per-tenant database held the rows. So the defs and instances this
//     harness hand-writes have to be written to a different database than the
//     one the worker is told to migrate, and every assertion has to read them
//     back from there.
//
// A second MySQL consequence, and it is the reason the tests below take care
// about teardown: the per-tenant database name is derived from the tenant UUID
// ALONE, so it is not per-suite. Two concurrent MySQL crash runs share it.
// PostgreSQL's harness took its own database (ensureCrashDatabase) precisely to
// escape engine/testutil's unqualified DELETEs; on MySQL that escape is not
// available, and per-test task queues plus unique workflow ids are what keep
// runs apart. Run this suite and a MySQL engine test concurrently and they will
// delete each other's rows.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/migration"
)

// crashTarget is one dialect's half of this suite: where its workflow rows
// live, how its statements are spelled, and what the worker must be told.
type crashTarget struct {
	// name is the cleat-worker --driver value and the label in test output.
	name string
	// db is where this harness reads and writes workflow rows. On PostgreSQL
	// and SQL Server that is the database the worker serves; on MySQL it is
	// the per-tenant database, which is NOT the one --db names.
	db *sql.DB
	// workerDB and migrateDB are the --db and --migrate-db values.
	workerDB  string
	migrateDB string
	// driverFlag is the value to pass as --driver, or "" when the worker's
	// default already is this dialect. PostgreSQL is the default, and leaving
	// its command line unchanged keeps every existing scenario byte-identical.
	driverFlag string
}

// entryPointsLiteral renders the entry_points column.
//
// Three spellings, not one: PostgreSQL's is a text[] and the other two are a
// JSON string. Writing ARRAY[...] on MySQL is a syntax error and writing
// '["a"]' on PostgreSQL stores the literal text.
func (tg *crashTarget) entryPointsLiteral() string {
	names := []string{
		"three_charges", "compensating", "with_cleanup", "continues_as_new",
		"cleanup_with_backoff", "parent_with_child", "child_echo", "catches_abort",
	}
	if tg.name == "postgres" {
		quoted := make([]string, len(names))
		for i, n := range names {
			quoted[i] = "'" + n + "'"
		}
		return "ARRAY[" + strings.Join(quoted, ",") + "]"
	}
	// json.Marshal cannot fail on a []string of compile-time constants.
	b, _ := json.Marshal(names)
	return "'" + string(b) + "'"
}

// placeholder returns the n-th (1-based) placeholder. MySQL binds by
// APPEARANCE and the other two by NUMBER, so a statement whose numbering
// disagrees with its text order is correct on two dialects and silently swaps
// its arguments on the third -- which is why every statement below is written
// with its placeholders in order.
func (tg *crashTarget) placeholder(n int) string {
	switch tg.name {
	case "mysql":
		return "?"
	case "mssql":
		return fmt.Sprintf("@p%d", n)
	default:
		return fmt.Sprintf("$%d", n)
	}
}

// jsonArg renders a JSON-valued argument: PostgreSQL needs the cast, the other
// two take the text.
func (tg *crashTarget) jsonArg(raw string) string {
	if tg.name == "postgres" {
		return raw + "::jsonb"
	}
	return raw
}

// stmt returns a statement this harness runs against the workflow tables.
//
// On SQL Server it must carry sp_set_session_context in the SAME BATCH.
// migration 075 installs a BLOCK PREDICATE on these tables keyed on
// dbo.fn_tenant_filter, and a connection that has not set the tenant is
// refused -- Msg 33504, "the target object ... has a block predicate that
// conflicts with this operation" -- which is what this harness hit the first
// time it ran. The context is CONNECTION-scoped and does not survive a pooled
// connection being handed to someone else, so it cannot be set once at open;
// engine's own MSSQL tests put it in the same statement for the same reason.
//
// Reading is worse than writing here, not better: a filtered read returns
// zero rows rather than an error, so a prefix-less "does this def exist?"
// answers no, and the INSERT that follows is refused as a duplicate key --
// a symptom two steps from its cause.
func (tg *crashTarget) stmt(query string) string {
	if tg.name != "mssql" {
		return query
	}
	return "EXEC sp_set_session_context @key=N'tenant_id', @value=N'" + defaultTenant + "'; " + query
}

// -- targets -------------------------------------------------------------

// postgresTarget is the existing PostgreSQL path, wrapped. Every scenario that
// is not in cleat#2288's scope still calls the helpers in harness_test.go
// directly; this exists so the shutdown scenarios can run on all three through
// one body.
func postgresTarget(t *testing.T) *crashTarget {
	t.Helper()
	db := ownerDB(t)
	t.Cleanup(func() { db.Close() })
	return &crashTarget{
		name:      "postgres",
		db:        db,
		workerDB:  appDSN(t),
		migrateDB: ensureCrashDatabase(t),
	}
}

// mysqlTarget creates and returns the MySQL side of the harness.
//
// Two databases, because MySQL needs two:
//
//   - a fresh base database for the worker's GLOBALS. Not optional: pointed at
//     a shared base database the worker refuses to start outright --
//     "15 secret(s) are stored and CLEAT_SECRET_MASTER_KEY is not set" -- and
//     deployment_secrets lives in the base database, not the tenant one.
//   - the per-tenant database the rows actually live in, created and migrated
//     here rather than by the worker, because this harness has to write the
//     definition BEFORE it starts a worker and the worker is what would
//     otherwise create it.
func mysqlTarget(t *testing.T) *crashTarget {
	t.Helper()
	dsn := os.Getenv("CLEAT_CRASH_MYSQL")
	if dsn == "" {
		t.Skip("CLEAT_CRASH_MYSQL not set, skipping the MySQL shutdown scenarios")
	}

	base := ensureMySQLDatabase(t, dsn, crashDatabase)
	tenant := ensureMySQLTenantDatabase(t, dsn)

	return &crashTarget{
		name:       "mysql",
		db:         tenant,
		workerDB:   base,
		migrateDB:  base,
		driverFlag: "mysql",
	}
}

// mssqlTarget creates and returns the SQL Server side of the harness. One
// database, unlike MySQL.
func mssqlTarget(t *testing.T) *crashTarget {
	t.Helper()
	dsn := os.Getenv("CLEAT_CRASH_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_CRASH_MSSQL not set, skipping the SQL Server shutdown scenarios")
	}

	db := ensureMSSQLDatabase(t, dsn, crashDatabase)

	return &crashTarget{
		name:       "mssql",
		db:         db.DB,
		workerDB:   db.DSN(),
		migrateDB:  db.DSN(),
		driverFlag: "mssql",
	}
}

// -- database lifecycle --------------------------------------------------

// ensureMySQLDatabase creates a database named `name` beside the one the DSN
// names, migrates it, and returns a DSN for it.
//
// Creates beside rather than reusing, for the same reason
// ensureCrashDatabase does on PostgreSQL -- this suite writes definitions and
// queues runs that an engine test's unqualified DELETEs would otherwise reach.
func ensureMySQLDatabase(t *testing.T, baseDSN, name string) string {
	t.Helper()
	admin := openAdmin(t, "mysql", baseDSN)
	if _, err := admin.Exec(`CREATE DATABASE IF NOT EXISTS ` + name); err != nil {
		t.Fatalf("creating the MySQL database %s: %v", name, err)
	}
	t.Cleanup(func() { admin.Close() })

	dsn, err := swapMySQLDB(baseDSN, name)
	if err != nil {
		t.Fatalf("deriving a MySQL DSN for %s: %v", name, err)
	}
	db := openAndPing(t, "mysql", dsn, name)
	defer db.Close()

	// Applied through migration.Runner -- the same call cmd/cleat-worker makes
	// at boot -- so the harness is not a second implementation of "apply the
	// shipped migrations".
	if err := migration.NewRunner(db, migration.DialectMySQL,
		filepath.Join(repoRoot(t), "migrations")).Run(context.Background()); err != nil {
		t.Fatalf("applying the shipped MySQL migrations to %s: %v\n\n"+
			"This is the code path cmd/cleat-worker takes at boot, so a failure "+
			"here is a worker that cannot start, not a harness problem.", name, err)
	}
	return dsn
}

// ensureMySQLTenantDatabase creates and migrates the per-tenant database this
// suite's rows live in, and returns a handle to it.
//
// The name comes from engine.MySQLTenantDatabaseName rather than being spelled
// out here: it is the rule the worker uses to find the database, and a test
// that re-derives it is a fourth copy of a rule that already disagreed with
// itself three times (see that function's comment).
func ensureMySQLTenantDatabase(t *testing.T, baseDSN string) *sql.DB {
	t.Helper()
	name := engine.MySQLTenantDatabaseName(defaultTenant)

	admin := openAdmin(t, "mysql", baseDSN)
	if _, err := admin.Exec(`CREATE DATABASE IF NOT EXISTS ` + name); err != nil {
		t.Fatalf("creating the MySQL tenant database %s: %v", name, err)
	}
	admin.Close()

	dsn, err := swapMySQLDB(baseDSN, name)
	if err != nil {
		t.Fatalf("deriving a MySQL DSN for the tenant database: %v", err)
	}
	db := openAndPing(t, "mysql", dsn, name)
	t.Cleanup(func() { db.Close() })

	if err := migration.NewRunner(db, migration.DialectMySQL,
		filepath.Join(repoRoot(t), "migrations")).Run(context.Background()); err != nil {
		t.Fatalf("applying the shipped MySQL migrations to the tenant database %s: %v", name, err)
	}
	return db
}

// dsnCarrier is a *sql.DB that remembers the DSN its connections were opened
// with, so a target can hand the same string to the worker's --db.
type dsnCarrier struct {
	*sql.DB
	dsn string
}

func (d *dsnCarrier) DSN() string { return d.dsn }

// ensureMSSQLDatabase creates a SQL Server database beside the one the DSN
// names, migrates it, and returns a handle.
func ensureMSSQLDatabase(t *testing.T, baseDSN, name string) *dsnCarrier {
	t.Helper()
	masterDSN, err := swapMSSQLDB(baseDSN, "master")
	if err != nil {
		t.Fatalf("deriving a SQL Server master DSN: %v", err)
	}
	admin := openAdmin(t, "sqlserver", masterDSN)

	// Created if absent and NOT dropped afterwards, the same as the PostgreSQL
	// side, so there is nothing to evict sessions for.
	//
	// This deliberately does not carry the `ALTER DATABASE ... SET SINGLE_USER
	// WITH ROLLBACK IMMEDIATE` that the drop recipe in
	// migration/idempotency_tenant_test.go needs: that statement exists to
	// take SIGKILLed workers' abandoned sessions off a database before
	// DROPPING it, and it restricts the database to one connection until
	// something sets MULTI_USER again. Borrowed without the DROP it does
	// nothing but break the next connection -- measured here as the worker
	// refusing to start with `Cannot open database "cleat_crash" that was
	// requested by the login` (4063), two processes after the cause.
	if _, err := admin.Exec(`IF DB_ID('` + name + `') IS NULL CREATE DATABASE ` + name); err != nil {
		t.Fatalf("creating the SQL Server database %s: %v", name, err)
	}
	admin.Close()

	dsn, err := swapMSSQLDB(baseDSN, name)
	if err != nil {
		t.Fatalf("deriving a SQL Server DSN for %s: %v", name, err)
	}
	db := openAndPing(t, "sqlserver", dsn, name)
	t.Cleanup(func() { db.Close() })

	if err := migration.NewRunner(db, migration.DialectMSSQL,
		filepath.Join(repoRoot(t), "migrations")).Run(context.Background()); err != nil {
		t.Fatalf("applying the shipped SQL Server migrations to %s: %v\n\n"+
			"This is the code path cmd/cleat-worker takes at boot, so a failure "+
			"here is a worker that cannot start, not a harness problem.", name, err)
	}
	return &dsnCarrier{DB: db, dsn: dsn}
}

// openAdmin opens the connection used for CREATE DATABASE / ALTER DATABASE.
//
// t.Fatalf, never t.Skip, when it is set but unreachable: the caller has
// already established that the variable is configured, so an unreachable
// server means the setup failed. A skip here would be the §2.12 defect -- a
// green that measured nothing.
func openAdmin(t *testing.T, driver, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("opening %s: %v", redact(dsn), err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("%s configured but unreachable at %s: %v", driver, redact(dsn), err)
	}
	return db
}

func openAndPing(t *testing.T, driver, dsn, what string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("opening the %s database: %v", what, err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("the %s database is unreachable at %s: %v", what, redact(dsn), err)
	}
	return db
}

// swapMySQLDB replaces the database in a go-sql-driver DSN
// (user:pass@tcp(host:port)/dbname?params). Split on the last '/' before the
// query string rather than parsed as a URL, which the DSN is not.
//
// The same function exists in migration/idempotency_tenant_test.go, which this
// package cannot import (it is a _test.go in another package). Noted rather
// than hidden: two copies of a DSN rewrite is how the tenant-database name
// rule came to disagree with itself three times.
func swapMySQLDB(dsn, name string) (string, error) {
	params := ""
	if i := strings.LastIndexByte(dsn, '?'); i >= 0 {
		params = dsn[i:]
		dsn = dsn[:i]
	}
	i := strings.LastIndexByte(dsn, '/')
	if i < 0 {
		return "", fmt.Errorf("no database component in the MySQL DSN %s", redact(dsn))
	}
	return dsn[:i+1] + name + params, nil
}

// swapMSSQLDB replaces the database query parameter in a sqlserver:// URL.
func swapMSSQLDB(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("database", name)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// -- the harness verbs, per dialect --------------------------------------

// deployFixture registers testdata/crashcall as a definition.
func (tg *crashTarget) deployFixture(t *testing.T, taskQueue string) {
	t.Helper()
	// PostgreSQL keeps the helper the rest of the suite already uses, so its
	// SQL exists once. Deleting it into this function to make the shape
	// uniform would be a rewrite of the one path this change is not about.
	if tg.name == "postgres" {
		deployFixture(t, tg.db, taskQueue)
		return
	}
	wasm := buildFixtureWASM(t)

	// Count, then UPDATE or INSERT -- not a DELETE-and-replace, and not a
	// dialect upsert.
	//
	// Not DELETE: on MySQL workflow_instances carries a foreign key to
	// workflow_defs, so deleting the definition of a run that is still there
	// is refused outright (Error 1451). A fresh database has nothing to delete
	// anyway; the shared MySQL tenant database does, which is exactly the case
	// this is guarding.
	//
	// Not an upsert: ON CONFLICT, ON DUPLICATE KEY UPDATE and MERGE are three
	// spellings of one intent, and writing two of them new for a harness that
	// owns its database would be inventing a second thing to get wrong.
	var existing int
	if err := tg.db.QueryRow(tg.stmt(
		`SELECT count(*) FROM workflow_defs WHERE name = 'crashcall' AND version = 1`)).Scan(&existing); err != nil {
		t.Fatalf("looking for the crashcall definition on %s: %v", tg.name, err)
	}

	if existing == 0 {
		q := `INSERT INTO workflow_defs
				(name, version, wasm_bytes, entry_points, min_version,
				 max_history_length, dag_spec, task_queue, abi_version, plugin_deps, tenant_id)
			VALUES ('crashcall', 1, ` + tg.placeholder(1) + `, ` + tg.entryPointsLiteral() + `, 1, 10000, '{}', ` +
			tg.placeholder(2) + `, 1, '{}', ` + tg.placeholder(3) + `)`
		if _, err := tg.db.Exec(tg.stmt(q), wasm, taskQueue, defaultTenant); err != nil {
			t.Fatalf("deploying the crashcall definition on %s: %v", tg.name, err)
		}
		return
	}

	q := `UPDATE workflow_defs SET wasm_bytes = ` + tg.placeholder(1) + `, task_queue = ` + tg.placeholder(2) +
		` WHERE name = 'crashcall' AND version = 1`
	if _, err := tg.db.Exec(tg.stmt(q), wasm, taskQueue); err != nil {
		t.Fatalf("updating the crashcall definition on %s: %v", tg.name, err)
	}
}

// startWorker launches a worker that will claim this target's task queue.
func (tg *crashTarget) startWorker(t *testing.T, bin, taskQueue, svcURL string, extraFlags ...string) *worker {
	t.Helper()
	flags := append([]string{}, extraFlags...)
	if tg.driverFlag != "" {
		flags = append([]string{"--driver", tg.driverFlag}, flags...)
	}
	return startWorkerWith(t, tg, bin, taskQueue, svcURL, flags...)
}

// startWorkflowEntry queues one crashcall instance.
func (tg *crashTarget) startWorkflowEntry(t *testing.T, id, orderID, taskQueue, entry string) {
	t.Helper()
	input := fmt.Sprintf(`{"__entry_point":%q,"orderID":%q}`, entry, orderID)
	q := `INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id)
		VALUES (` + tg.placeholder(1) + `, 'crashcall', 1, 'ready', ` + tg.jsonArg(tg.placeholder(2)) + `, ` +
		tg.placeholder(3) + `, ` + tg.placeholder(4) + `)`
	if _, err := tg.db.Exec(tg.stmt(q), id, input, taskQueue, defaultTenant); err != nil {
		t.Fatalf("queueing workflow %s on %s: %v", id, tg.name, err)
	}
	t.Cleanup(func() {
		_, _ = tg.db.Exec(tg.stmt(`DELETE FROM event_history WHERE workflow_id = `+tg.placeholder(1)), id)
		_, _ = tg.db.Exec(tg.stmt(`DELETE FROM workflow_instances WHERE id = `+tg.placeholder(1)), id)
	})
}

// runRow reads the run's status and error.
func (tg *crashTarget) runRow(t *testing.T, id string) (status, errMsg string) {
	t.Helper()
	var msg sql.NullString
	if err := tg.db.QueryRow(
		tg.stmt(`SELECT status, error_msg FROM workflow_instances WHERE id = `+tg.placeholder(1)),
		id).Scan(&status, &msg); err != nil {
		t.Fatalf("reading %s on %s: %v", id, tg.name, err)
	}
	return status, msg.String
}

// awaitTerminal polls until the workflow leaves ready/running.
func (tg *crashTarget) awaitTerminal(t *testing.T, id string, budget time.Duration) (status, errMsg string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		status, errMsg = tg.runRow(t, id)
		if status != "ready" && status != "running" {
			return status, errMsg
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("workflow %s on %s was still %q after %v", id, tg.name, status, budget)
	return "", ""
}
