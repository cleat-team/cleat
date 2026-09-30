package engine

// cleat#2210 / cleat#2758: ConsumeSignal used to run its DELETE and its
// signal_consumed_seq UPDATE as two separate, untransacted statements under
// mssqlRetry, which retries on any error in isMSSQLRetryable -- a wider set
// than "the transaction was definitively rolled back". The exposure was an
// UNKNOWN-OUTCOME error (a timeout, a dropped connection) landing after the
// UPDATE had already committed server-side but before its acknowledgement
// reached the caller: mssqlRetry reran the whole closure, redoing the now-
// harmless DELETE and a second, real increment.
//
// This test forces a REAL SQL Server deadlock (1205) against the real
// ConsumeSignal call and checks the counter lands at exactly 1 after its
// withRollbackGuaranteedRetry wrapper retries consumeSignalOnce.
//
// WORTH BEING PRECISE ABOUT WHAT THIS DOES AND DOES NOT FALSIFY, because a
// falsification was run and it disagreed with the first draft of this
// comment. Reverting the fix and rerunning this exact test still PASSED --
// correctly, not as a gap in the test: a 1205 is a rollback-guaranteed error,
// so the statement it strikes never left a partial commit behind, whichever
// of the two statements it hits, whichever code shape wrote them. A pure
// deadlock was never the old code's exposure; only an unknown-outcome error
// was, and that class is not deterministically reproducible against a real
// server without flaky timing (see git history of this file for the attempt
// and why it was dropped).
//
// What this test DOES establish, genuinely: ConsumeSignal's transaction
// survives a real rollback-guaranteed retry without double-applying, which
// is the property the fix's transaction boundary exists to guarantee.
// Combined with two tests it does not duplicate -- TestWithRollbackGuaranteed
// Retry's "timeout is not replayed" case (mssql_retry_wiring_test.go), which
// proves generically that an unknown-outcome error is never replayed by this
// wrapper, and TestMSSQLDeadlock_ClassifiedFromTheRealDriverError, which
// proves a real 1205 from this driver classifies as rollback-guaranteed --
// the argument for the fix is: every error ConsumeSignal's retry wrapper
// will actually replay is one SQL Server guarantees rolled back nothing
// partial, and this test is the one member of that argument requiring a real
// server rather than a constructed error value.

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// deadlockConsumeSignal forces ConsumeSignal's transaction to be chosen as a
// deadlock victim, by taking the same two row locks in the OPPOSITE order on
// a second connection, with DEADLOCK_PRIORITY set high enough that SQL
// Server's monitor kills the other side instead.
//
// ConsumeSignal's transaction locks workflow_signals (DELETE) then
// workflow_instances (UPDATE). This probe locks workflow_instances first,
// waits for ConsumeSignal to be underway, then reaches for workflow_signals
// -- the classic opposite-order cycle, same shape as provokeMSSQLDeadlock in
// mssql_deadlock_test.go, adapted so the real production method is one of
// the two racers rather than a hand-written stand-in for both sides.
//
// adm must be a cross-tenant-exempt pool (testutil.MSSQLAdminDB), not a plain
// one: a plain pool's SELECT is filtered by the RLS policy to zero rows for a
// tenant it never claimed, so the UPDLOCK below would take no lock at all and
// this would silently fail to deadlock -- the exact fixture failure
// TestMSSQLDeadlock_ClassifiedFromTheRealDriverError's own comment records.
func deadlockConsumeSignal(t *testing.T, adm *sql.DB, s *MSSQLStore, workflowID, tenantID string, signalID int64) error {
	t.Helper()
	ctx := context.Background()

	probeConn, err := adm.Conn(ctx)
	if err != nil {
		t.Fatalf("probe conn: %v", err)
	}
	defer probeConn.Close()

	if _, err := probeConn.ExecContext(ctx, `SET DEADLOCK_PRIORITY HIGH`); err != nil {
		t.Fatalf("set deadlock priority: %v", err)
	}
	probeTx, err := probeConn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("probe begin: %v", err)
	}
	defer probeTx.Rollback()

	// Lock 1: workflow_instances, taken first -- opposite of ConsumeSignal's
	// own order.
	if _, err := probeTx.ExecContext(ctx,
		`SELECT 1 FROM workflow_instances WITH (UPDLOCK, ROWLOCK) WHERE id = @p1 AND tenant_id = @p2`,
		workflowID, tenantID); err != nil {
		t.Fatalf("probe lock workflow_instances: %v", err)
	}

	var consumeErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		consumeErr = s.ConsumeSignal(ctx, workflowID, signalID)
	}()

	// ConsumeSignal's DELETE is the first statement in a fresh transaction on
	// a fresh connection; this gives it time to acquire its lock on
	// workflow_signals before the probe reaches for the same row. Not a
	// synchronization primitive -- a bounded wait, same trade-off
	// provokeMSSQLDeadlock's own 10s timeout makes, just without a barrier
	// channel on the other side since ConsumeSignal offers none to hook into.
	time.Sleep(300 * time.Millisecond)

	// Lock 2: workflow_signals, the row ConsumeSignal's DELETE now holds.
	// This blocks; so does ConsumeSignal's own UPDATE on workflow_instances,
	// waiting on this probe's Lock 1 -- the cycle. DEADLOCK_PRIORITY HIGH on
	// this session means the server kills ConsumeSignal's transaction, not
	// this one.
	if _, err := probeTx.ExecContext(ctx,
		`SELECT 1 FROM workflow_signals WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`,
		signalID); err != nil {
		t.Fatalf("probe lock workflow_signals: %v", err)
	}
	// Release immediately -- nothing was written, and ConsumeSignal's retry
	// needs both rows free to succeed.
	probeTx.Rollback()

	wg.Wait()
	return consumeErr
}

