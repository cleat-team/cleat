package engine

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/lib/pq"
)

// cleat#3409: PostgresStore.CompactHistory deleted the old events, THEN took its
// generation fence, and committed whether or not the fence held. A reclaim that
// landed in between left the events deleted, no snapshot written, and a nil
// error: steps 0-2 gone and nothing covering them, which a later replay cannot
// recover from.
//
// Measured before the fix (PostgreSQL, holder transaction, events at steps 0-4,
// compactionStep=3, keepStep=3):
//
//	control (holder changes nothing)   err=nil  events left 2  compaction_step 3
//	reclaim commits while it waits     err=nil  events left 2  compaction_step NULL
//
// The second row is the defect. The first row is why the probe can be believed:
// it shows the same call, same seed, doing the compaction when nothing moves.
//
// #3403's TestTheFencesLockTheLeaseRowAndNotTheInstanceRow reaches the same
// reclaim for the snapshot half only. It seeds no events, so it could not see the
// delete.

// seedCompactableRun starts and claims a run and records events at steps 0..4.
func seedCompactableRun(t *testing.T, store *PostgresStore, db *sql.DB, name string) (id string, generation int64) {
	t.Helper()
	ctx := context.Background()
	const worker = "compact-fence-worker"
	id = startPayloadReaderRun(t, store, name)
	wf, err := store.ClaimWorkflow(ctx, worker)
	if err != nil || wf == nil || wf.ID != id {
		t.Fatalf("ClaimWorkflow = %v, %v; want %s", wf, err, id)
	}
	eng := NewEngine(nil, nil,
		WithDB(db),
		WithWorkflowStore(store),
		WithWorkerID(worker),
		WithGeneration(wf.Generation),
		WithTenantID(DefaultTenantUUID))
	for step := 0; step < 5; step++ {
		if err := eng.flushEvent(ctx, id, EventRecord{Step: step, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "resp"}, ""); err != nil {
			t.Fatalf("seed event at step %d: %v", step, err)
		}
	}
	return id, wf.Generation
}

