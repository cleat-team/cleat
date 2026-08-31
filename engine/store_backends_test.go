package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/engine/testutil"
)

// StoreBackend represents a database backend that can be tested.
// Each backend (PostgreSQL, MySQL, SQL Server) implements this interface
// so that shared test groups can run against all of them.
type StoreBackend interface {
	// Name returns the backend name, e.g. "postgres", "mysql", "mssql".
	Name() string
	// Setup creates a WorkflowStore for testing and returns a teardown function.
	Setup(t *testing.T) (WorkflowStore, func())
	// Enabled reports whether this backend is available in the current environment.
	Enabled() bool
}

// registeredBackends holds all backends registered via RegisterBackend.
var registeredBackends []StoreBackend

// RegisterBackend adds a backend to the test suite.
func RegisterBackend(b StoreBackend) {
	registeredBackends = append(registeredBackends, b)
}

// PostgresBackend implements StoreBackend for PostgreSQL.
type PostgresBackend struct{}

func (b *PostgresBackend) Name() string {
	return "postgres"
}

func (b *PostgresBackend) Enabled() bool {
	return true
}

func (b *PostgresBackend) Setup(t *testing.T) (WorkflowStore, func()) {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	applyPostgresProcedures(t, db)
	testutil.CleanupPostgresTestData(t, db)
	store := NewPostgresStore(db)
	teardown := func() {
		testutil.CleanupPostgresTestData(t, db)
		db.Close()
	}
	return store, teardown
}

// SetupForTenant returns a store that genuinely enforces Row-Level Security.
//
// The superuser/owner connection that adminDB (and Setup, above) uses
// bypasses RLS entirely -- that's a hard PostgreSQL rule for superusers, and
// applies to the table-owning role too unless FORCE ROW LEVEL SECURITY is
// set. So the WorkflowStore returned here is built on a *second* connection,
// authenticated as testutil.PostgresRLSTestRole: an ordinary, non-owning
// role for which Postgres always evaluates RLS policies. Without this,
// every cross-tenant isolation assertion in tenant_isolation_test.go would
// trivially "pass" by seeing every row and simply not asserting on the ones
// it shouldn't -- or, worse, silently prove nothing about whether RLS
// actually blocks cross-tenant access. See testutil.OpenPostgresRLSTestDB.
func (b *PostgresBackend) SetupForTenant(t *testing.T, tenantID string) (WorkflowStore, func()) {
	t.Helper()
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)

	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	store := NewPostgresStore(appDB)
	store.tenantID = tenantID
	teardown := func() {
		testutil.CleanupPostgresTestData(t, adminDB)
		appDB.Close()
		adminDB.Close()
	}
	return store, teardown
}

// MySQLBackend implements StoreBackend for MySQL 8.0+ / MariaDB 10.6+.
type MySQLBackend struct{}

func (b *MySQLBackend) Name() string {
	return "mysql"
}

func (b *MySQLBackend) Enabled() bool {
	return os.Getenv("CLEAT_TEST_MYSQL") != ""
}

func (b *MySQLBackend) Setup(t *testing.T) (WorkflowStore, func()) {
	t.Helper()
	if !b.Enabled() {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}
	db := testutil.MySQLTestDB(t)
	testutil.SetupMySQLFullSchema(t, db)
	applyMySQLProcedures(t, db)
	testutil.CleanupMySQLTestData(t, db)
	store := NewMySQLStore(db)
	teardown := func() {
		testutil.CleanupMySQLTestData(t, db)
		db.Close()
	}
	return store, teardown
}

func (b *MySQLBackend) SetupForTenant(t *testing.T, tenantID string) (WorkflowStore, func()) {
	t.Helper()
	if !b.Enabled() {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}
	db := testutil.MySQLTestDB(t)
	testutil.SetupMySQLFullSchema(t, db)
	testutil.CleanupMySQLTestData(t, db)
	store := NewMySQLStore(db)
	store.tenantID = tenantID
	teardown := func() {
		testutil.CleanupMySQLTestData(t, db)
		db.Close()
	}
	return store, teardown
}

// MSSQLBackend implements StoreBackend for SQL Server 2017+ / Azure SQL Database.
type MSSQLBackend struct{}

func (b *MSSQLBackend) Name() string {
	return "mssql"
}

func (b *MSSQLBackend) Enabled() bool {
	return os.Getenv("CLEAT_TEST_MSSQL") != ""
}

