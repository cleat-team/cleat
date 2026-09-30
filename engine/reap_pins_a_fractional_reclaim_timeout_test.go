// reap_pins_a_fractional_reclaim_timeout_test.go — per-dialect real-database
// coverage for cleat#2189: ReapStaleInstances truncated its reclaim timeout
// to whole seconds on all three dialects (PG: "%d seconds" over
// int(timeout.Seconds()); MySQL: INTERVAL ? SECOND over int(...); MSSQL:
// DATEADD(SECOND, ...) over int(...)), which halves #2166's 1s
// reclaimSlack at the default 14.5s reclaim-after and leaves 0.1s of slack
// at --heartbeat 2.9s. #2180 fixed the same truncation in StaleSetShape
// only, so the stall detector's Stale count (ms-precise) and the reap
// statement (second-truncated) disagreed about which rows were
// reclaimable.
//
// Each dialect seeds two rows against one fractional R (14.9s): one aged
// 14.1s -- inside R, must stay 'running' -- and one aged 15.9s -- past R,
// must be reclaimed to 'ready'. Pre-fix, int(14.9s) truncates to a 14s
// cutoff, so the 14.1s row (14.1 > 14) was wrongly reclaimed too -- this is
// the case that distinguishes the fix from the bug: the inside row must sit
// strictly inside (floor(R), R), not merely inside (0, R).
//
// cleat#2197: this margin was originally 300ms either side of R (14.2s /
// 14.8s) -- narrow enough that a slow CI runner, or an MSSQL deadlock retry
// (withRollbackGuaranteedRetry can re-run the whole reap statement) eating
// more than 300ms between the seed INSERT and the reap query flips the
// result, independent of whether the fix itself is correct.
//
// cleat-review on cleat#2751: a first widening moved BOTH rows a full
// second, to 13.5s/15.5s -- which moved the inside row from 14.2s (above
// the truncated 14s cutoff, where a truncating bug misclassifies it) to
// 13.5s (below 14s, where a truncating bug classifies it correctly too).
// Measured with `timeout = timeout.Truncate(time.Second)` reintroduced into
// PostgresStore.ReapStaleInstances: the 13.5s/15.5s version PASSED against
// that mutant -- #2189's own bug went uncaught. The flake risk is one-sided
// (only the inside row's ACTUAL age at reap time, seed-time age plus
// whatever elapses before the query runs, can cross above R -- the past
// row can only get more obviously stale), so the fix raises R's fraction
// instead of lowering the inside row: R=14.9s keeps the inside row at
// 14.1s, which stays above the 14s truncated cutoff (where the mutant must
// misclassify it) with 800ms of headroom below R itself (where slow
// CI or a retry would otherwise flip a correct implementation's answer,
// same hazard the original 300ms margin had, now 800ms wide instead).
package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

const (
	reapFractionalR         = 14900 * time.Millisecond // 14.9s
	reapFractionalInsideR   = 14100 * time.Millisecond // 14.1s stale: must NOT reclaim (above the 14s truncated cutoff)
	reapFractionalPastR     = 15900 * time.Millisecond // 15.9s stale: must reclaim
	reapFractionalTestLimit = 10
)

func deployReapFractionalTestDef(t *testing.T, ctx context.Context, store WorkflowStore) {
	t.Helper()
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: "reap-fractional-def", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy def: %v", err)
	}
}

func assertReapFractionalStatuses(t *testing.T, dialect string, db *sql.DB, ctx context.Context, placeholder func(int) string, insideID, pastID string) {
	t.Helper()
	statusOf := func(id string) string {
		var status string
		q := fmt.Sprintf("SELECT status FROM workflow_instances WHERE id = %s", placeholder(1))
		if err := db.QueryRowContext(ctx, q, id).Scan(&status); err != nil {
			t.Fatalf("%s: reading status of %s: %v", dialect, id, err)
		}
		return status
	}
	if got := statusOf(insideID); got != "running" {
		t.Errorf("%s: the 14.1s-stale row (inside R=14.9s) status = %q, want running -- "+
			"a whole-second-truncated reap would cut off at 14s and wrongly reclaim this row", dialect, got)
	}
	if got := statusOf(pastID); got != "ready" {
		t.Errorf("%s: the 15.9s-stale row (past R=14.9s) status = %q, want ready -- it should have been reclaimed", dialect, got)
	}
}

// --- PostgreSQL -------------------------------------------------------

