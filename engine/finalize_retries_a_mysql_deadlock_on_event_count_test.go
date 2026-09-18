package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestFinalizeRetriesADeadlockOnEventCount is cleat#1883.
//
// samples-go/mysql's TestRepeatsOfOneSignalDoNotReachAQuorum finalized a
// workflow to status=failed carrying InnoDB's raw
//
//	append events in tx: increment event_count: Error 1213 (40001):
//	Deadlock found when trying to get lock; try restarting transaction
//
// -- the same isDeadlockError/isLockWaitTimeout-existed-and-was-not-called
// shape as cleat#1753, on a different call site: FinalizeWorkflowSegment
// never retried a lock conflict the way startNewRunUnderIdempotencyKey does.
//
// RECONSTRUCTED, NOT REPLAYED -- the ports repo is not checked out here, and
// racing the store method the fix touches is what proves the fix, whatever
// produces the contention. N racers finalize DIFFERENT step ranges for the
// SAME row under the SAME generation. Exactly one wins the fence; the rest
// lose it and roll back -- but a loser's OWN transaction has already run its
// INSERTs into event_history and its OWN "increment event_count" UPDATE
// before it ever reaches the fence check, so those locks are held while that
// happens. If the WINNER's transaction, in the same window, reaches
// finalize_workflow_status's unconditional
//
//	DELETE FROM event_history WHERE workflow_id = p_workflow_id
//
// (migrations/mysql/071_the_finalize_procedure_stops_casting_the_result.sql),
// that DELETE's range scan needs the gap a loser's uncommitted INSERT sits
// in, while the loser is blocked wanting the SAME workflow_instances row the
// winner still holds -- a cycle. InnoDB kills one side, and when it kills a
// loser blocked on ITS OWN increment, the message is exactly the one #1883
// reported.
//
// MEASURED, not assumed. With finalizeWorkflowSegmentRetrying's retry loop
// bypassed (calling finalizeWorkflowSegmentInner directly), every one of 40
// rounds of 8 racers produced this exact error reaching the caller -- a
// reliably reproducible race, unlike cleat#1753's ~40%-per-round precedent,
// because this window is the width of a whole transaction's INSERTs rather
// than a single statement. Restoring the retry: 0 of 40.
func TestFinalizeRetriesADeadlockOnEventCount(t *testing.T) {
	const (
		racers = 8
		rounds = 40
	)

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			const defName = "finalize-deadlock"
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			for round := 0; round < rounds; round++ {
				raceFinalizeSegments(t, ctx, store, defName, round, racers)
			}
		})
	}
}

// raceFinalizeSegments claims one fresh workflow and releases `racers`
// concurrent FinalizeWorkflowSegment calls against it, each appending a
// disjoint step range under the SAME generation.
//
// Exactly one racer winning the fence and the rest losing it with
// ErrFenceLost is EXPECTED and asserts nothing -- that IS the fencing
// mechanism working. What must never happen, and is this test's whole
// assertion, is anything else reaching the caller: a raw deadlock or
// lock-wait error is exactly the divergence cleat#1883 found, and any other
// error is a different, unexpected defect this test is not about.
func raceFinalizeSegments(t *testing.T, ctx context.Context, store WorkflowStore, defName string, round, racers int) {
	t.Helper()

	id := fmt.Sprintf("finalize-racer-%d-%d", round, time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, id, defName, 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	claimed, err := store.ClaimWorkflow(ctx, "worker-1")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimWorkflow: %v %v", claimed, err)
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		errs  []error
		start = make(chan struct{})
	)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release together, so the window is actually contended

			recs := []EventRecord{{
				Step:        i,
				EventType:   EventTypeSignalReceived,
				SignalName:  fmt.Sprintf("sig-%d", i),
				TimestampMs: time.Now().UnixMilli(),
			}}
			// An OBJECT, not a bare string -- the latter is valid JSON and
			// logs an ERROR-level warning about the result contract on every
			// racer, which is noise this test has no interest in producing.
			err := store.FinalizeWorkflowSegment(ctx, claimed.ID, "worker-1",
				claimed.Generation, recs, "done", `{"ok":true}`, "", "", nil, time.Time{})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		if errors.Is(err, ErrFenceLost) {
			continue // expected: exactly one racer wins the fence
		}
		t.Errorf("round %d: an unretried error reached the caller: %v", round, err)
		if isDeadlockOrLockWait(err) {
			t.Logf("  ^ this is the lock conflict cleat#1883 found: a losing racer's " +
				"own \"increment event_count\" blocked on the row the winner holds, " +
				"while the winner's finalize_workflow_status blocks deleting the " +
				"loser's own uncommitted event_history rows -- a cycle, and unretried")
		}
	}
}
