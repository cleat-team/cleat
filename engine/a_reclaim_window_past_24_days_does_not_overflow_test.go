// a_reclaim_window_past_24_days_does_not_overflow_test.go — cleat#2197: SQL
// Server's DATEADD takes an `int` for its number argument, so a single
// DATEADD(MILLISECOND, …) overflows past about 24.86 days (2^31 ms).
// ReapStaleInstances and StaleSetShape both moved to millisecond precision
// in cleat#2194/#2180 (for a fractional-second reclaim timeout) and
// reintroduced that ceiling -- before it, the equivalent seconds-only form
// overflowed only at about 68 years.
//
// --reclaim-timeout has no upper bound (validateReclaimTimeout only refuses
// values that are too SMALL), so an operator setting something like 720h to
// mean "practically never reclaim" got a reaper that errored on every tick,
// with #2180's fail-closed probe then skipping the tick too -- a config
// error converted into a silent no-reap by the exact mechanism meant to make
// a failure loud.
//
// R=30 days is comfortably past the ~24.86-day ceiling and comfortably
// short of the ~68-year one restored by splitMillisecondOffset
// (mssql_operations.go), so a regression here is unambiguous: it can only
// mean the split arithmetic broke, not that some other boundary was
// approached by accident.
package engine

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestAReclaimWindowPast24DaysDoesNotOverflowOnMSSQL(t *testing.T) {
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
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: "reclaim-window-overflow-def", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy def: %v", err)
	}

	// admin bypasses RLS for the raw seed, same as the fractional-reclaim
	// test's MSSQL arm -- db itself is subject to the tenant-scoping policy
	// ReapStaleInstances/StaleSetShape rely on.
	admin := testutil.MSSQLAdminDB(t, db)

	const r30Days = 30 * 24 * time.Hour

	// Pre-fix, both statements below fail with "Arithmetic overflow error
	// converting expression to data type int. (8115)" before ever reaching
	// a row -- so a seeded, genuinely-stale row is what makes this a
	// regression test for the OVERFLOW rather than only a syntax check: it
	// proves the split arithmetic still reclaims correctly, not merely that
	// it avoids erroring on an empty result set.
	staleID := fmt.Sprintf("reclaim-overflow-stale-%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input,
		                                assigned_to, heartbeat_at, tenant_id)
		VALUES (@p1, 'reclaim-window-overflow-def', 1, 'running', '{}', 'reclaim-overflow-worker',
		        DATEADD(DAY, -31, SYSUTCDATETIME()), @p2)`,
		staleID, store.tenantID); err != nil {
		t.Fatalf("seeding %s: %v", staleID, err)
	}
	t.Cleanup(func() {
		admin.Exec(`DELETE FROM workflow_instances WHERE id = @p1`, staleID)
	})

	// StaleSetShape first: a read, so if this overflows the reap below would
	// too, and separating them tells the two statements' fixes apart --
	// reap only touches its own single DATEADD, StaleSetShape has two
	// independent ones (missed-beat and stale) in one query.
	shape, err := store.StaleSetShape(ctx, r30Days, r30Days)
	if err != nil {
		t.Fatalf("StaleSetShape at R=30 days: %v\n\n"+
			"cleat#2197: DATEADD(MILLISECOND, …) overflows past ~24.86 days "+
			"(2^31 ms) unless split into whole seconds plus a millisecond "+
			"remainder -- see splitMillisecondOffset.", err)
	}
	if shape.Stale < 1 {
		t.Errorf("StaleSetShape at R=30 days reported Stale=%d, want at least 1 "+
			"(the seeded 31-day-old row) -- it should have counted the row, not just avoided erroring on it",
			shape.Stale)
	}

	n, err := store.ReapStaleInstances(ctx, r30Days, 10)
	if err != nil {
		t.Fatalf("ReapStaleInstances at R=30 days: %v\n\n"+
			"cleat#2197: DATEADD(MILLISECOND, …) overflows past ~24.86 days "+
			"(2^31 ms) unless split into whole seconds plus a millisecond "+
			"remainder -- see splitMillisecondOffset.", err)
	}
	if n != 1 {
		t.Fatalf("ReapStaleInstances at R=30 days reclaimed %d rows, want 1 (the seeded 31-day-old row)", n)
	}

	var status string
	if err := admin.QueryRowContext(ctx,
		`SELECT status FROM workflow_instances WHERE id = @p1`, staleID).Scan(&status); err != nil {
		t.Fatalf("reading status of %s: %v", staleID, err)
	}
	if status != "ready" {
		t.Errorf("seeded row's status = %q after reap, want ready", status)
	}
}

// TestAReclaimWindowPast24DaysDoesNotOverflowOnMSSQLStaleHolderReaper is the
// same regression as the test above, for cleat#2196's two new methods.
// cleat-review2 found that ListStaleHolders and reapStaleInstancesExceptOnce
// each copied the OLD single-DATEADD(MILLISECOND, ...) form rather than the
// sibling reapStaleInstancesOnce's two-part split, at the time this file was
// first written for #2196 -- reproduced live against a real MSSQL 2022
// instance at a ~68-year offset ("Arithmetic overflow error converting
// expression to data type int"), fixed by reusing splitMillisecondOffset in
// both. R=30 days is enough to prove the regression without waiting for 68
// years of wall-clock plausibility to matter.
func TestAReclaimWindowPast24DaysDoesNotOverflowOnMSSQLStaleHolderReaper(t *testing.T) {
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
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: "reclaim-window-overflow-holders-def", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy def: %v", err)
	}

	admin := testutil.MSSQLAdminDB(t, db)

	const r30Days = 30 * 24 * time.Hour

	staleID := fmt.Sprintf("reclaim-overflow-holders-stale-%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input,
		                                assigned_to, heartbeat_at, tenant_id, generation)
		VALUES (@p1, 'reclaim-window-overflow-holders-def', 1, 'running', '{}', 'reclaim-overflow-holders-worker',
		        DATEADD(DAY, -31, SYSUTCDATETIME()), @p2, 1)`,
		staleID, store.tenantID); err != nil {
		t.Fatalf("seeding %s: %v", staleID, err)
	}
	t.Cleanup(func() {
		admin.Exec(`DELETE FROM workflow_instances WHERE id = @p1`, staleID)
	})

	holders, err := store.ListStaleHolders(ctx, r30Days, 10)
	if err != nil {
		t.Fatalf("ListStaleHolders at R=30 days: %v\n\n"+
			"cleat#2197: DATEADD(MILLISECOND, …) overflows past ~24.86 days "+
			"(2^31 ms) unless split into whole seconds plus a millisecond "+
			"remainder -- see splitMillisecondOffset.", err)
	}
	found := false
	for _, h := range holders {
		if h.Key.WorkflowID == staleID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListStaleHolders at R=30 days did not report %s; got %+v", staleID, holders)
	}

	n, err := store.ReapStaleInstancesExcept(ctx, r30Days, 10, nil)
	if err != nil {
		t.Fatalf("ReapStaleInstancesExcept at R=30 days: %v\n\n"+
			"cleat#2197: DATEADD(MILLISECOND, …) overflows past ~24.86 days "+
			"(2^31 ms) unless split into whole seconds plus a millisecond "+
			"remainder -- see splitMillisecondOffset.", err)
	}
	if n != 1 {
		t.Fatalf("ReapStaleInstancesExcept at R=30 days reclaimed %d rows, want 1 (the seeded 31-day-old row)", n)
	}

	var status string
	if err := admin.QueryRowContext(ctx,
		`SELECT status FROM workflow_instances WHERE id = @p1`, staleID).Scan(&status); err != nil {
		t.Fatalf("reading status of %s: %v", staleID, err)
	}
	if status != "ready" {
		t.Errorf("seeded row's status = %q after reap, want ready", status)
	}
}
