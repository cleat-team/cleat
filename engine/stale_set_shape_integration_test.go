// stale_set_shape_integration_test.go — per-dialect real-database coverage
// for DBStallDetector.StaleSetShape (cleat#2006).
//
// cleat-review on cleat#2006 (2026-09-24): every existing test of
// suspectedDBStall's wiring drove a mock DBStallDetector, so nothing in the
// suite ever executed a real dialect's SQL. MSSQLStore.StaleSetShape ran
// over s.db directly, with no SESSION_CONTEXT set -- under the shipped RLS
// security policies (fn_tenant_filter) a statement with no session context
// matches no rows, so it silently reported Running: 0, err: nil on every
// SQL Server call, and the suspected-stall detector could never fire on
// that dialect. Nothing in the suite noticed, because nothing in the suite
// ran the real query. These tests do.
package engine

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// staleSetShapeExpect is what one dialect's StaleSetShape call is checked
// against. Kept as a struct (not five separate assertions inline) so the
// three dialect tests below read as the same shape-check called three ways.
type staleSetShapeExpect struct {
	running            int
	missedBeat         int
	distinctAssignedTo int
	noRecentHeartbeat  bool
	stale              int
}

func assertStaleSetShape(t *testing.T, dialect string, got StaleSetShape, want staleSetShapeExpect) {
	t.Helper()
	if got.Running != want.running {
		t.Errorf("%s: StaleSetShape.Running = %d, want %d -- if this is 0 with every other field also zero and no error, the read likely matched no rows at all (the exact MSSQL failure mode cleat-review found: a query with no error and no rows, because RLS/session context silently hid everything)", dialect, got.Running, want.running)
	}
	if got.MissedBeat != want.missedBeat {
		t.Errorf("%s: StaleSetShape.MissedBeat = %d, want %d", dialect, got.MissedBeat, want.missedBeat)
	}
	if got.DistinctAssignedTo != want.distinctAssignedTo {
		t.Errorf("%s: StaleSetShape.DistinctAssignedTo = %d, want %d", dialect, got.DistinctAssignedTo, want.distinctAssignedTo)
	}
	if got.NoRecentHeartbeat != want.noRecentHeartbeat {
		t.Errorf("%s: StaleSetShape.NoRecentHeartbeat = %v, want %v", dialect, got.NoRecentHeartbeat, want.noRecentHeartbeat)
	}
	if got.Stale != want.stale {
		t.Errorf("%s: StaleSetShape.Stale = %d, want %d", dialect, got.Stale, want.stale)
	}
}

// deployStaleShapeTestDef deploys one workflow def every raw INSERT below
// depends on -- workflow_instances carries FOREIGN KEY (tenant_id,
// def_name, def_version) REFERENCES workflow_defs on all three dialects.
func deployStaleShapeTestDef(t *testing.T, ctx context.Context, store WorkflowStore) {
	t.Helper()
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: "stale-shape-def", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy def: %v", err)
	}
}

// --- PostgreSQL -------------------------------------------------------

func TestStaleSetShapePostgresAgainstARealDatabase(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	defer db.Close()

	ctx := context.Background()
	store := NewPostgresStore(db)
	deployStaleShapeTestDef(t, ctx, store)

	ids := make([]string, 3)
	for i, assignedTo := range []string{"worker-a", "worker-b", "worker-c"} {
		ids[i] = fmt.Sprintf("stale-shape-pg-%d-%d", time.Now().UnixNano(), i)
		ageSeconds := 0
		if i == 2 {
			ageSeconds = 3600 // one stale row
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input,
			                                assigned_to, heartbeat_at, tenant_id)
			VALUES ($1, 'stale-shape-def', 1, 'running', '{}', $2,
			        now() - make_interval(secs => $3), $4)`,
			ids[i], assignedTo, ageSeconds, store.tenantID); err != nil {
			t.Fatalf("seeding row %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
		}
	})

	// Two fresh, one stale, three distinct workers -- NoRecentHeartbeat
	// must read false: a survivor exists.
	shape, err := store.StaleSetShape(ctx, 2*time.Hour, 10*time.Second)
	if err != nil {
		t.Fatalf("StaleSetShape (mixed freshness): %v", err)
	}
	assertStaleSetShape(t, "postgres", shape, staleSetShapeExpect{
		running: 3, missedBeat: 1, distinctAssignedTo: 3, noRecentHeartbeat: false, stale: 0,
	})

	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_instances SET heartbeat_at = now() - interval '1 hour' WHERE id = ANY($1)`,
		pgTextArray(ids)); err != nil {
		t.Fatalf("age every row: %v", err)
	}
	shape, err = store.StaleSetShape(ctx, 2*time.Hour, 10*time.Second)
	if err != nil {
		t.Fatalf("StaleSetShape (all stale): %v", err)
	}
	assertStaleSetShape(t, "postgres", shape, staleSetShapeExpect{
		running: 3, missedBeat: 3, distinctAssignedTo: 3, noRecentHeartbeat: true, stale: 0,
	})
}

// --- MySQL --------------------------------------------------------------

