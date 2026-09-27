package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestMSSQLLockTimeoutDoesNotSurviveAPooledConnectionRecycle is the
// defense-in-depth half of cleat#1963's LOCK_TIMEOUT fix, proven rather than
// assumed per the coordinator's review of #2088.
//
// claimWorkflowsOnce resets LOCK_TIMEOUT -- see the comment above it in
// mssql_lifecycle.go, and resetLockTimeoutMSSQL's own doc comment (cleat#2148)
// for why that is no longer one unconditional defer. This test checks the
// layer underneath the whole mechanism, however it is implemented: if a
// caller EVER left LOCK_TIMEOUT set on a connection returned to the pool --
// a bug in this package, or a future caller with the same pattern that gets
// the ordering wrong -- would that connection poison whatever the pool hands
// it to next?
//
// This package already established (a_deleted_row_and_an_invisible_row_are_
// told_apart_test.go, mssql_store.go's ResetSession doc comment) that
// database/sql calls ResetSession on every recycle and go-mssqldb answers it
// with sp_reset_connection, which clears SESSION_CONTEXT. sp_reset_connection
// is documented to reset SET options generally, not SESSION_CONTEXT
// specifically -- this test is the same measurement extended to
// LOCK_TIMEOUT, on a pool forced to reuse the exact same physical connection
// (SetMaxOpenConns(1)).
func TestMSSQLLockTimeoutDoesNotSurviveAPooledConnectionRecycle(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := testutil.MSSQLTestDB(t)
	ctx := context.Background()

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// First use: set LOCK_TIMEOUT and deliberately do NOT reset it --
	// simulating exactly the bug this fix's defer exists to prevent -- then
	// let the connection return to the pool.
	conn1, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn1: %v", err)
	}
	if _, err := conn1.ExecContext(ctx, "SET LOCK_TIMEOUT 500"); err != nil {
		t.Fatalf("set lock timeout on conn1: %v", err)
	}
	var setValue int
	if err := conn1.QueryRowContext(ctx, "SELECT @@LOCK_TIMEOUT").Scan(&setValue); err != nil {
		t.Fatalf("read back on conn1: %v", err)
	}
	if setValue != 500 {
		t.Fatalf("@@LOCK_TIMEOUT on conn1 = %d, want 500 -- the SET itself didn't take, "+
			"nothing below measures anything until this holds", setValue)
	}
	if err := conn1.Close(); err != nil {
		t.Fatalf("close conn1: %v", err)
	}

	// Known-positive for the harness itself: SetMaxOpenConns(1) is only a
	// forcing function if the pool actually reuses the physical connection
	// rather than opening a second one that would trivially read -1 as its
	// own fresh default and prove nothing about recycling.
	if stats := db.Stats(); stats.OpenConnections != 1 {
		t.Fatalf("db.Stats().OpenConnections = %d after conn1.Close(), want 1 -- SetMaxOpenConns(1) "+
			"isn't forcing single-connection reuse the way this test assumes", stats.OpenConnections)
	}

	// Second use: with MaxOpenConns(1) and one connection already idle, this
	// MUST be the same physical connection recycled, not a fresh one.
	conn2, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn2: %v", err)
	}
	defer conn2.Close()
	if stats := db.Stats(); stats.OpenConnections != 1 {
		t.Fatalf("db.Stats().OpenConnections = %d after acquiring conn2, want 1 -- a second physical "+
			"connection was opened instead of the idle one being reused, so the read below would prove "+
			"nothing about recycling", stats.OpenConnections)
	}
	var recycledValue int
	if err := conn2.QueryRowContext(ctx, "SELECT @@LOCK_TIMEOUT").Scan(&recycledValue); err != nil {
		t.Fatalf("read back on conn2: %v", err)
	}
	if recycledValue != -1 {
		t.Fatalf("@@LOCK_TIMEOUT on the recycled connection = %d, want -1 (SQL Server's default, "+
			"\"wait indefinitely\") -- a caller that left LOCK_TIMEOUT set (which this fix's own "+
			"defer is designed never to do) would silently make some unrelated later statement on "+
			"this pool fail with error 1222 for no reason it could see. If this test fails, the fix "+
			"in mssql_lifecycle.go needs to stop relying on pool recycling and reset unconditionally "+
			"in a defer -- which it already does; this test exists to prove that belt-and-braces "+
			"layer actually holds rather than assuming it from database/sql's documented contract",
			recycledValue)
	}
}