// openMSSQLTenantStore builds an MSSQLStore the way production builds one.
//
// NewMSSQLStore(db) on a plain pool sets sp_set_session_context on none of its
// connections, so under the shipped security policies every tenant-scoped read
// matches nothing and the store cannot see rows it just wrote. Every non-test
// caller goes through the factory (cmd/cleat-worker, cmd/cleat-bench,
// cmd/deploy-workflow); OpenStore is what wraps the connector.
//
// SetupForTenant previously assigned store.tenantID directly, which set the Go
// field without ever setting the session context -- so the store filtered as
// the default tenant while believing it was another one. That is the §1.3
// shape: a scope that exists in the process and not in the database.
func openMSSQLTenantStore(t *testing.T, tenantID string) *MSSQLStore {
	t.Helper()
	ws, closer, err := NewMSSQLStoreFactory(os.Getenv("CLEAT_TEST_MSSQL")).OpenStore(
		context.Background(), tenantID, "default")
	if err != nil {
		t.Fatalf("open a tenant-scoped MSSQL store for %s: %v", tenantID, err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	store, ok := ws.(*MSSQLStore)
	if !ok {
		t.Fatalf("OpenStore returned %T, want *MSSQLStore", ws)
	}
	return store
}

func (b *MSSQLBackend) Setup(t *testing.T) (WorkflowStore, func()) {
	t.Helper()
	if !b.Enabled() {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	applyMSSQLProcedures(t, db)
	testutil.CleanupMSSQLTestData(t, db)
	store := openMSSQLTenantStore(t, DefaultTenantUUID)
	teardown := func() {
		testutil.CleanupMSSQLTestData(t, db)
		db.Close()
	}
	return store, teardown
}

func (b *MSSQLBackend) SetupForTenant(t *testing.T, tenantID string) (WorkflowStore, func()) {
	t.Helper()
	if !b.Enabled() {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	testutil.CleanupMSSQLTestData(t, db)
	store := openMSSQLTenantStore(t, tenantID)
	teardown := func() {
		testutil.CleanupMSSQLTestData(t, db)
		db.Close()
	}
	return store, teardown
}

func init() {
	RegisterBackend(&PostgresBackend{})
	RegisterBackend(&MySQLBackend{})
	RegisterBackend(&MSSQLBackend{})
}

// setupTestData creates standard test fixtures needed by most test groups.
// Call this after backend.Setup() in each test.
func setupTestData(t *testing.T, store WorkflowStore) {
	t.Helper()

	// Deploy a workflow def so instances can reference it
	def := &WorkflowDef{
		Name:       "test-workflow",
		Version:    1,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d}, // minimal WASM magic bytes
		ABIVersion: 1,
		MinVersion: 1,
	}
	if err := store.DeployWorkflowDef(context.Background(), def); err != nil {
		t.Fatalf("setupTestData: DeployWorkflowDef: %v", err)
	}

	// Create workflow instances in various states for testing
	now := time.Now()

	// A "ready" workflow instance
	readyWfID, _, err := store.StartNewRun(context.Background(), "", "test-workflow", 1,
		json.RawMessage(`{"key":"value"}`), "setup-ready-1", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("setupTestData: StartNewRun ready: %v", err)
	}

	// A "running" workflow instance
	_, _, err = store.StartNewRun(context.Background(), "", "test-workflow", 1,
		json.RawMessage(`{"key":"running"}`), "setup-running-1", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("setupTestData: StartNewRun running: %v", err)
	}
	// Claim it to make it running
	_, err = store.ClaimWorkflow(context.Background(), "test-worker")
	if err != nil {
		t.Fatalf("setupTestData: ClaimWorkflow: %v", err)
	}

	// Create a schedule for testing
	err = store.CreateSchedule(context.Background(), Schedule{
		Name:           "test-schedule",
		DefName:        "test-workflow",
		EntryPoint:     "main",
		CronExpression: "* * * * *",
		Input:          json.RawMessage(`{}`),
		Enabled:        true,
		NextRunAt:      now.Add(-1 * time.Hour), // due now
	})
	if err != nil {
		t.Fatalf("setupTestData: CreateSchedule: %v", err)
	}

	// Create a promise for testing
	err = store.CreatePromise(context.Background(), readyWfID, "test-promise", "promise-1")
	if err != nil {
		t.Fatalf("setupTestData: CreatePromise: %v", err)
	}
}

// describeClaimState reports what the claim predicate would see right now.
//
// TestClaimWorkflow, TestClaimSkipLocked and TestListWorkflows_ByStatus fail
// intermittently on some machines and in the cluster CI job -- with
// "ClaimWorkflow returned nil", "first claim returned 10, want 3" and
// "expected at least 1 result" respectively. Those messages say a claim did
// not behave, and nothing about why: not how many rows existed, what status or
// task_queue they carried, or whether they were due. Reproducing has so far
// needed the exact machine state that produced it.
//
// Rather than guess, the assertions call this so the next failure arrives with
// the evidence attached. It is deliberately read-only and best-effort: a
// diagnostic that can itself fail the test would be worse than none.
func describeClaimState(t *testing.T, store WorkflowStore) {
	t.Helper()

	var db *sql.DB
	switch s := store.(type) {
	case *PostgresStore:
		db = s.db
		t.Logf("claim state: store.taskQueues=%v store.tenantID=%q", s.taskQueues, s.tenantID)
	default:
		t.Logf("claim state: no diagnostic for %T", store)
		return
	}

	var total, ready, due, running int
	err := db.QueryRow(`
		SELECT count(*),
		       count(*) FILTER (WHERE status = 'ready'),
		       count(*) FILTER (WHERE status = 'ready' AND next_wake_at <= now()),
		       count(*) FILTER (WHERE status = 'running')
		FROM workflow_instances`).Scan(&total, &ready, &due, &running)
	if err != nil {
		t.Logf("claim state: query failed: %v", err)
		return
	}
	t.Logf("claim state: workflow_instances total=%d ready=%d ready+due=%d running=%d",
		total, ready, due, running)

	rows, err := db.Query(`
		SELECT coalesce(task_queue, '<null>'), status, count(*)
		FROM workflow_instances GROUP BY 1, 2 ORDER BY 1, 2`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var tq, status string
		var n int
		if err := rows.Scan(&tq, &status, &n); err != nil {
			return
		}
		t.Logf("claim state:   task_queue=%q status=%q count=%d", tq, status, n)
	}
}

// truncateAll cleans up ALL test data between test cases.
// setupTestData leaves behind a "ready" workflow and a "running" workflow
// that DeleteExpiredEvents does not touch (it only cleans events for
// terminal workflows).  We delete every row from all dynamic tables so that
// subtests and subsequent tests each start with a truly empty database.
func truncateAll(t *testing.T, store WorkflowStore) {
	t.Helper()
	ctx := context.Background()

	// Delete schedules via the store interface.
	_ = store.DeleteSchedule(ctx, "test-schedule")
	_ = store.DeleteSchedule(ctx, "test-schedule-2")

	// Delete expired events for terminal workflows (a no-op for ready/running
	// instances, but harmless).
	_, _ = store.DeleteExpiredEvents(ctx, time.Now().Add(1*time.Hour))

	// Wipe rows from dynamic tables so the next test section starts clean.
	// Type-switch on the concrete store so we can access the unexported *sql.DB.
	//
	// Only tables present in all three backend test schemas are included.
	// The deletion order respects MySQL's FK constraints (child rows first).
	// MySQL-only tables (workflow_update_requests, idempotency_keys) are
	// handled in a separate branch.
	switch s := store.(type) {
	case *MySQLStore:
		s.db.Exec("DELETE FROM workflow_update_requests")
		s.db.Exec("DELETE FROM workflow_promises")
		s.db.Exec("DELETE FROM workflow_signals")
		s.db.Exec("DELETE FROM concurrency_keys")
		s.db.Exec("DELETE FROM idempotency_keys")
		s.db.Exec("DELETE FROM event_history")
		s.db.Exec("DELETE FROM workflow_instances")
	case *PostgresStore:
		s.db.Exec("DELETE FROM workflow_update_requests")
		s.db.Exec("DELETE FROM workflow_promises")
		s.db.Exec("DELETE FROM workflow_signals")
		s.db.Exec("DELETE FROM concurrency_keys")
		s.db.Exec("DELETE FROM idempotency_keys")
		s.db.Exec("DELETE FROM event_history")
		s.db.Exec("DELETE FROM workflow_instances")
	case *MSSQLStore:
		s.db.Exec("DELETE FROM workflow_update_requests")
		s.db.Exec("DELETE FROM workflow_promises")
		s.db.Exec("DELETE FROM workflow_signals")
		s.db.Exec("DELETE FROM concurrency_keys")
		s.db.Exec("DELETE FROM idempotency_keys")
		s.db.Exec("DELETE FROM event_history")
		s.db.Exec("DELETE FROM workflow_instances")
	}
}

// TestCascadeDelete verifies that ON DELETE CASCADE foreign keys work correctly
// on the child tables referencing workflow_instances. It tests against each
// available database backend using raw SQL.
//
// event_history is asserted on MySQL and SQL Server, not PostgreSQL. All
// three dialects' 001_schema.sql ship it with the same
// "ON DELETE CASCADE" FK, but migrations/postgres/003_procedures.sql
// deliberately drops fk_event_history_workflow on PostgreSQL alone ("no
// longer needed; events are deleted on terminal", by
// finalize_workflow_status itself), and migrations/postgres/032's comment
// re-derives the same fact independently, checked against pg_constraint on
// a live database rather than assumed from the CREATE TABLE text: "NO FK AT
// ALL". Adding fk_test_cascade_eh back here and asserting it cascades
// tested a constraint the shipped PostgreSQL schema has not had since 003,
// and goes out of its way to explain why it does not have it -- not this
// test's job to reassert.
//
// This was found from a full-package run failing with
// `ALTER TABLE event_history ADD CONSTRAINT fk_test_cascade_eh ... : pq:
// insert or update on table "event_history" violates foreign key
// constraint (23503)` -- meaning a row already in event_history at that
// point referenced a workflow_instances id that no longer existed, which
// "no FK at all" is exactly what permits. The specific earlier test that
// left that row could not be pinned down (five separate full and partial
// reruns against freshly recreated databases, including two back-to-back
// runs against the same database, all passed with zero orphaned
// event_history rows found afterward by direct query) so this is not
// claimed as the full explanation, only as what is independently true
// regardless of it: the assertion this test made was never something
// production's PostgreSQL schema promises, with or without an orphan
// anywhere in the table. MySQL and SQL Server never dropped their
// equivalent FK, so their event_history assertion below is a real, current
// invariant and stays.
func TestCascadeDelete(t *testing.T) {
	dialects := []struct {
		name    string
		dialect testutil.Dialect
	}{
		{"postgres", testutil.DialectPostgres},
		{"mysql", testutil.DialectMySQL},
		{"mssql", testutil.DialectMSSQL},
	}

	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			if d.name == "mysql" && os.Getenv("CLEAT_TEST_MYSQL") == "" {
				t.Skip("CLEAT_TEST_MYSQL not set")
			}
			if d.name == "mssql" && os.Getenv("CLEAT_TEST_MSSQL") == "" {
				t.Skip("CLEAT_TEST_MSSQL not set")
			}

			db := testutil.TestDB(t, d.dialect)
			testutil.SetupFullSchema(t, db, d.dialect)
			// Clean any data left from previous test runs. Dispatched by
			// dialect: this loop runs against all three, and calling the
			// PostgreSQL cleanup for every one of them worked only for as long
			// as that helper issued dialect-neutral SQL.
			testutil.CleanupAllTestData(t, db, d.dialect)

			addCascadeFKs(t, db, d.dialect)

			// The privileged handle, for the DELETE that is meant to cascade
			// and for the counts that check it did. db stays as it is for the
			// DDL above and below, which needs ALTER rather than DML rights.
			//
			// This test is the clearest case for the distinction. The DELETE
			// ran on db, and on a SQL Server built from the shipped migrations
			// db is subject to the tenant filter -- so it matched no rows and
			// nothing cascaded. Three of the five child tables then reported a
			// surviving row. The other two, event_history and workflow_signals,
			// reported 0 and *passed*: their rows were still there too, and the
			// same policy that stopped the DELETE hid them from the count.
			// Getting a green from two assertions whose subject the test could
			// no longer see is the failure mode this handle exists to remove.
			verify := testutil.AdminDB(t, db, d.dialect)

			// Insert a workflow def so workflow_instances FK is satisfied.
			// The DELETE is what makes a re-run possible, so it goes through
			// verify: on db it would match nothing on SQL Server and the
			// INSERT below would fail on the primary key the second time this
			// test ever ran against a database.
			verify.Exec(`DELETE FROM workflow_defs WHERE name = 'cascade-test-def'`)
			var emptyBlob string
			switch d.dialect {
			case testutil.DialectMSSQL:
				emptyBlob = "0x00" // MSSQL requires hex literal for varbinary
			default:
				emptyBlob = "'\\x'" // Postgres/MySQL accept hex string
			}
			_, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes) VALUES ('cascade-test-def', 1, ` + emptyBlob + `)`)
			if err != nil {
				t.Fatalf("insert workflow_defs: %v", err)
			}

			wfID := "cascade-test-001"

			// Insert workflow instance.
			insertWorkflowInstance(t, db, d.dialect, wfID)

			// Insert child rows in all 5 tables.
			insertChildRows(t, db, d.dialect, wfID)

			// Delete the workflow instance - cascade should clean up children.
			res, err := verify.Exec(`DELETE FROM workflow_instances WHERE id = '` + wfID + `'`)
			if err != nil {
				t.Fatalf("delete workflow_instances: %v", err)
			}
			// Assert the parent actually went. Without this the whole test can
			// pass on a DELETE that matched nothing, provided the counts below
			// are equally blind -- which is exactly what happened on SQL
			// Server.
			if n, err := res.RowsAffected(); err != nil {
				t.Fatalf("rows affected by the parent delete: %v", err)
			} else if n != 1 {
				t.Fatalf("deleting workflow_instances %q affected %d rows, want 1 -- "+
					"nothing was deleted, so nothing could cascade and the child "+
					"counts below say nothing about ON DELETE CASCADE", wfID, n)
			}

			// Verify all child rows are gone.
			//
			// event_history is excluded on PostgreSQL: addCascadeFKs does not
			// add a CASCADE FK for it there (see TestCascadeDelete's doc
			// comment), so the row insertChildRows wrote for it is not
			// expected to go away with the parent -- that omission is
			// production's, not a gap in this test. CleanupTestData's
			// pattern-based delete at the end of this subtest still removes
			// it.
			childTables := []string{
				"workflow_signals",
				"workflow_promises",
				"concurrency_keys",
				"workflow_update_requests",
			}
			if d.dialect != testutil.DialectPostgres {
				childTables = append([]string{"event_history"}, childTables...)
			}
			for _, table := range childTables {
				var count int
				query := `SELECT COUNT(*) FROM ` + table + ` WHERE workflow_id = '` + wfID + `'`
				switch d.dialect {
				case testutil.DialectMSSQL:
					query = `SELECT COUNT(*) FROM [` + table + `] WHERE workflow_id = '` + wfID + `'`
				}
				if err := verify.QueryRow(query).Scan(&count); err != nil {
					t.Errorf("count %s: %v", table, err)
				}
				if count != 0 {
					t.Errorf("%s: expected 0 rows after cascade delete, got %d", table, count)
				}
			}

			// Clean up test FK constraints so other tests don't see them.
			removeCascadeFKs(t, db, d.dialect)

			// Clean up workflow_defs.
			verify.Exec(`DELETE FROM workflow_defs WHERE name = 'cascade-test-def'`)

			testutil.CleanupTestData(t, verify, d.dialect, "cascade-test-%")
			db.Close()
		})
	}
}

// addCascadeFKs adds ON DELETE CASCADE foreign keys to the test schema.
// The approach differs by dialect because the test schemas have different FK states:
//   - Postgres: no FKs at all, so add them directly.
//   - MySQL: 4 tables have FKs (drop+re-add), concurrency_keys has none (add fresh).
//   - MSSQL: 4 tables have inline REFERENCES (auto-named, IF EXISTS skips them),
//     ADD CONSTRAINT creates named CASCADE FK alongside. concurrency_keys has no FK.
func addCascadeFKs(t *testing.T, db *sql.DB, dialect testutil.Dialect) {
	t.Helper()

	exec := func(sql string) {
		t.Helper()
		if _, err := db.Exec(sql); err != nil {
			t.Fatalf("add CASCADE FK: %v\nSQL: %s", err, sql)
		}
	}

	switch dialect {
	case testutil.DialectPostgres:
		// event_history is deliberately not here. See TestCascadeDelete's doc
		// comment: migrations/postgres/003_procedures.sql drops
		// fk_event_history_workflow on this dialect alone, so adding it back
		// under a test-only name would assert a constraint the shipped
		// schema has never had since 003 -- and would validate it against
		// whatever event_history already holds, including rows a prior
		// test's own (permitted, FK-less) state legitimately left behind.
		//
		// Drop test constraints from prior runs so re-runs are idempotent.
		exec(`ALTER TABLE workflow_signals DROP CONSTRAINT IF EXISTS fk_test_cascade_ws`)
		exec(`ALTER TABLE workflow_promises DROP CONSTRAINT IF EXISTS fk_test_cascade_wp`)
		exec(`ALTER TABLE concurrency_keys DROP CONSTRAINT IF EXISTS fk_test_cascade_ck`)
		exec(`ALTER TABLE workflow_update_requests DROP CONSTRAINT IF EXISTS fk_test_cascade_wu`)
		exec(`ALTER TABLE workflow_signals ADD CONSTRAINT fk_test_cascade_ws FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)
		exec(`ALTER TABLE workflow_promises ADD CONSTRAINT fk_test_cascade_wp FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)
		exec(`ALTER TABLE concurrency_keys ADD CONSTRAINT fk_test_cascade_ck FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)
		exec(`ALTER TABLE workflow_update_requests ADD CONSTRAINT fk_test_cascade_wu FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)

	case testutil.DialectMySQL:
		// The inline FK in CREATE TABLE already has ON DELETE CASCADE for
		// event_history, workflow_signals, workflow_promises, and
		// workflow_update_requests. Drop and re-add them idempotently.
		exec(`SET @cname = (SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE TABLE_NAME = 'event_history' AND CONSTRAINT_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME = 'workflow_instances')`)
		exec(`SET @sql = IF(@cname IS NOT NULL, CONCAT('ALTER TABLE event_history DROP FOREIGN KEY ', @cname), 'SELECT 1')`)
		exec(`PREPARE stmt FROM @sql`)
		exec(`EXECUTE stmt`)
		exec(`DEALLOCATE PREPARE stmt`)
		exec(`ALTER TABLE event_history ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)

		// workflow_signals
		exec(`SET @cname = (SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE TABLE_NAME = 'workflow_signals' AND CONSTRAINT_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME = 'workflow_instances')`)
		exec(`SET @sql = IF(@cname IS NOT NULL, CONCAT('ALTER TABLE workflow_signals DROP FOREIGN KEY ', @cname), 'SELECT 1')`)
		exec(`PREPARE stmt FROM @sql`)
		exec(`EXECUTE stmt`)
		exec(`DEALLOCATE PREPARE stmt`)
		exec(`ALTER TABLE workflow_signals ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)

		// workflow_promises
		exec(`SET @cname = (SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE TABLE_NAME = 'workflow_promises' AND CONSTRAINT_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME = 'workflow_instances')`)
		exec(`SET @sql = IF(@cname IS NOT NULL, CONCAT('ALTER TABLE workflow_promises DROP FOREIGN KEY ', @cname), 'SELECT 1')`)
		exec(`PREPARE stmt FROM @sql`)
		exec(`EXECUTE stmt`)
		exec(`DEALLOCATE PREPARE stmt`)
		exec(`ALTER TABLE workflow_promises ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)

		// workflow_update_requests
		exec(`SET @cname = (SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE TABLE_NAME = 'workflow_update_requests' AND CONSTRAINT_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME = 'workflow_instances')`)
		exec(`SET @sql = IF(@cname IS NOT NULL, CONCAT('ALTER TABLE workflow_update_requests DROP FOREIGN KEY ', @cname), 'SELECT 1')`)
		exec(`PREPARE stmt FROM @sql`)
		exec(`EXECUTE stmt`)
		exec(`DEALLOCATE PREPARE stmt`)
		exec(`ALTER TABLE workflow_update_requests ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)

		// concurrency_keys: add FK (skips if already exists with a different name)
		db.Exec(`ALTER TABLE concurrency_keys ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)

	case testutil.DialectMSSQL:
		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_event_history_workflow') ALTER TABLE dbo.event_history DROP CONSTRAINT fk_event_history_workflow`)
		exec(`ALTER TABLE dbo.event_history ADD CONSTRAINT fk_event_history_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE`)

		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_signals_workflow') ALTER TABLE dbo.workflow_signals DROP CONSTRAINT fk_signals_workflow`)
		exec(`ALTER TABLE dbo.workflow_signals ADD CONSTRAINT fk_signals_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE`)

		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_promises_workflow') ALTER TABLE dbo.workflow_promises DROP CONSTRAINT fk_promises_workflow`)
		exec(`ALTER TABLE dbo.workflow_promises ADD CONSTRAINT fk_promises_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE`)

		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_concurrency_keys_workflow') ALTER TABLE dbo.concurrency_keys DROP CONSTRAINT fk_concurrency_keys_workflow`)
		exec(`ALTER TABLE dbo.concurrency_keys ADD CONSTRAINT fk_concurrency_keys_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE`)

		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_update_requests_workflow') ALTER TABLE dbo.workflow_update_requests DROP CONSTRAINT fk_update_requests_workflow`)
		exec(`ALTER TABLE dbo.workflow_update_requests ADD CONSTRAINT fk_update_requests_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE`)
	}
}