func TestStaleSetShapeMySQLAgainstARealDatabase(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL integration test")
	}
	if testing.Short() {
		t.Skip("Skipping MySQL integration test in short mode")
	}
	db := openMySQLTestDB(t)
	testutil.SetupMySQLFullSchema(t, db)
	testutil.CleanupMySQLTestData(t, db)
	defer db.Close()

	ctx := context.Background()
	store := NewMySQLStore(db)
	deployStaleShapeTestDef(t, ctx, store)

	ids := make([]string, 3)
	for i, assignedTo := range []string{"worker-a", "worker-b", "worker-c"} {
		ids[i] = fmt.Sprintf("stale-shape-my-%d-%d", time.Now().UnixNano(), i)
		ageSeconds := 0
		if i == 2 {
			ageSeconds = 3600
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input,
			                                assigned_to, heartbeat_at, tenant_id)
			VALUES (?, 'stale-shape-def', 1, 'running', '{}', ?,
			        NOW(6) - INTERVAL ? SECOND, ?)`,
			ids[i], assignedTo, ageSeconds, store.tenantID); err != nil {
			t.Fatalf("seeding row %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			db.Exec(`DELETE FROM workflow_instances WHERE id = ?`, id)
		}
	})

	shape, err := store.StaleSetShape(ctx, 2*time.Hour, 10*time.Second)
	if err != nil {
		t.Fatalf("StaleSetShape (mixed freshness): %v", err)
	}
	assertStaleSetShape(t, "mysql", shape, staleSetShapeExpect{
		running: 3, missedBeat: 1, distinctAssignedTo: 3, noRecentHeartbeat: false, stale: 0,
	})

	for _, id := range ids {
		if _, err := db.ExecContext(ctx,
			`UPDATE workflow_instances SET heartbeat_at = NOW(6) - INTERVAL 1 HOUR WHERE id = ?`, id); err != nil {
			t.Fatalf("age row %s: %v", id, err)
		}
	}
	shape, err = store.StaleSetShape(ctx, 2*time.Hour, 10*time.Second)
	if err != nil {
		t.Fatalf("StaleSetShape (all stale): %v", err)
	}
	assertStaleSetShape(t, "mysql", shape, staleSetShapeExpect{
		running: 3, missedBeat: 3, distinctAssignedTo: 3, noRecentHeartbeat: true, stale: 0,
	})
}

// --- SQL Server -----------------------------------------------------------
//
// This is the dialect cleat-review's finding is actually about: before the
// beginTxWithContext fix, every assertion below failed the same way --
// Running: 0, every other field 0, err: nil -- because StaleSetShape ran
// with no SESSION_CONTEXT and the RLS policy matched nothing.

func TestStaleSetShapeMSSQLAgainstARealDatabase(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping MSSQL integration test")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}
	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	applyMSSQLProcedures(t, db)
	testutil.CleanupMSSQLTestData(t, db)
	defer db.Close()

	ctx := context.Background()
	// Deliberately the plain constructor, NOT the factory -- this is the
	// exact construction the bug hid behind. See MSSQLStore.StaleSetShape's
	// doc comment: it now sets SESSION_CONTEXT itself, per transaction,
	// same as ReapStaleInstances always has, so this bare pool is correct.
	store := NewMSSQLStore(db)
	deployStaleShapeTestDef(t, ctx, store)

	// admin is a session-context-bypass connection, the same one
	// setupMSSQLIntegrationTest uses for its own raw reads: `db` itself is
	// subject to the same RLS policies StaleSetShape's own fix is about, so
	// a raw seed/age/cleanup statement over `db` with no session context
	// sees (and affects) nothing -- discovered writing this test, when the
	// aging UPDATE below reported 0 rows affected against `db` directly.
	admin := testutil.MSSQLAdminDB(t, db)

	ids := make([]string, 3)
	for i, assignedTo := range []string{"worker-a", "worker-b", "worker-c"} {
		ids[i] = fmt.Sprintf("stale-shape-mssql-%d-%d", time.Now().UnixNano(), i)
		ageSeconds := 0
		if i == 2 {
			ageSeconds = 3600
		}
		if _, err := admin.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input,
			                                assigned_to, heartbeat_at, tenant_id)
			VALUES (@p1, 'stale-shape-def', 1, 'running', '{}', @p2,
			        DATEADD(SECOND, -@p3, SYSUTCDATETIME()), @p4)`,
			ids[i], assignedTo, ageSeconds, store.tenantID); err != nil {
			t.Fatalf("seeding row %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			admin.Exec(`DELETE FROM workflow_instances WHERE id = @p1`, id)
		}
	})

	shape, err := store.StaleSetShape(ctx, 2*time.Hour, 10*time.Second)
	if err != nil {
		t.Fatalf("StaleSetShape (mixed freshness): %v", err)
	}
	assertStaleSetShape(t, "mssql", shape, staleSetShapeExpect{
		running: 3, missedBeat: 1, distinctAssignedTo: 3, noRecentHeartbeat: false, stale: 0,
	})

	for _, id := range ids {
		res, err := admin.ExecContext(ctx,
			`UPDATE workflow_instances SET heartbeat_at = DATEADD(HOUR, -1, SYSUTCDATETIME()) WHERE id = @p1`, id)
		if err != nil {
			t.Fatalf("age row %s: %v", id, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("age row %s: %d rows affected, want 1 -- the row this test just seeded is not visible to the aging UPDATE", id, n)
		}
	}
	shape, err = store.StaleSetShape(ctx, 2*time.Hour, 10*time.Second)
	if err != nil {
		t.Fatalf("StaleSetShape (all stale): %v", err)
	}
	assertStaleSetShape(t, "mssql", shape, staleSetShapeExpect{
		running: 3, missedBeat: 3, distinctAssignedTo: 3, noRecentHeartbeat: true, stale: 0,
	})
}

