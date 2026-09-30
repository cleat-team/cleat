package engine

// cleat#2244 (read, not measured, when filed): engine/flush.go's per-step
// event fence -- insertEventSQL's `$32 = '' OR EXISTS (SELECT 1 FROM
// workflow_instances WHERE id = $1 AND assigned_to = $32 AND
// generation = $33)` -- is a plain, non-locking read. On PostgreSQL at
// READ COMMITTED and on SQL Server with RCSI on (the configuration
// docs/reference/database-backends.md recommends), a plain read does not
// wait for an in-flight, uncommitted UPDATE on the row it reads; it sees
// the last COMMITTED version instead. ReapStaleInstances reclaims a stale
// workflow with exactly such an UPDATE (assigned_to = NULL,
// generation = generation + 1), inside its own transaction. So while that
// transaction is open but not yet committed, a zombie worker's flushEvent,
// fencing against its now-stale (assigned_to, generation), could in
// principle still see the PRE-reclaim row and pass its own fence check --
// the same shape as cleat#2239's DeliverSignal/purge race and cleat#2242's
// ingest/delete race, both already measured and fixed elsewhere in this
// tree.
//
// This measures it the same way those two did: hold the reclaim's own
// UPDATE open in its own uncommitted transaction, start the zombie's
// flushEvent concurrently against the PRE-reclaim (workerID, generation),
// and observe -- not assume -- whether it returns before the reclaim
// commits, and if it does, whether the write survives and what it does to
// the step the LEGITIMATE new owner goes on to write.
//
// Simplified relative to the real ReapStaleInstances: this drives the
// SAME SET list (status, assigned_to, heartbeat_at, generation,
// reclaim_count) against one row picked by `id = ?` rather than by
// ReapStaleInstances' own heartbeat-staleness WHERE/ORDER BY/LIMIT --
// that selection logic is a separate concern, already covered by
// flush_fence_test.go and the reaper's own tests, and is not what this
// file is about. What this file needs is the WRITE's timing relative to a
// concurrent read, not how the row was chosen.
//
// cleat-review's own filed read: the effect is "bounded by the reclaim
// timeout and by ON CONFLICT (workflow_id, step)". The ON CONFLICT half of
// that claim is the second thing this test checks, not only whether the
// zombie's write lands: insertEventSQL's ON CONFLICT ... DO UPDATE ...
// WHERE clause only fires for the three suspend-then-complete event types
// (await_child, await_promise, await_all_children) with every outcome
// column still NULL -- see that constant's own doc. A plain "call" event
// like the one this test flushes does not match that WHERE, so whichever
// writer's row for a given step lands FIRST wins PERMANENTLY and every
// later write to the same step is silently declined (0 rows affected) --
// which is exactly the "a stale step result the new owner then replays"
// case cleat-review named as the thing that would make this matter.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// reclaimRaceResult records what actually happened, not just a verdict --
// see CLAUDE.md's "print the PRECONDITIONS beside every row of a result
// table" and "state what was and was not observed".
type reclaimRaceResult struct {
	dialect string

	// Did the reclaim's own UPDATE find and affect exactly the one row this
	// test seeded? If this is not 1, nothing below is evidence about
	// anything.
	reclaimRowsAffected int64

	// Did the zombie's flushEvent RETURN before the reclaim transaction
	// committed? If false, the read blocked (or was simply slower than the
	// wait window) and the race this test exists to force did not occur on
	// this run.
	zombieReturnedBeforeCommit bool
	// The zombie's flushEvent error, from whenever it actually returned
	// (before or after the commit).
	zombieErr error

	// Did the zombie's write actually land in event_history?
	zombieRowLanded bool
	zombieRowValue  string

	// Did the LEGITIMATE new owner's later write to the SAME step get
	// accepted, or silently declined by ON CONFLICT because the zombie's
	// row (if any) got there first?
	newOwnerRowValue    string
	newOwnerWriteLanded bool
}

func (r reclaimRaceResult) String() string {
	return fmt.Sprintf(
		"dialect=%s reclaimRows=%d zombieRacedAhead=%v zombieErr=%v zombieLanded=%v(%q) newOwnerLanded=%v(%q)",
		r.dialect, r.reclaimRowsAffected, r.zombieReturnedBeforeCommit, r.zombieErr,
		r.zombieRowLanded, r.zombieRowValue, r.newOwnerWriteLanded, r.newOwnerRowValue)
}