// insertWorkflowInstance inserts a single workflow_instances row for cascade testing.
func insertWorkflowInstance(t *testing.T, db *sql.DB, dialect testutil.Dialect, wfID string) {
	t.Helper()

	switch dialect {
	case testutil.DialectPostgres:
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, completed_at) VALUES ($1, 'cascade-test-def', 1, 'dead_lettered', NOW())`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_instances (postgres): %v", err)
		}
	case testutil.DialectMySQL:
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, completed_at) VALUES (?, 'cascade-test-def', 1, 'dead_lettered', NOW(6))`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_instances (mysql): %v", err)
		}
	case testutil.DialectMSSQL:
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, completed_at) VALUES (@p1, 'cascade-test-def', 1, 'dead_lettered', SYSUTCDATETIME())`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_instances (mssql): %v", err)
		}
	}
}

// insertChildRows inserts one row into each of the 5 child tables for cascade testing.
func insertChildRows(t *testing.T, db *sql.DB, dialect testutil.Dialect, wfID string) {
	t.Helper()

	// event_history
	switch dialect {
	case testutil.DialectPostgres:
		_, err := db.Exec(`INSERT INTO event_history (workflow_id, step, service, operation, request, event_type) VALUES ($1, 1, 'test-svc', 'test-op', '{}', 'call')`, wfID)
		if err != nil {
			t.Fatalf("insert event_history (postgres): %v", err)
		}
	case testutil.DialectMySQL:
		_, err := db.Exec(`INSERT INTO event_history (workflow_id, step, service, operation, request, event_type) VALUES (?, 1, 'test-svc', 'test-op', '{}', 'call')`, wfID)
		if err != nil {
			t.Fatalf("insert event_history (mysql): %v", err)
		}
	case testutil.DialectMSSQL:
		_, err := db.Exec(`INSERT INTO event_history (workflow_id, step, service, operation, request, event_type) VALUES (@p1, 1, 'test-svc', 'test-op', '{}', 'call')`, wfID)
		if err != nil {
			t.Fatalf("insert event_history (mssql): %v", err)
		}
	}

	// workflow_signals
	switch dialect {
	case testutil.DialectPostgres:
		_, err := db.Exec(`INSERT INTO workflow_signals (workflow_id, signal_name) VALUES ($1, 'test-signal')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_signals (postgres): %v", err)
		}
	case testutil.DialectMySQL:
		_, err := db.Exec(`INSERT INTO workflow_signals (workflow_id, signal_name, payload) VALUES (?, 'test-signal', '{}')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_signals (mysql): %v", err)
		}
	case testutil.DialectMSSQL:
		_, err := db.Exec(`INSERT INTO workflow_signals (workflow_id, signal_name, payload) VALUES (@p1, 'test-signal', '{}')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_signals (mssql): %v", err)
		}
	}

	// workflow_promises
	switch dialect {
	case testutil.DialectPostgres:
		_, err := db.Exec(`INSERT INTO workflow_promises (workflow_id, promise_id, promise_name) VALUES ($1, 'promise-1', 'test-promise')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_promises (postgres): %v", err)
		}
	case testutil.DialectMySQL:
		_, err := db.Exec(`INSERT INTO workflow_promises (workflow_id, promise_id, promise_name, tenant_id) VALUES (?, 'promise-1', 'test-promise', '')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_promises (mysql): %v", err)
		}
	case testutil.DialectMSSQL:
		_, err := db.Exec(`INSERT INTO workflow_promises (workflow_id, promise_id, promise_name, tenant_id) VALUES (@p1, 'promise-1', 'test-promise', '')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_promises (mssql): %v", err)
		}
	}

	// concurrency_keys
	switch dialect {
	case testutil.DialectPostgres:
		_, err := db.Exec(`INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at) VALUES (digest('cascade-test-key', 'sha256'), 'cascade-test-key', $1, NOW() + INTERVAL '1 hour')`, wfID)
		if err != nil {
			t.Fatalf("insert concurrency_keys (postgres): %v", err)
		}
	case testutil.DialectMySQL:
		_, err := db.Exec(`INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at) VALUES (UNHEX(SHA2('cascade-test-key', 256)), 'cascade-test-key', ?, NOW(6) + INTERVAL 1 HOUR)`, wfID)
		if err != nil {
			t.Fatalf("insert concurrency_keys (mysql): %v", err)
		}
	case testutil.DialectMSSQL:
		_, err := db.Exec(`INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at) VALUES (HASHBYTES('SHA2_256', 'cascade-test-key'), 'cascade-test-key', @p1, DATEADD(HOUR, 1, SYSUTCDATETIME()))`, wfID)
		if err != nil {
			t.Fatalf("insert concurrency_keys (mssql): %v", err)
		}
	}

	// workflow_update_requests
	switch dialect {
	case testutil.DialectPostgres:
		_, err := db.Exec(`INSERT INTO workflow_update_requests (workflow_id, update_name) VALUES ($1, 'test-update')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_update_requests (postgres): %v", err)
		}
	case testutil.DialectMySQL:
		_, err := db.Exec(`INSERT INTO workflow_update_requests (workflow_id, update_name, payload) VALUES (?, 'test-update', '{}')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_update_requests (mysql): %v", err)
		}
	case testutil.DialectMSSQL:
		_, err := db.Exec(`INSERT INTO workflow_update_requests (workflow_id, update_name, payload) VALUES (@p1, 'test-update', '{}')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_update_requests (mssql): %v", err)
		}
	}
}

