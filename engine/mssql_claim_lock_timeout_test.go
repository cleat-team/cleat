package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestMSSQLClaimStep4LockTimeoutBoundsAGenuinelyLockedRow is cleat#1963's
// liveness fix, verified.
//
// Step 4's own UPDATE carries WITH (READPAST), but that is a fast path, not
// a guarantee: READPAST only skips a row whose conflict is visible at the
// point the UPDATE qualifies rows to update. This package's own
// investigation (see mssql_readpast_ambiguity_test.go and the dated
// correction on cleat#1963) found a holder shape READPAST cannot see --
// one that locks a row through ONLY a nonclustered claim index (a covering
// SELECT needing no columns outside that index's key/filter, so it never
// touches the clustered row) -- which blocks Step 4 indefinitely: the
// conflict is on a resource this UPDATE's own index maintenance touches
// only after it has already committed to updating that row, past the point
// READPAST's skip-check runs. TestZZScratch* scratch probes (not shipped)
// measured this directly: a real store.ClaimWorkflows call blocked 3/3,
// confirmed via sys.dm_tran_locks to be waiting specifically on the
// nonclustered leaf the holder query locks.
//
// The fix is SET LOCK_TIMEOUT around Step 4's UPDATE (mssqlClaimLockTimeoutMS
// in mssql_lifecycle.go), which bounds the wait whatever the lock shape --
// this one, a lock escalation from a bulk retention delete (cleat#2060), or
// anything unforeseen -- rather than trying to make READPAST see every case.
// On SQL Server error 1222 ("Lock request time out period exceeded"),
// claimWorkflowsOnce treats it as "no claim this round", not an error: the
// whole transaction rolls back (nothing was committed, so nothing needs
// releasing -- Step 3's concurrency-key/queue-holder admissions for this
// attempt are undone with it), and ClaimWorkflows' own poll loop retries on
// its normal schedule.
//
// This holder deliberately uses the SAME unscoped covered-SELECT shape as
// mssql_readpast_ambiguity_test.go's own holder (status/next_wake_at only,
// no `id =` restriction) rather than a scoped one -- that is the shape
// proven to defeat READPAST here; a scoped holder (locking the clustered row
// too) is already handled by READPAST's fast path and would not exercise
// this fix.
func TestMSSQLClaimStep4LockTimeoutBoundsAGenuinelyLockedRow(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	testutil.CleanupMSSQLTestData(t, db)
	t.Cleanup(func() { testutil.CleanupMSSQLTestData(t, db) })

	store := openMSSQLTenantStore(t, DefaultTenantUUID)
	ctx := context.Background()
	setupTestData(t, store)

	const n = 4
	var ids []string
	for i := 0; i < n; i++ {
		id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
			json.RawMessage(`{}`), fmt.Sprintf("locktimeout-%d-%d", i, time.Now().UnixNano()),
			DefaultTenantUUID, 0)
		if err != nil {
			t.Fatalf("StartNewRun[%d]: %v", i, err)
		}
		ids = append(ids, id)
	}

	// Hold every seeded row via the unscoped covered SELECT -- an NC-only
	// lock, invisible to Step 4's READPAST fast path.
	holderConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer holderConn.Close()
	holderTx, err := holderConn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer holderTx.Rollback()
	if _, err := holderTx.ExecContext(ctx,
		`EXEC sp_set_session_context @key=N'tenant_id', @value=@p1`, DefaultTenantUUID); err != nil {
		t.Fatalf("holder session context: %v", err)
	}
	holderRows, err := holderTx.QueryContext(ctx, `
		SELECT id FROM workflow_instances WITH (UPDLOCK, ROWLOCK)
		WHERE status IN ('ready','terminating') AND next_wake_at <= SYSUTCDATETIME()`)
	if err != nil {
		t.Fatalf("holder lock: %v", err)
	}
	var locked int
	for holderRows.Next() {
		locked++
	}
	holderRows.Close()
	// KNOWN-POSITIVE for the holder itself: it must actually lock something,
	// or a fast claim would mean nothing about the fix.
	if locked < n {
		t.Fatalf("holder locked %d row(s), want at least %d -- the holder's own SELECT did not "+
			"lock what this test seeded", locked, n)
	}

	// Hold well past mssqlClaimLockTimeoutMS so the claim genuinely bounds
	// on the timeout rather than racing a fast holder release.
	holdFor := 3 * time.Second
	holderDone := make(chan struct{})
	go func() {
		time.Sleep(holdFor)
		holderTx.Rollback()
		close(holderDone)
	}()

	start := time.Now()
	claimed, err := store.ClaimWorkflows(ctx, "worker-locktimeout", 10)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("ClaimWorkflows claimed %d workflow(s) while the holder held every candidate row -- "+
			"it should have returned cleanly with none, not claimed a genuinely-locked row", len(claimed))
	}
	margin := 2 * time.Second
	bound := mssqlClaimLockTimeoutMS
	if elapsed > time.Duration(bound)*time.Millisecond+margin {
		t.Fatalf("ClaimWorkflows took %v to return 0 claims, want under LOCK_TIMEOUT (%dms) plus %v margin -- "+
			"this is the liveness bound the fix exists to provide", elapsed, bound, margin)
	}
	if elapsed >= holdFor {
		t.Fatalf("ClaimWorkflows took %v, at or past the holder's %v hold -- it wasn't bounded by "+
			"LOCK_TIMEOUT at all, it waited for the holder to release, which is the pre-fix behavior", elapsed, holdFor)
	}
	t.Logf("ClaimWorkflows returned 0 claims in %v (LOCK_TIMEOUT=%dms), well before the holder's %v release",
		elapsed, bound, holdFor)

	// No leak: this attempt's transaction rolled back on the lock timeout,
	// so nothing Step 3 admitted should have survived. Both tables carry an
	// RLS filter predicate (migrations/mssql/001_schema.sql), so this must
	// go through a tenant_id-scoped connection -- a plain one silently
	// matches zero rows regardless of what is actually there.
	checkConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("leak-check conn: %v", err)
	}
	defer checkConn.Close()
	if _, err := checkConn.ExecContext(ctx,
		`EXEC sp_set_session_context @key=N'tenant_id', @value=@p1`, DefaultTenantUUID); err != nil {
		t.Fatalf("leak-check session context: %v", err)
	}
	for _, table := range []string{"concurrency_keys", "queue_holders"} {
		var count int
		q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE workflow_id IN (
			SELECT value FROM STRING_SPLIT(@p1, ','))`, table)
		if err := checkConn.QueryRowContext(ctx, q, strings.Join(ids, ",")).Scan(&count); err != nil {
			t.Fatalf("checking %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s has %d row(s) for the skipped ids after a lock-timeout skip, want 0 -- "+
				"the rolled-back attempt leaked an admission it never actually committed", table, count)
		}
	}

	<-holderDone

	// The next poll claims normally once the holder is gone.
	claimed2, err := store.ClaimWorkflows(ctx, "worker-locktimeout-2", 10)
	if err != nil {
		t.Fatalf("second ClaimWorkflows: %v", err)
	}
	if len(claimed2) < n {
		t.Fatalf("second ClaimWorkflows claimed %d workflow(s), want at least %d -- the previously-skipped "+
			"rows should be claimable normally once the holder released them", len(claimed2), n)
	}
}