func TestFlushEvent_RacingAnOpenReclaimTransaction(t *testing.T) {
	var results []reclaimRaceResult

	t.Run("postgres", func(t *testing.T) {
		store, teardown := (&PostgresBackend{}).Setup(t)
		defer teardown()
		results = append(results, runReclaimRaceTest(t, "postgres", store))
	})

	t.Run("mysql", func(t *testing.T) {
		store, teardown := (&MySQLBackend{}).Setup(t)
		defer teardown()
		results = append(results, runReclaimRaceTest(t, "mysql", store))
	})

	t.Run("mysql_read_committed", func(t *testing.T) {
		if os.Getenv("CLEAT_TEST_MYSQL") == "" {
			t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
		}
		db := mysqlReadCommittedTestDB(t)
		testutil.SetupMySQLFullSchema(t, db)
		applyMySQLProcedures(t, db)
		testutil.CleanupMySQLTestData(t, db)
		store := NewMySQLStore(db)
		t.Cleanup(func() {
			testutil.CleanupMySQLTestData(t, db)
			db.Close()
		})
		results = append(results, runReclaimRaceTest(t, "mysql_read_committed", store))
	})

	t.Run("mssql_rcsi_on", func(t *testing.T) {
		results = append(results, runReclaimRaceMSSQL(t, "mssql_rcsi_on", true))
	})
	t.Run("mssql_rcsi_off", func(t *testing.T) {
		results = append(results, runReclaimRaceMSSQL(t, "mssql_rcsi_off", false))
	})

	t.Cleanup(func() {
		t.Logf("cleat#2244 measurement summary (%d configurations run):", len(results))
		for _, r := range results {
			t.Logf("  %s", r)
		}
	})
}

