package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestMSSQLClaimStep4RecheckPreventsDoubleClaim is cleat#1963's regression
// test for the defect the coordinator found while this issue was being
// root-caused: claimWorkflowsOnce's Step 4 UPDATE (mssql_lifecycle.go)
// filtered only `id IN (...) AND tenant_id = @p3` -- no recheck of status.
//
// Why that matters, independent of exactly how two claimers both come to
// believe a row is free (this package's own investigation found that can
// happen via mismatched nonclustered-index locks -- READPAST only detects a
// conflict on the resource it is about to lock, and two queries with
// different predicate shapes can lock the same row through different
// indexes -- but reproducing that specific mechanism through the live
// go-mssqldb driver turned out to be unreliable to force deterministically:
// the driver's own plan choice did not always match a raw-SQL probe using
// byte-identical, parameter-ordered-identical text, which is itself
// consistent with this package's existing finding that "store.ClaimWorkflows
// end to end does NOT reliably do the same [as raw SQL]"):
//
// Step 4 always locks by id, a point lookup that always resolves to the
// clustered index -- so ANY two transactions that both reach Step 4 for the
// same id, by whatever route, serialize on that same clustered-key lock
// regardless of what index either side's Step 1 used. This test exercises
// that serialization directly and deterministically -- two real sessions
// racing on Step 4's own literal UPDATE text, one blocking behind the
// other's held lock, not slept for -- without needing to force a specific
// Step 1 index mismatch at all. It proves the property the coordinator
// named: the recheck holds regardless of how the two claimers got there.
//
// KNOWN-POSITIVE: proven to fail the predicted way by temporarily setting
// mssqlClaimStep4Recheck to `1 = 1` in mssql_lifecycle.go and re-running --
// session B's UPDATE affected 1 row instead of 0 ("it stole a row session A
// already committed a claim on"), which is the double claim this test exists
// to catch and is where the test's own t.Fatalf halts, before the
// generation/assigned_to assertions below run. Restored and reconfirmed
// passing (3 consecutive runs) before commit.
func TestMSSQLClaimStep4RecheckPreventsDoubleClaim(t *testing.T) {
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

	runID := fmt.Sprintf("mssql-step4-recheck-%d", time.Now().UnixNano())
	id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), runID, DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	// step4UPDATE is mssql_lifecycle.go's Step 4 UPDATE, minus the OUTPUT
	// column list (irrelevant here) and reduced to a single id. Its
	// claimability recheck is mssqlClaimStep4Recheck -- the SAME constant
	// claimWorkflowsOnce interpolates into its own Step 4 -- not a copied
	// literal, so this test exercises the clause actually shipped rather
	// than one that could silently drift from it.
	step4UPDATE := fmt.Sprintf(`
		UPDATE workflow_instances
		SET status = 'running', assigned_to = @p1, generation = generation + 1,
		    heartbeat_at = SYSUTCDATETIME(), started_at = COALESCE(started_at, SYSUTCDATETIME())
		WHERE id = @p2 AND tenant_id = @p3 AND %s`, mssqlClaimStep4Recheck)

	// Session A: claims the row as worker-A, holds the transaction open.
	connA, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("session A conn: %v", err)
	}
	defer connA.Close()
	txA, err := connA.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("session A begin: %v", err)
	}
	defer txA.Rollback()
	if _, err := txA.ExecContext(ctx,
		`EXEC sp_set_session_context @key=N'tenant_id', @value=@p1`, DefaultTenantUUID); err != nil {
		t.Fatalf("session A session context: %v", err)
	}
	var spidA int
	if err := txA.QueryRowContext(ctx, `SELECT @@SPID`).Scan(&spidA); err != nil {
		t.Fatalf("session A spid: %v", err)
	}
	resA, err := txA.ExecContext(ctx, step4UPDATE, "worker-A", id, DefaultTenantUUID)
	if err != nil {
		t.Fatalf("session A claim: %v", err)
	}
	if affected, _ := resA.RowsAffected(); affected != 1 {
		t.Fatalf("session A claim affected %d rows, want 1 -- the fixture is not what this test assumes", affected)
	}

	// Session B: races to claim the same row as worker-B, blocked behind
	// session A's held lock on id's clustered key -- deterministic, since
	// this UPDATE is always a point lookup by id.
	type claimAttempt struct {
		affected int64
		err      error
	}
	resultCh := make(chan claimAttempt, 1)
	go func() {
		connB, err := db.Conn(ctx)
		if err != nil {
			resultCh <- claimAttempt{err: fmt.Errorf("session B conn: %w", err)}
			return
		}
		defer connB.Close()
		if _, err := connB.ExecContext(ctx,
			`EXEC sp_set_session_context @key=N'tenant_id', @value=@p1`, DefaultTenantUUID); err != nil {
			resultCh <- claimAttempt{err: fmt.Errorf("session B session context: %w", err)}
			return
		}
		res, err := connB.ExecContext(ctx, step4UPDATE, "worker-B", id, DefaultTenantUUID)
		if err != nil {
			resultCh <- claimAttempt{err: err}
			return
		}
		affected, _ := res.RowsAffected()
		resultCh <- claimAttempt{affected: affected}
	}()

	// Poll for session B genuinely blocked behind session A -- the
	// deterministic barrier, not a sleep.
	deadline := time.Now().Add(10 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sys.dm_exec_requests WHERE blocking_session_id = @p1`,
			spidA).Scan(&n); err != nil {
			t.Fatalf("poll for blocked session B: %v", err)
		}
		if n > 0 {
			blocked = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !blocked {
		t.Fatalf("session B never showed up blocked on session A (spid %d) within 10s -- "+
			"this test's premise (both UPDATEs target the same clustered key) did not hold", spidA)
	}

	if err := txA.Commit(); err != nil {
		t.Fatalf("session A commit: %v", err)
	}

	var result claimAttempt
	select {
	case result = <-resultCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("session B did not return within 10s of session A committing")
	}
	if result.err != nil {
		t.Fatalf("session B claim: %v", result.err)
	}
	if result.affected != 0 {
		t.Fatalf("session B's claim affected %d row(s), want 0 -- it stole a row session A already "+
			"committed a claim on: double claim, two workers now believe they own the same run", result.affected)
	}

	// Through connA (still live; its transaction is committed, not the
	// connection): workflow_instances carries a FILTER predicate (RLS) and
	// no BLOCK predicate, so a plain query through a connection with no
	// tenant_id in its session context silently matches zero rows rather
	// than erroring -- the same trap mssql_readpast_ambiguity_test.go's
	// reset step documents.
	var finalStatus, finalAssignedTo string
	var finalGeneration int64
	if err := connA.QueryRowContext(ctx,
		`SELECT status, assigned_to, generation FROM workflow_instances WHERE id = @p1`, id).
		Scan(&finalStatus, &finalAssignedTo, &finalGeneration); err != nil {
		t.Fatalf("final state: %v", err)
	}
	if finalStatus != "running" {
		t.Errorf("final status = %q, want running", finalStatus)
	}
	if finalAssignedTo != "worker-A" {
		t.Errorf("final assigned_to = %q, want worker-A -- session B's blocked UPDATE mutated a row it should have excluded", finalAssignedTo)
	}
	if finalGeneration != 1 {
		t.Errorf("final generation = %d, want 1 -- a second increment means session B's UPDATE matched this row "+
			"(the double claim, even if affected/assigned_to look right for some other reason)", finalGeneration)
	}
}