// TestMSSQLClaimWorkflowsOnceResetsLockTimeoutBeforeEndingItsTransaction is
// cleat#2148, and it is the test the issue asked for: one that actually goes
// through claimWorkflowsOnce, rather than simulating the bug on a raw
// *sql.Conn the way the test above does.
//
// THE BUG, restated because it is what makes the "claimed" half below a
// known-positive rather than a sanity check: claimWorkflowsOnce's LOCK_TIMEOUT
// reset used to be a single deferred func, registered right after `SET
// LOCK_TIMEOUT` and relying on defer's LIFO ordering to run after rows2 is
// drained. But TWO of this function's exits end the transaction EXPLICITLY
// before returning -- finishClaim's tx.Commit() on a successful claim, and
// tx.Rollback() on the no-rows-after-recheck path -- so by the time that
// defer ran on either of them, tx was already finished and the reset failed
// with sql.ErrTxDone, logged at Error. "Every SQL Server claim tick that
// reaches Step 4" in production is the ordinary, successful-claim case: this
// is not an edge case to engineer, it is what happens whenever this store
// claims anything at all.
//
// THE EMPTY PATH IS NOT A KNOWN-POSITIVE THE SAME WAY, and this test says so
// rather than implying otherwise. Step 1's SELECT takes UPDLOCK on every
// candidate row as part of finding it, and that lock is held by THIS
// transaction until it ends -- so within one uncontested transaction (no
// concurrent writer can touch a row this transaction already holds), Step 4's
// recheck cannot exclude an id Step 3 admitted, and claimWorkflowsOnce's own
// `len(wfs) == 0` branch is reached in practice only via genuine cross-session
// contention, not from a single sequential call. What IS being asserted on
// the empty call here is the same invariant the claimed call is: no
// Error-level log on a call that touches LOCK_TIMEOUT not at all (Step 1
// finds zero candidates, returns before line 289's SET LOCK_TIMEOUT even
// runs) -- a real but different property, not a second reproduction of
// cleat#2148's specific defect.
//
// KNOWN-POSITIVE, checked by hand rather than left to assertion: reverting
// resetLockTimeoutMSSQL's explicit call sites (keeping only the general
// defer) reproduces
//
//	claim: failed to reset LOCK_TIMEOUT on a pooled connection error="sql: transaction has already been committed or rolled back"
//
// on the CLAIMED subtest below, which fails on "no Error-level log" as this
// test asserts. It does not fail the empty subtest, which is exactly the
// asymmetry the comment above explains.
func TestMSSQLClaimWorkflowsOnceResetsLockTimeoutBeforeEndingItsTransaction(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	testutil.CleanupMSSQLTestData(t, db)
	t.Cleanup(func() { testutil.CleanupMSSQLTestData(t, db) })

	var logBuf bytes.Buffer
	store := openMSSQLTenantStore(t, DefaultTenantUUID).WithLogger(slog.New(slog.NewTextHandler(&logBuf, nil)))
	ctx := context.Background()
	setupTestData(t, store)

	// Force reuse of the SAME physical connection across both claims and the
	// read-back below -- otherwise a fresh connection from the pool would
	// read @@LOCK_TIMEOUT's default and prove nothing about whether THIS
	// store's own connection was actually reset, the same forcing function
	// TestMSSQLLockTimeoutDoesNotSurviveAPooledConnectionRecycle above uses.
	store.db.SetMaxOpenConns(1)
	store.db.SetMaxIdleConns(1)

	// Claimed path: one runnable workflow, claimed successfully. This is the
	// production case cleat#2148 describes -- LOCK_TIMEOUT gets set at Step 4
	// and the transaction ends via finishClaim's Commit.
	id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), fmt.Sprintf("lockreset-claimed-%d", time.Now().UnixNano()),
		DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	claimed, err := store.ClaimWorkflows(ctx, "worker-lockreset-claimed", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows (claimed path): %v", err)
	}
	var gotID bool
	for _, wf := range claimed {
		if wf.ID == id {
			gotID = true
		}
	}
	if !gotID {
		t.Fatalf("ClaimWorkflows did not claim %s -- the claimed-path assertions below would test "+
			"nothing without a real claim to test them on", id)
	}

	// Empty path: nothing left to claim. See the doc comment above for why
	// this exercises a different, LOCK_TIMEOUT-untouched return than the
	// claimed path above, and is asserted for its own sake rather than as a
	// second reproduction of the same defect.
	claimed2, err := store.ClaimWorkflows(ctx, "worker-lockreset-empty", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows (empty path): %v", err)
	}
	if len(claimed2) != 0 {
		t.Fatalf("ClaimWorkflows (empty path) claimed %d workflow(s), want 0 -- nothing was seeded "+
			"for it to find", len(claimed2))
	}

	if logged := logBuf.String(); strings.Contains(logged, "level=ERROR") {
		t.Errorf("claiming logged an Error-level line on a healthy claimed/empty pair, want none "+
			"(cleat#2148 -- a reset attempted after its own transaction already ended):\n%s", logged)
	}

	// And the connection itself: LOCK_TIMEOUT must not still carry Step 4's
	// value onto whatever the pool hands out next. With MaxOpenConns(1) and
	// everything above sequential, this MUST be the same physical connection
	// claimWorkflowsOnce used.
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	var lockTimeout int
	if err := conn.QueryRowContext(ctx, "SELECT @@LOCK_TIMEOUT").Scan(&lockTimeout); err != nil {
		t.Fatalf("read back @@LOCK_TIMEOUT: %v", err)
	}
	if lockTimeout != -1 {
		t.Fatalf("@@LOCK_TIMEOUT on the connection claimWorkflowsOnce used = %d, want -1 -- left set, "+
			"this would make some unrelated later statement on this pool fail with error 1222 for no "+
			"reason it could see", lockTimeout)
	}
}
