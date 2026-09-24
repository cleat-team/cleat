package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestHeartbeatReservedPoolSeesARealClaimedRunOnAllThreeDialects is
// cleat#2192's must-have regression test, per cleat-review's review of the
// existing TestHeartbeatBatchFencedIsolatedFromASaturatedExecutionPool
// (cmd/cleat-worker/heartbeat_pool_isolation_test.go): "It's PG-only, uses a
// nonexistent run ID, and never checks lost." That test still exercises pool
// SATURATION -- it stays -- but it cannot see a store pointed at the wrong
// database, because a nonexistent run is lost on every store equally.
//
// This deploys a real workflow def, starts and claims a REAL run through
// factory.OpenStore -- the same call cmd/cleat-worker/main.go uses to build
// its execution store on all three dialects -- then heartbeats that run
// through factory.OpenIsolatedStore, the exact call cleat#2009's reserved
// pool makes, and asserts the run comes back NOT lost.
//
// MySQL carries a second assertion the other two dialects don't need: see
// its subtest below for why, and for the negative control that proves the
// positive assertion is actually discriminating.
func TestHeartbeatReservedPoolSeesARealClaimedRunOnAllThreeDialects(t *testing.T) {
	t.Run("postgres", testHeartbeatReservedPoolSeesARealClaimedRunPostgres)
	t.Run("mysql", testHeartbeatReservedPoolSeesARealClaimedRunMySQL)
	t.Run("mssql", testHeartbeatReservedPoolSeesARealClaimedRunMSSQL)
}

// deployStartAndClaim deploys a minimal def, starts one run under it, and
// claims it -- returning the claimed instance so the caller has a real
// (id, generation) pair to heartbeat, rather than the fixed nonexistent ID
// cleat-review flagged in the saturation test.
func deployStartAndClaim(t *testing.T, store WorkflowStore, defName, idempotencyKey, workerID string) *WorkflowInstance {
	t.Helper()
	ctx := context.Background()
	def := &WorkflowDef{
		Name:       defName,
		Version:    1,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d}, // minimal WASM magic
		ABIVersion: 1,
		MinVersion: 1,
	}
	if err := store.DeployWorkflowDef(ctx, def); err != nil {
		t.Fatalf("DeployWorkflowDef(%s): %v", defName, err)
	}
	runID, alreadyExisted, err := store.StartNewRun(ctx, "", defName, 1,
		json.RawMessage(`{}`), idempotencyKey, DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	if alreadyExisted {
		t.Fatalf("StartNewRun returned alreadyExisted=true for key %q -- a leftover row from a "+
			"previous run of this test was not cleaned up", idempotencyKey)
	}
	wf, err := store.ClaimWorkflow(ctx, workerID)
	if err != nil {
		t.Fatalf("ClaimWorkflow(%s): %v", workerID, err)
	}
	if wf == nil {
		t.Fatal("ClaimWorkflow returned nil, expected the run just started")
	}
	if wf.ID != runID {
		t.Fatalf("ClaimWorkflow claimed %s, want %s -- another ready row was still lying around", wf.ID, runID)
	}
	return wf
}

// assertNotLost heartbeats wf through store and fails if it comes back
// reported lost, or if the call errors outright.
func assertNotLost(t *testing.T, store WorkflowStore, workerID string, wf *WorkflowInstance, label string) {
	t.Helper()
	lost, err := store.HeartbeatBatchFenced(context.Background(), workerID,
		[]GenerationKey{{WorkflowID: wf.ID, Generation: wf.Generation}})
	if err != nil {
		t.Fatalf("%s: HeartbeatBatchFenced: %v", label, err)
	}
	if len(lost) != 0 {
		t.Fatalf("%s: HeartbeatBatchFenced reported %v lost, want none -- the reserved pool did "+
			"not see the row the execution store just claimed", label, lost)
	}
}

func testHeartbeatReservedPoolSeesARealClaimedRunPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping a real heartbeat-reserved-pool reproduction in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	applyPostgresProcedures(t, db)
	testutil.CleanupPostgresTestData(t, db)
	defer func() {
		testutil.CleanupPostgresTestData(t, db)
		db.Close()
	}()

	factory := NewPostgresStoreFactory(db, "public").WithDSN(testutil.PostgresTestDSN())
	ctx := context.Background()

	execStore, execCloser, err := factory.OpenStore(ctx, DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer execCloser.Close()

	wf := deployStartAndClaim(t, execStore, "hb-reserved-pool-pg", "hb-reserved-pool-pg-key", "hb-reserved-pool-pg-worker")

	heartbeatStore, closer, err := factory.OpenIsolatedStore(ctx, DefaultTenantUUID, 2, "default")
	if err != nil {
		t.Fatalf("OpenIsolatedStore: %v", err)
	}
	defer closer.Close()

	assertNotLost(t, heartbeatStore, "hb-reserved-pool-pg-worker", wf, "postgres reserved pool")
}