func TestReapStaleInstancesPinsAFractionalReclaimTimeoutOnPostgres(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	defer db.Close()

	ctx := context.Background()
	store := NewPostgresStore(db)
	deployReapFractionalTestDef(t, ctx, store)

	insideID := fmt.Sprintf("reap-fractional-pg-inside-%d", time.Now().UnixNano())
	pastID := fmt.Sprintf("reap-fractional-pg-past-%d", time.Now().UnixNano())
	seed := func(id string, age time.Duration) {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input,
			                                assigned_to, heartbeat_at, tenant_id)
			VALUES ($1, 'reap-fractional-def', 1, 'running', '{}', 'reap-fractional-worker',
			        now() - make_interval(secs => $2::float8), $3)`,
			id, age.Seconds(), store.tenantID); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}
	seed(insideID, reapFractionalInsideR)
	seed(pastID, reapFractionalPastR)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM workflow_instances WHERE id IN ($1, $2)`, insideID, pastID)
	})

	n, err := store.ReapStaleInstances(ctx, reapFractionalR, reapFractionalTestLimit)
	if err != nil {
		t.Fatalf("ReapStaleInstances: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReapStaleInstances reclaimed %d rows, want 1 (only the 15.9s-stale row)", n)
	}
	assertReapFractionalStatuses(t, "postgres", db, ctx, func(i int) string { return fmt.Sprintf("$%d", i) }, insideID, pastID)
}

// --- MySQL --------------------------------------------------------------

func TestReapStaleInstancesPinsAFractionalReclaimTimeoutOnMySQL(t *testing.T) {
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
	deployReapFractionalTestDef(t, ctx, store)

	insideID := fmt.Sprintf("reap-fractional-my-inside-%d", time.Now().UnixNano())
	pastID := fmt.Sprintf("reap-fractional-my-past-%d", time.Now().UnixNano())
	seed := func(id string, age time.Duration) {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input,
			                                assigned_to, heartbeat_at, tenant_id)
			VALUES (?, 'reap-fractional-def', 1, 'running', '{}', 'reap-fractional-worker',
			        NOW(6) - INTERVAL ? MICROSECOND, ?)`,
			id, age.Microseconds(), store.tenantID); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}
	seed(insideID, reapFractionalInsideR)
	seed(pastID, reapFractionalPastR)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM workflow_instances WHERE id IN (?, ?)`, insideID, pastID)
	})

	n, err := store.ReapStaleInstances(ctx, reapFractionalR, reapFractionalTestLimit)
	if err != nil {
		t.Fatalf("ReapStaleInstances: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReapStaleInstances reclaimed %d rows, want 1 (only the 15.9s-stale row)", n)
	}
	assertReapFractionalStatuses(t, "mysql", db, ctx, func(int) string { return "?" }, insideID, pastID)
}

// --- SQL Server -----------------------------------------------------------

func TestReapStaleInstancesPinsAFractionalReclaimTimeoutOnMSSQL(t *testing.T) {
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
	store := NewMSSQLStore(db)
	deployReapFractionalTestDef(t, ctx, store)

	// admin bypasses RLS for the raw seed/read, same as
	// stale_set_shape_integration_test.go's MSSQL arm -- db itself is
	// subject to the same tenant-scoping policy ReapStaleInstances relies
	// on, so a raw statement over db sees nothing.
	admin := testutil.MSSQLAdminDB(t, db)

	insideID := fmt.Sprintf("reap-fractional-mssql-inside-%d", time.Now().UnixNano())
	pastID := fmt.Sprintf("reap-fractional-mssql-past-%d", time.Now().UnixNano())
	seed := func(id string, age time.Duration) {
		if _, err := admin.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input,
			                                assigned_to, heartbeat_at, tenant_id)
			VALUES (@p1, 'reap-fractional-def', 1, 'running', '{}', 'reap-fractional-worker',
			        DATEADD(MILLISECOND, -@p2, SYSUTCDATETIME()), @p3)`,
			id, age.Milliseconds(), store.tenantID); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}
	seed(insideID, reapFractionalInsideR)
	seed(pastID, reapFractionalPastR)
	t.Cleanup(func() {
		admin.Exec(`DELETE FROM workflow_instances WHERE id IN (@p1, @p2)`, insideID, pastID)
	})

	n, err := store.ReapStaleInstances(ctx, reapFractionalR, reapFractionalTestLimit)
	if err != nil {
		t.Fatalf("ReapStaleInstances: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReapStaleInstances reclaimed %d rows, want 1 (only the 15.9s-stale row)", n)
	}
	assertReapFractionalStatuses(t, "mssql", admin, ctx, func(i int) string { return fmt.Sprintf("@p%d", i) }, insideID, pastID)
}