func TestMSSQLStore_ConsumeSignalDeadlockRetryDoesNotDoubleIncrement(t *testing.T) {
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}

	ctx := context.Background()
	adminDB := testutil.MSSQLTestDB(t)
	t.Cleanup(func() { adminDB.Close() })
	testutil.SetupMSSQLFullSchema(t, adminDB)
	testutil.CleanupMSSQLTestData(t, adminDB)
	t.Cleanup(func() { testutil.CleanupMSSQLTestData(t, adminDB) })

	// Cross-tenant-exempt: RLS is live from SetupMSSQLFullSchema (the test
	// schema is built from the shipped migrations, policies included), so
	// seeding, locking and reading back all need a pool that is not itself
	// filtered by the policy it is trying to observe.
	adm := testutil.MSSQLAdminDB(t, adminDB)

	const tenantID = DefaultTenantUUID
	run := uuid.New().String()[:8]
	defName := "consume-signal-deadlock-def-" + run
	wfID := "consume-signal-deadlock-wf-" + run

	if _, err := adm.ExecContext(ctx, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		VALUES (@p1, 1, 0x0061736d, 1, 1, @p2)`, defName, tenantID); err != nil {
		t.Fatalf("seed workflow_def: %v", err)
	}
	if _, err := adm.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id, signal_consumed_seq)
		VALUES (@p1, @p2, 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3, 0)`,
		wfID, defName, tenantID); err != nil {
		t.Fatalf("seed workflow_instance: %v", err)
	}
	var signalID int64
	if err := adm.QueryRowContext(ctx, `
		INSERT INTO workflow_signals (workflow_id, signal_name, payload, tenant_id)
		OUTPUT INSERTED.id
		VALUES (@p1, 'deadlock-test-signal', '{}', @p2)`, wfID, tenantID).Scan(&signalID); err != nil {
		t.Fatalf("seed signal: %v", err)
	}

	factory := NewMSSQLStoreFactory(dsn)
	defer factory.Close()
	opened, closer, err := factory.OpenStore(ctx, tenantID)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer closer.Close()
	s, ok := opened.(*MSSQLStore)
	if !ok {
		t.Fatalf("OpenStore returned %T, want *MSSQLStore", opened)
	}

	if err := deadlockConsumeSignal(t, adm, s, wfID, tenantID, signalID); err != nil {
		t.Fatalf("ConsumeSignal under a forced deadlock: %v -- if this is a rollback-guaranteed "+
			"error rather than nil, the retry did not happen or did not succeed", err)
	}

	var remaining int
	if err := adm.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_signals WHERE id = @p1`, signalID).Scan(&remaining); err != nil {
		t.Fatalf("count remaining signal: %v", err)
	}
	if remaining != 0 {
		t.Errorf("signal row still present after ConsumeSignal succeeded (count=%d), want 0", remaining)
	}

	var consumedSeq int
	if err := adm.QueryRowContext(ctx,
		`SELECT signal_consumed_seq FROM workflow_instances WHERE id = @p1`, wfID).Scan(&consumedSeq); err != nil {
		t.Fatalf("read signal_consumed_seq: %v", err)
	}
	if consumedSeq != 1 {
		t.Errorf("signal_consumed_seq = %d after one ConsumeSignal call that survived a forced "+
			"deadlock retry, want exactly 1 (cleat#2210: a retry of two untransacted statements "+
			"re-runs the increment against an already-deleted row, producing 2 instead)", consumedSeq)
	}
}