func runReclaimRaceMSSQL(t *testing.T, name string, rcsiOn bool) reclaimRaceResult {
	t.Helper()
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	raw := testutil.MSSQLTestDB(t)
	db, dsn := mssqlPrivateRCSIDatabase(t, raw, "2244_"+name, rcsiOn)
	applyMSSQLProcedures(t, db)

	ws, closer, err := NewMSSQLStoreFactory(dsn).OpenStore(context.Background(), DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	store, ok := ws.(*MSSQLStore)
	if !ok {
		t.Fatalf("OpenStore returned %T, want *MSSQLStore", ws)
	}
	return runReclaimRaceTest(t, name, store)
}

// runReclaimRaceTest is the shared body: seed, claim, hold the reclaim
// open, race flushEvent against it, resolve, and check both the zombie's
// write and the legitimate new owner's follow-up write to the same step.
func runReclaimRaceTest(t *testing.T, name string, store WorkflowStore) reclaimRaceResult {
	t.Helper()
	ctx := context.Background()
	result := reclaimRaceResult{dialect: name}

	wfID := newIntentWorkflow(t, ctx, store, "reclaim-race-"+name)

	wf, err := store.ClaimWorkflow(ctx, "worker-zombie")
	if err != nil || wf == nil || wf.ID != wfID {
		t.Fatalf("ClaimWorkflow (worker-zombie): wf=%v err=%v", wf, err)
	}
	staleGeneration := wf.Generation

	reclaimTx, rowsAffected := holdReclaimOpen(t, ctx, store, wfID)
	result.reclaimRowsAffected = rowsAffected
	if rowsAffected != 1 {
		t.Fatalf("holding the reclaim open affected %d rows, want 1 -- "+
			"this run proves nothing about the race", rowsAffected)
	}

	eng := NewEngine(nil, nil,
		WithDB(rawDBOf(t, store)),
		WithWorkflowStore(store),
		WithWorkerID("worker-zombie"),
		WithGeneration(staleGeneration),
		WithTenantID(DefaultTenantUUID))

	rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "resp-from-zombie-mid-reclaim"}
	type flushOutcome struct {
		err     error
		elapsed time.Duration
	}
	zombieDone := make(chan flushOutcome, 1)
	start := time.Now()
	go func() {
		err := eng.flushEvent(ctx, wfID, rec, "")
		zombieDone <- flushOutcome{err: err, elapsed: time.Since(start)}
	}()

	// Long enough that an UNBLOCKED flushEvent would already have finished
	// -- proving the race's outcome rather than sampling a lucky ordering.
	// Same window runIngestRaceTest and runDeliverSignalRaceSuite use for
	// the same reason.
	select {
	case outcome := <-zombieDone:
		result.zombieReturnedBeforeCommit = true
		result.zombieErr = outcome.err
		t.Logf("%s: zombie flushEvent RETURNED BEFORE the reclaim committed, "+
			"after %s: err=%v", name, outcome.elapsed, outcome.err)
	case <-time.After(700 * time.Millisecond):
		t.Logf("%s: zombie flushEvent had NOT returned 700ms after starting, "+
			"with the reclaim still uncommitted -- it appears blocked", name)
	}

	if err := reclaimTx.Commit(); err != nil {
		t.Fatalf("commit reclaim tx: %v", err)
	}

	if !result.zombieReturnedBeforeCommit {
		select {
		case outcome := <-zombieDone:
			result.zombieErr = outcome.err
			t.Logf("%s: zombie flushEvent returned %s after the reclaim committed "+
				"(%s after starting): err=%v", name, time.Since(start), outcome.elapsed, outcome.err)
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: zombie flushEvent did not return within 10s of the "+
				"reclaim committing -- deadlock?", name)
		}
	}

	hist, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory (after zombie): %v", err)
	}
	for _, h := range hist {
		if h.Step == 0 {
			result.zombieRowLanded = true
			result.zombieRowValue = h.Response
		}
	}
	if result.zombieRowLanded && !errors.Is(result.zombieErr, ErrFenceLost) {
		// Consistent: the zombie's own view (no error) agrees with what is
		// actually in the table (a row exists). Nothing to flag here beyond
		// the summary log -- this is one of the two coherent outcomes.
	} else if !result.zombieRowLanded && errors.Is(result.zombieErr, ErrFenceLost) {
		// The other coherent outcome: the fence held, nothing landed.
	} else {
		t.Errorf("%s: INCOHERENT: zombie err=%v but row-landed=%v -- "+
			"flushEvent's own report disagrees with what LoadEventHistory shows",
			name, result.zombieErr, result.zombieRowLanded)
	}

	// THE ACTUAL REGRESSION GUARD (cleat-review, R1 on #2815): everything
	// above this point only LOGS the race's outcome, so a reintroduced race
	// -- FOR SHARE removed from insertEventSQL, say -- reports
	// zombieRowLanded=true, zombieErr=nil, newOwnerWriteLanded=false, and the
	// test still passes, because nothing before this line asserts any of the
	// three. Measured directly: with FOR SHARE deleted, the zombie's write
	// returns nil in ~6ms and the new owner's write is silently declined --
	// coherent by the check above, PASS overall, and cleat#2244 could regress
	// with this file staying green. A fence that lets a zombie through is a
	// fence loss whether or not the write happens to be internally
	// consistent about it.
	if result.zombieRowLanded {
		t.Errorf("%s: the zombie's write to step 0 LANDED while the reclaim "+
			"was open -- the fence did not hold", name)
	}
	if !errors.Is(result.zombieErr, ErrFenceLost) {
		t.Errorf("%s: zombie flushEvent returned %v, want ErrFenceLost", name, result.zombieErr)
	}

	// The legitimate new owner claims the reclaimed workflow and writes its
	// OWN version of the SAME step. This is the check cleat-review's
	// "matters" question turns on: does the new owner's write actually take
	// effect, or does insertEventSQL's ON CONFLICT clause silently decline
	// it because the zombie's row (if it landed) already occupies step 0
	// and is not one of the three event types that clause reopens?
	newWf, err := store.ClaimWorkflow(ctx, "worker-legitimate")
	if err != nil || newWf == nil || newWf.ID != wfID {
		t.Fatalf("ClaimWorkflow (worker-legitimate): wf=%v err=%v", newWf, err)
	}
	if newWf.Generation == staleGeneration {
		t.Fatalf("new owner's generation (%d) equals the zombie's (%d) -- "+
			"the reclaim did not actually advance it", newWf.Generation, staleGeneration)
	}

	newEng := NewEngine(nil, nil,
		WithDB(rawDBOf(t, store)),
		WithWorkflowStore(store),
		WithWorkerID("worker-legitimate"),
		WithGeneration(newWf.Generation),
		WithTenantID(DefaultTenantUUID))

	newRec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "resp-from-legitimate-new-owner"}
	if err := newEng.flushEvent(ctx, wfID, newRec, ""); err != nil {
		t.Logf("%s: legitimate new owner's flushEvent to the same step: err=%v", name, err)
	}

	hist2, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory (after new owner): %v", err)
	}
	for _, h := range hist2 {
		if h.Step == 0 {
			result.newOwnerRowValue = h.Response
			result.newOwnerWriteLanded = h.Response == newRec.Response
		}
	}
	if result.zombieRowLanded && !result.newOwnerWriteLanded {
		t.Logf("%s: CONFIRMED IMPACT -- the zombie's write to step 0 landed first and "+
			"the legitimate new owner's write to the SAME step was silently declined; "+
			"step 0 permanently reads %q", name, result.newOwnerRowValue)
	}
	if !result.newOwnerWriteLanded {
		t.Errorf("%s: the legitimate new owner's write to step 0 did NOT land "+
			"(step 0 reads %q) -- see cleat#2244 for why this is the impact that matters",
			name, result.newOwnerRowValue)
	}

	return result
}