// testHeartbeatReservedPoolSeesARealClaimedRunMySQL is the dialect this test
// actually exists for. MySQL has no RLS -- a tenant is isolated by which
// PHYSICAL DATABASE a connection is pointed at (MySQLTenantDatabaseName), not
// by a predicate a shared pool can apply per query. Before this fix,
// cmd/cleat-worker/main.go opened the heartbeat pool with a bare
// sql.Open(driver, baseDSN): that connects to the BASE database, not the
// tenant's cleat_<id> one, so a heartbeat through it could not see a row that
// lived in a different physical database and reported every run lost on its
// first heartbeat -- cancelling every in-flight MySQL execution by design.
//
// The positive half (through factory.OpenIsolatedStore) mirrors the other two
// dialects. The negative half reconstructs the pre-fix bare pool by hand and
// asserts it DOES report the run lost -- the falsification cleat-review asked
// for, made a standing part of the suite instead of a one-off manual check,
// so a regression back to the bare-sql.Open shape is caught automatically.
func testHeartbeatReservedPoolSeesARealClaimedRunMySQL(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL reserved-pool reproduction")
	}
	if testing.Short() {
		t.Skip("Skipping a real heartbeat-reserved-pool reproduction in short mode")
	}
	db := openMySQLTestDB(t)
	testutil.SetupMySQLFullSchema(t, db)
	applyMySQLProcedures(t, db)
	testutil.CleanupMySQLTestData(t, db)
	defer func() {
		testutil.CleanupMySQLTestData(t, db)
		db.Close()
	}()

	baseDSN := mysqlTestBaseDSN(t)
	factory := NewMySQLStoreFactory(db, baseDSN)
	ctx := context.Background()

	// MySQL's per-tenant DATABASE needs its own copy of the schema -- it is
	// a separate physical database from the one SetupMySQLFullSchema(t, db)
	// just set up. cmd/cleat-worker/main.go does this too (the `if *driver ==
	// "mysql"` re-migration block, right after the shared migration run),
	// via the identical factory.TenantDB(ctx, defaultTenantID) call.
	tenantDB, err := factory.TenantDB(ctx, DefaultTenantUUID)
	if err != nil {
		t.Fatalf("TenantDB: %v", err)
	}
	testutil.SetupMySQLFullSchema(t, tenantDB)
	applyMySQLProcedures(t, tenantDB)
	testutil.CleanupMySQLTestData(t, tenantDB)
	defer testutil.CleanupMySQLTestData(t, tenantDB)

	execStore, execCloser, err := factory.OpenStore(ctx, DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer execCloser.Close()

	wf := deployStartAndClaim(t, execStore, "hb-reserved-pool-mysql", "hb-reserved-pool-mysql-key", "hb-reserved-pool-mysql-worker")

	// Positive: through the factory, on the tenant's own database.
	heartbeatStore, closer, err := factory.OpenIsolatedStore(ctx, DefaultTenantUUID, 2, "default")
	if err != nil {
		t.Fatalf("OpenIsolatedStore: %v", err)
	}
	defer closer.Close()
	assertNotLost(t, heartbeatStore, "hb-reserved-pool-mysql-worker", wf, "mysql reserved pool (via factory)")

	// Negative control: a bare pool opened directly against CLEAT_TEST_MYSQL
	// as-is -- the full DSN, database name and all -- exactly what
	// sql.Open(sqlDriver, *dbURL) did in main.go before this fix. Not
	// baseDSN: that has the database name stripped and would fail to
	// connect to ANY database at all, which is a different failure than the
	// one this test exists to reproduce (connecting to the WRONG one).
	// This MUST fail to see the row; if it doesn't, the positive assertion
	// above proves nothing.
	baseDB, err := sql.Open("mysql", os.Getenv("CLEAT_TEST_MYSQL"))
	if err != nil {
		t.Fatalf("open base-database pool: %v", err)
	}
	defer baseDB.Close()
	brokenStore := NewMySQLStore(baseDB)
	brokenLost, err := brokenStore.HeartbeatBatchFenced(ctx, "hb-reserved-pool-mysql-worker",
		[]GenerationKey{{WorkflowID: wf.ID, Generation: wf.Generation}})
	if err != nil {
		t.Fatalf("negative control: HeartbeatBatchFenced through the base-database pool: %v", err)
	}
	if len(brokenLost) != 1 || brokenLost[0] != wf.ID {
		t.Fatalf("negative control: HeartbeatBatchFenced through a bare base-database pool reported "+
			"lost=%v, want [%s] -- if this is empty, the base database can somehow see the tenant's "+
			"row and the positive assertion above is not discriminating anything", brokenLost, wf.ID)
	}
}

func testHeartbeatReservedPoolSeesARealClaimedRunMSSQL(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server reserved-pool reproduction")
	}
	if testing.Short() {
		t.Skip("Skipping a real heartbeat-reserved-pool reproduction in short mode")
	}
	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	applyMSSQLProcedures(t, db)
	testutil.CleanupMSSQLTestData(t, db)
	defer func() {
		testutil.CleanupMSSQLTestData(t, db)
		db.Close()
	}()

	connStr := os.Getenv("CLEAT_TEST_MSSQL")
	factory := NewMSSQLStoreFactory(connStr)
	ctx := context.Background()

	execStore, execCloser, err := factory.OpenStore(ctx, DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer execCloser.Close()

	wf := deployStartAndClaim(t, execStore, "hb-reserved-pool-mssql", "hb-reserved-pool-mssql-key", "hb-reserved-pool-mssql-worker")

	heartbeatStore, closer, err := factory.OpenIsolatedStore(ctx, DefaultTenantUUID, 2, "default")
	if err != nil {
		t.Fatalf("OpenIsolatedStore: %v", err)
	}
	defer closer.Close()

	assertNotLost(t, heartbeatStore, "hb-reserved-pool-mssql-worker", wf, "mssql reserved pool")
}