func eventsLeft(t *testing.T, db *sql.DB, id string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM event_history WHERE workflow_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func compactionStepOf(t *testing.T, db *sql.DB, id string) sql.NullInt64 {
	t.Helper()
	var step sql.NullInt64
	if err := db.QueryRow(`SELECT compaction_step FROM workflow_payloads WHERE id = $1`, id).Scan(&step); err != nil {
		t.Fatalf("read compaction_step: %v", err)
	}
	return step
}

// waitUntilBlockedBy returns once another backend is waiting on a lock the given
// transaction holds, polling pg_blocking_pids rather than sleeping: the
// goroutine that starts CompactHistory has no guaranteed progress in any fixed
// interval, and a holder released before it reached its fence turns the reclaim
// case into a call that simply sees the new generation (a red that means
// nothing) and the lock-order case into one that cannot deadlock (a green that
// means nothing).
func waitUntilBlockedBy(t *testing.T, db *sql.DB, holder *sql.Tx) {
	t.Helper()
	ctx := context.Background()
	var pid int
	if err := holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("holder pid: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&n); err != nil {
			t.Fatalf("probe blocked backends: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no backend became blocked on the holder within 20s: CompactHistory never waited on the row the holder locked")
}

// compactWhileHolderRuns runs CompactHistory(compactionStep=3, keepStep=3) with
// a holder transaction that takes the lease row's lock first, waits until the
// call is blocked on it, then commits (a reclaim) or rolls back (a bystander).
func compactWhileHolderRuns(t *testing.T, store *PostgresStore, db *sql.DB, id string, reclaim bool) error {
	t.Helper()
	ctx := context.Background()
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback()
	lock := `SELECT id FROM workflow_leases WHERE id = $1 FOR NO KEY UPDATE`
	if reclaim {
		lock = `UPDATE workflow_leases SET generation = generation + 1 WHERE id = $1`
	}
	if _, err := holder.ExecContext(ctx, lock, id); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- store.CompactHistory(ctx, id, []byte(`{"version":1,"compacted_step":3,"events":[]}`), 3, 3)
	}()
	waitUntilBlockedBy(t, db, holder)
	select {
	case err := <-done:
		t.Fatalf("CompactHistory returned (%v) while another transaction held the lease row -- it no longer waits on the lock that serializes it with a reclaim", err)
	default:
	}
	release := holder.Rollback
	if reclaim {
		release = holder.Commit
	}
	if err := release(); err != nil {
		t.Fatalf("release holder: %v", err)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("CompactHistory still blocked 10s after the holder released")
		return nil
	}
}

func TestCompactHistoryCompactsWhenNothingMovesTheGeneration(t *testing.T) {
	store, db := newRunTestStore(t)
	id, _ := seedCompactableRun(t, store, db, "compact-control")
	if err := compactWhileHolderRuns(t, store, db, id, false); err != nil {
		t.Fatalf("CompactHistory with an unchanged generation: %v", err)
	}
	if got := eventsLeft(t, db, id); got != 2 {
		t.Errorf("events left = %d, want 2 (steps 3 and 4 kept)", got)
	}
	if step := compactionStepOf(t, db, id); !step.Valid || step.Int64 != 3 {
		t.Errorf("compaction_step = %+v, want 3: the control case must really compact, or the lost-fence case below proves nothing", step)
	}
}

func TestCompactHistoryRollsBackItsDeleteWhenTheFenceIsLost(t *testing.T) {
	store, db := newRunTestStore(t)
	id, _ := seedCompactableRun(t, store, db, "compact-lost-fence")

	err := compactWhileHolderRuns(t, store, db, id, true)

	if err == nil {
		t.Error("CompactHistory returned nil though a reclaim committed while it waited: it reports a compaction that did not happen")
	} else if !errors.Is(err, ErrFenceLost) {
		t.Errorf("CompactHistory = %v, want an error wrapping ErrFenceLost", err)
	}
	if got := eventsLeft(t, db, id); got != 5 {
		t.Errorf("events left = %d, want all 5: the delete committed although the fence was lost, so steps 0-2 are gone with no snapshot covering them", got)
	}
	if step := compactionStepOf(t, db, id); step.Valid {
		t.Errorf("compaction_step = %+v, want NULL: a snapshot was written for a run whose generation had moved", step)
	}
}

// cleat#3409 also asked whether moving the fence ahead of the delete changes the
// lock order, which cleat-review measured as a 40P01 between CompactHistory and
// finalize on #3403. finalize_workflow_status (the last migration that defines
// it, 007) UPDATEs the lease row and THEN deletes the run's events, so its
// order is lease -> events. CompactHistory used to take events -> lease, the
// reverse; with the fence first it takes lease -> events, finalize's order.
//
// This is deterministic: a transaction stands in for finalize (the same two
// statements, run by hand so there is a window between them), holds the lease
// row, and CompactHistory is started while it does. Emulated, not the real
// procedure, because calling the procedure runs both statements with no gap.
func TestCompactHistoryAndFinalizeTakeTheirLocksInTheSameOrder(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	id, _ := seedCompactableRun(t, store, db, "compact-lock-order")

	finalize, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin finalize stand-in: %v", err)
	}
	defer finalize.Rollback()
	// finalize's first statement: the fenced UPDATE of the lease row. It does not
	// move generation, so CompactHistory's own fence still matches afterwards.
	if _, err := finalize.ExecContext(ctx, `UPDATE workflow_leases SET next_wake_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("finalize stand-in: lease update: %v", err)
	}

	compactDone := make(chan error, 1)
	go func() {
		compactDone <- store.CompactHistory(ctx, id, []byte(`{"version":1,"compacted_step":3,"events":[]}`), 3, 3)
	}()
	waitUntilBlockedBy(t, db, finalize)
	select {
	case err := <-compactDone:
		t.Fatalf("CompactHistory returned (%v) while the lease row was held", err)
	default:
	}

	// finalize's second statement: delete the run's events. Under the old order
	// CompactHistory already holds locks on the rows it deleted and is waiting for
	// the lease row, so this waits for it and the two deadlock.
	finalizeDone := make(chan error, 1)
	go func() {
		if _, err := finalize.ExecContext(ctx, `DELETE FROM event_history WHERE workflow_id = $1`, id); err != nil {
			finalizeDone <- err
			return
		}
		finalizeDone <- finalize.Commit()
	}()

	isDeadlock := func(err error) bool {
		var pqErr *pq.Error
		return errors.As(err, &pqErr) && pqErr.Code == "40P01"
	}
	var finalizeErr, compactErr error
	for i := 0; i < 2; i++ {
		select {
		case finalizeErr = <-finalizeDone:
		case compactErr = <-compactDone:
		case <-time.After(15 * time.Second):
			t.Fatal("CompactHistory and the finalize stand-in are still blocked after 15s")
		}
	}
	if isDeadlock(finalizeErr) {
		t.Errorf("the finalize stand-in was chosen as the deadlock victim: %v", finalizeErr)
	}
	if isDeadlock(compactErr) {
		t.Errorf("CompactHistory was chosen as the deadlock victim: %v", compactErr)
	}
	if finalizeErr != nil && !isDeadlock(finalizeErr) {
		t.Errorf("finalize stand-in: %v", finalizeErr)
	}
}