// removeCascadeFKs drops the test-specific CASCADE FK constraints so they
// don't interfere with other tests sharing the same database.
func removeCascadeFKs(t *testing.T, db *sql.DB, dialect testutil.Dialect) {
	t.Helper()

	exec := func(sql string) {
		t.Helper()
		if _, err := db.Exec(sql); err != nil {
			t.Logf("remove CASCADE FK (non-fatal): %v\nSQL: %s", err, sql)
		}
	}

	switch dialect {
	case testutil.DialectPostgres:
		// event_history is not here; addCascadeFKs never adds fk_test_cascade_eh
		// on this dialect. See TestCascadeDelete's doc comment.
		exec(`ALTER TABLE workflow_signals DROP CONSTRAINT IF EXISTS fk_test_cascade_ws`)
		exec(`ALTER TABLE workflow_promises DROP CONSTRAINT IF EXISTS fk_test_cascade_wp`)
		exec(`ALTER TABLE concurrency_keys DROP CONSTRAINT IF EXISTS fk_test_cascade_ck`)
		exec(`ALTER TABLE workflow_update_requests DROP CONSTRAINT IF EXISTS fk_test_cascade_wu`)
	case testutil.DialectMySQL:
		for _, tbl := range []string{"event_history", "workflow_signals", "workflow_promises", "workflow_update_requests", "concurrency_keys"} {
			cnameQuery := "SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE TABLE_NAME = '" + tbl + "' AND CONSTRAINT_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME = 'workflow_instances'"
			rows, err := db.Query(cnameQuery)
			if err != nil {
				t.Logf("remove MySQL CASCADE FK for %s (non-fatal): %v", tbl, err)
				continue
			}
			var cname string
			for rows.Next() {
				if err := rows.Scan(&cname); err != nil {
					t.Logf("remove MySQL CASCADE FK for %s (non-fatal): %v", tbl, err)
					break
				}
				exec("ALTER TABLE " + tbl + " DROP FOREIGN KEY " + cname)
			}
			rows.Close()
		}
	case testutil.DialectMSSQL:
		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_event_history_workflow') ALTER TABLE event_history DROP CONSTRAINT fk_event_history_workflow`)
		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_signals_workflow') ALTER TABLE workflow_signals DROP CONSTRAINT fk_signals_workflow`)
		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_promises_workflow') ALTER TABLE workflow_promises DROP CONSTRAINT fk_promises_workflow`)
		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_concurrency_keys_workflow') ALTER TABLE concurrency_keys DROP CONSTRAINT fk_concurrency_keys_workflow`)
		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_update_requests_workflow') ALTER TABLE workflow_update_requests DROP CONSTRAINT fk_update_requests_workflow`)
	}
}
