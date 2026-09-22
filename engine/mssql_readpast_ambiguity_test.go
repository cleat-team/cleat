package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestAContendedMSSQLClaimBlocksRatherThanReturningAnAmbiguousZero measures
// the SQL Server half of cleat#923's ambiguity, which cleat#982's
// investigation found had never actually been measured on this dialect --
// and the answer is not the one #923's own doc comment predicts.
//
// # Why this test exists, and what it replaces
//
// TestAZeroClaimIsAmbiguousWhenRowsAreMerelyLocked (claim_skip_locked_ambiguity_test.go)
// pins the SAME question for PostgreSQL's FOR UPDATE SKIP LOCKED: a locked
// row is REMOVED from the candidate set, so a zero-row claim is ambiguous
// between "nothing to run" and "everything was locked". cleat#982's four SQL
// Server "the row was not there" failures were originally explained by
// citing that test's control as though it had an `/mssql` subtest -- it does
// not, has never had one, and the misattribution was caught and corrected
// twice independently in that issue's thread before anyone measured what SQL
// Server's READPAST actually does under the same contention. This is that
// measurement, and it found something different from what it went looking
// for.
//
// # What claimWorkflowsOnce's own doc comment claims, and what is measured here
//
// mssql_lifecycle.go says READPAST/UPDLOCK is "SQL Server's equivalent of FOR
// UPDATE SKIP LOCKED". That is true of a BARE SELECT: three separate raw-SQL
// experiments (a minimal two-column table, the same table with
// dbo.fn_tenant_filter's FILTER PREDICATE attached, and the exact production
// candidate-SELECT text including its LEFT JOIN to queues, all run as two
// plain sqlcmd sessions with no Go, no connection pool, no ORM) each show a
// second session's READPAST SELECT correctly returning ZERO rows while a
// first session holds UPDLOCK/ROWLOCK on all of them.
//
// It is NOT what claimWorkflowsOnce does end to end. Measured twice,
// independently, both times identical: a holder connection (plain
// db.Conn + BeginTx, session-context-scoped exactly like the claim path
// scopes itself) takes UPDLOCK/ROWLOCK on every candidate row, and a THIRD
// connection confirms via sys.dm_tran_locks -- at the exact instant between
// the holder finishing its lock and the claim starting -- that the locks are
// genuinely GRANTED (not WAITing), KEY-level, one per row, open_transaction_count=1.
// Then store.ClaimWorkflows is called. It does not return zero. Its internal
// candidate-SELECT (the same READPAST/UPDLOCK/ROWLOCK query proven correct
// above) evidently treats the locked rows as available, and the SUBSEQUENT
// UPDATE...OUTPUT that actually claims them then blocks on the genuinely-held
// lock -- for over 300 seconds in one run, 60+ in a second, both times ended
// only by killing the holder's session.
//
// # What was ruled out, so the next person does not re-derive it
//
//   - RCSI: sys.databases.is_read_committed_snapshot_on = 0 on the test
//     database.
//   - SNAPSHOT isolation: sys.databases.snapshot_isolation_state_desc = OFF,
//     which means no session on this database can even opt into it -- ruling
//     out "the reader sees the pre-lock snapshot version" as a mechanism, a
//     hypothesis that otherwise fits the symptom (candidate found; write
//     blocks) exactly.
//   - Stale state from earlier iterations: sys.dm_exec_sessions showed zero
//     lingering go-mssqldb sessions or open transactions immediately before
//     each measurement.
//   - The LEFT JOIN to queues: the exact production SELECT text, run raw,
//     correctly returns zero candidates under the same lock.
//   - The FILTER predicate itself: a bare table with fn_tenant_filter
//     attached shows the same correct zero.
//
// What was NOT established is why the Go-driven path differs from the raw
// SQL that is byte-for-byte the same query text. That gap is filed
// separately (see the comment above testContendedClaimTimeout below) rather
// than left implicit here.
//
// # Why this test is fast rather than reproducing the measurement directly
//
// The measurement above took 60-320 seconds per run, waiting out a genuine
// SQL Server lock. That is not something to commit to a suite every SQL
// Server job runs. This test bounds the same contended claim with a short
// context timeout instead, and pins the OBSERVED behavior -- the claim call
// does not return quickly with zero results; it is still blocked when the
// timeout fires. If SQL Server's behavior here is ever fixed to genuinely
// skip contended rows (matching the doc comment's own claim), this test
// starts failing because the claim returns before the deadline -- which is
// an improvement worth noticing, not a regression to silently paper over.
func TestAContendedMSSQLClaimBlocksRatherThanReturningAnAmbiguousZero(t *testing.T) {
	// No skip on an unset CLEAT_TEST_MSSQL, matching the PostgreSQL sibling
	// test's own testutil.TestDB: MSSQLTestDB falls back to a default DSN and
	// Fatals on a failed connection rather than skipping silently. CLAUDE.md's
	// own rule is that an unset DSN skipping silently is the trap, not the
	// safety net.
	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	testutil.CleanupMSSQLTestData(t, db)
	t.Cleanup(func() { testutil.CleanupMSSQLTestData(t, db) })

	store := openMSSQLTenantStore(t, DefaultTenantUUID)
	ctx := context.Background()
	setupTestData(t, store)

	const n = 3
	for i := 0; i < n; i++ {
		if _, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
			json.RawMessage(`{}`), fmt.Sprintf("mssql-readpast-%d-%d", i, time.Now().UnixNano()),
			DefaultTenantUUID, 0); err != nil {
			t.Fatalf("StartNewRun[%d]: %v", i, err)
		}
	}

	// Baseline: with nothing locked the claim finds the work. A floor, not an
	// equality -- setupTestData leaves a runnable row of its own, same reason
	// as the PostgreSQL sibling test.
	got, err := store.ClaimWorkflows(ctx, "worker-baseline", 10)
	if err != nil {
		t.Fatalf("baseline ClaimWorkflows: %v", err)
	}
	if len(got) < n {
		t.Fatalf("baseline claimed %d, want at least %d -- the fixture is not what this test assumes",
			len(got), n)
	}

	// Reset through a SESSION_CONTEXT-scoped connection, not the plain db
	// handle. workflow_instances carries a FILTER predicate and no BLOCK
	// predicate, so an UPDATE through a connection with no tenant_id in its
	// session context is silently accepted and matches ZERO rows -- sa has no
	// cleat_admin exemption, so CAST(SESSION_CONTEXT(N'tenant_id') AS
	// UNIQUEIDENTIFIER) is NULL and the WHERE clause the security policy
	// injects is unknown for every row. That is not hypothetical: the first
	// version of this test did exactly that, and the reset silently updated
	// nothing, which would have been trusted as a real holder-locked-nothing
	// failure if the known-positive below had not caught it.
	resetConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("reset conn: %v", err)
	}
	defer resetConn.Close()
	resetTx, err := resetConn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("reset begin: %v", err)
	}
	if _, err := resetTx.ExecContext(ctx,
		`EXEC sp_set_session_context @key=N'tenant_id', @value=@p1`, DefaultTenantUUID); err != nil {
		resetTx.Rollback()
		t.Fatalf("setting session context on the reset connection: %v", err)
	}
	for _, wf := range got {
		res, err := resetTx.ExecContext(ctx,
			`UPDATE workflow_instances SET status='ready', assigned_to=NULL WHERE id=@p1`, wf.ID)
		if err != nil {
			resetTx.Rollback()
			t.Fatalf("resetting %s: %v", wf.ID, err)
		}
		if rows, _ := res.RowsAffected(); rows != 1 {
			resetTx.Rollback()
			t.Fatalf("resetting %s affected %d rows, want 1 -- the FILTER predicate is still "+
				"hiding this row from the reset connection", wf.ID, rows)
		}
	}
	if err := resetTx.Commit(); err != nil {
		t.Fatalf("reset commit: %v", err)
	}

	// Hold UPDLOCK/ROWLOCK on every candidate, as a long-running transaction
	// would -- deliberately no READPAST, so this connection blocks other
	// lockers rather than skipping past them.
	holderDB := testutil.MSSQLTestDB(t)
	holderConn, err := holderDB.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer holderConn.Close()
	holderTx, err := holderConn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer holderTx.Rollback()

	// Tenant-scope the holder the same way MSSQLStore.setSessionContext does.
	// Skipping this is not a smaller version of the test -- the FILTER
	// predicate on workflow_instances would make the SELECT below return zero
	// rows, and a holder that locked nothing produces a fast, empty claim for
	// an entirely different and uninteresting reason.
	if _, err := holderTx.ExecContext(ctx,
		`EXEC sp_set_session_context @key=N'tenant_id', @value=@p1`, DefaultTenantUUID); err != nil {
		t.Fatalf("setting session context on the holder: %v", err)
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
	if err := holderRows.Err(); err != nil {
		holderRows.Close()
		t.Fatalf("holder lock rows: %v", err)
	}
	holderRows.Close()
	// KNOWN-POSITIVE: the holder must actually have locked something, or
	// whatever the claim does next measures an empty fixture rather than
	// contention.
	if locked < n {
		t.Fatalf("holder locked %d row(s), want at least %d -- the holder's own SELECT did not "+
			"see the candidates it was meant to lock, so nothing below tests what this test claims to",
			locked, n)
	}

	// The measurement. Bounded to a few seconds: the un-bounded version of
	// this call blocked for 60-320s in two separate runs, both times only
	// ending when the holder's session was killed from outside the test. A
	// context deadline here is the FAST way to observe "still blocked at
	// time T" without paying minutes for it -- see the type doc above for
	// the raw-SQL measurements this bound is standing in for.
	claimCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	claimed, claimErr := store.ClaimWorkflows(claimCtx, "worker-contended", 10)

	if claimErr == nil {
		// The doc comment's claim -- READPAST behaves like SKIP LOCKED --
		// would predict claimed having length 0. What was actually measured,
		// twice, is that the call does not return this fast at all; it is
		// still blocked on the UPDATE when the 3s deadline fires, which is
		// the branch below. Returning without error inside 3s is neither of
		// the two measured outcomes, so it is reported as its own finding
		// rather than forced into either bucket.
		t.Fatalf("ClaimWorkflows returned inside the 3s bound with no error, claiming %d row(s). "+
			"Neither measured outcome (a clean zero, matching SKIP LOCKED; or blocking on the "+
			"write, measured twice against this exact query) predicts this. If READPAST's "+
			"candidate-selection has started correctly excluding contended rows, this is an "+
			"IMPROVEMENT worth its own investigation, not a value to special-case here.", len(claimed))
	}
	if ctxErr := claimCtx.Err(); ctxErr == nil {
		// claimErr is non-nil but the deadline did not fire -- some OTHER
		// error occurred (a connection failure, a genuine SQL error). That is
		// not this test's finding either.
		t.Fatalf("ClaimWorkflows returned an error before the 3s deadline, and it was not a "+
			"context deadline: %v. That is a different failure from the one this test measures.",
			claimErr)
	}

	t.Logf("ClaimWorkflows was still blocked when the 3s context deadline fired: %v. "+
		"This matches both full-length measurements (60s and 320s, ended only by killing the "+
		"holder's session) and contradicts claimWorkflowsOnce's own doc comment, which describes "+
		"READPAST/UPDLOCK as SQL Server's equivalent of FOR UPDATE SKIP LOCKED. Filed as its own "+
		"issue rather than fixed here: the mechanism producing the difference between this and "+
		"the raw-SQL measurement (which correctly returns zero candidates under identical, "+
		"externally-verified lock state) was not established.", claimErr)
}