// holdReclaimOpen begins the same kind of transaction the real
// ReapStaleInstances uses for this dialect (RLS/session context included
// where the dialect requires it) and runs ReapStaleInstances' own SET list
// against exactly the one row named by id -- see this file's header for why
// the selection predicate is simplified to `id = ?` rather than
// reproducing the heartbeat/LIMIT/ORDER BY machinery. Returns the
// UNCOMMITTED transaction and the rows affected; the caller commits (or
// could roll back) explicitly.
func holdReclaimOpen(t *testing.T, ctx context.Context, store WorkflowStore, workflowID string) (*sql.Tx, int64) {
	t.Helper()
	switch s := store.(type) {
	case *PostgresStore:
		tx, err := s.beginTxWithRLS(ctx)
		if err != nil {
			t.Fatalf("begin reclaim tx (postgres): %v", err)
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = CASE WHEN pending_terminal_status IS NOT NULL
			                  THEN 'terminating' ELSE 'ready' END,
			    assigned_to = NULL, heartbeat_at = NULL, generation = generation + 1,
			    reclaim_count = reclaim_count + 1
			WHERE id = $1 AND status = 'running'`, workflowID)
		if err != nil {
			t.Fatalf("reclaim UPDATE (postgres): %v", err)
		}
		n, _ := res.RowsAffected()
		return tx, n
	case *MySQLStore:
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin reclaim tx (mysql): %v", err)
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = CASE WHEN pending_terminal_status IS NOT NULL
			                  THEN 'terminating' ELSE 'ready' END,
			    assigned_to = NULL, heartbeat_at = NULL, generation = generation + 1,
			    reclaim_count = reclaim_count + 1
			WHERE id = ? AND status = 'running'`, workflowID)
		if err != nil {
			t.Fatalf("reclaim UPDATE (mysql): %v", err)
		}
		n, _ := res.RowsAffected()
		return tx, n
	case *MSSQLStore:
		tx, err := s.beginTxWithContext(ctx)
		if err != nil {
			t.Fatalf("begin reclaim tx (mssql): %v", err)
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = CASE WHEN pending_terminal_status IS NOT NULL
			                  THEN 'terminating' ELSE 'ready' END,
			    assigned_to = NULL, heartbeat_at = NULL, generation = generation + 1,
			    reclaim_count = reclaim_count + 1
			WHERE id = @p1 AND status = 'running'`, workflowID)
		if err != nil {
			t.Fatalf("reclaim UPDATE (mssql): %v", err)
		}
		n, _ := res.RowsAffected()
		return tx, n
	default:
		t.Fatalf("holdReclaimOpen: unsupported store type %T", store)
		return nil, 0
	}
}
