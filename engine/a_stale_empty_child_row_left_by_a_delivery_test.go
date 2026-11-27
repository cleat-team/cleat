package engine

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#4511, cleat-review2's SHOULD: "the stale empty child row stays reopenable forever under the ON CONFLICT guards. Add a re-flush test row."
//
// Delivering an update across a child wait leaves the wait's own row EMPTY at step 0 with the delivery, its completion and the guest's second wait behind it. Before this change an
// empty child row was always the LAST row of its history; now one can have rows after it, and the store's upsert guard (store_event_write.go: an await_child, await_promise or
// await_all_children row whose response, error and result are all NULL) still lets a later write of that step overwrite it.
//
// Two rows, both measured on PostgreSQL, MySQL and SQL Server.
//   - THE NORMAL PATH holds: the child finishes, the wake answers over the LIVE row (step 3), and the stale one at step 0 is untouched and the chain verifies.
//   - A STRAY ANSWER over the stale row is accepted, and breaks the chain at step 1. That is the store's guard, not this change's code, and it is not fixed here: it is reachable
//     only by a writer that holds the run's history as it was before the delivery (a lease-lost worker whose flush the fence did not refuse), and the production flush fence is
//     a different layer that this test does not exercise. It is pinned so that it cannot be silent, and goes red the day the store refuses it.

func seedStaleChildHistory(t *testing.T, store WorkflowStore, db *sql.DB, d testutil.Dialect) (string, int64) {
	t.Helper()
	ctx := context.Background()
	wfID := uniqueRunID("stale-child-" + string(d))
	seedWorkflowInstance(t, db, d, wfID)
	t0 := time.Now().UnixMilli() - 20000
	for _, r := range []EventRecord{
		{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", TimestampMs: t0},
		{Step: 1, EventType: EventTypeUpdateReceived, UpdateHandlerName: "bump", UpdatePayload: `{}`, UpdateRequestID: "u1", UpdateInterruptsWait: true, TimestampMs: t0 + 10},
		{Step: 2, EventType: EventTypeUpdateCompleted, UpdateRequestID: "u1", Response: `{}`, TimestampMs: t0 + 20},
		{Step: 3, EventType: EventTypeAwaitChild, RunID: "r1", TimestampMs: t0 + 30},
	} {
		if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{r}); err != nil {
			t.Fatalf("seed step %d: %v", r.Step, err)
		}
	}
	cv, ok := store.(interface {
		VerifyWorkflowEvents(ctx context.Context, workflowID string) error
	})
	if !ok {
		t.Fatalf("PRECONDITION FAILED: %T has no VerifyWorkflowEvents, so the chain cannot be read", store)
	}
	if err := cv.VerifyWorkflowEvents(ctx, wfID); err != nil {
		t.Fatalf("PRECONDITION FAILED: the seeded history does not verify before anything is written over it: %v", err)
	}
	return wfID, t0
}

func staleChildRow(t *testing.T, store WorkflowStore, wfID string, step int) EventRecord {
	t.Helper()
	h, err := store.LoadEventHistory(context.Background(), wfID)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	for _, r := range h {
		if r.Step == step {
			return r
		}
	}
	t.Fatalf("no step %d in the history %v", step, evTypesOf(h))
	return EventRecord{}
}

func TestTheLiveChildWaitAfterADeliveryIsAnsweredOverWhileTheStaleRowStaysEmptyAndTheChainVerifies(t *testing.T) {
	eachDialectDB(t, func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore) {
		ctx := context.Background()
		wfID, t0 := seedStaleChildHistory(t, store, db, d)
		// The child finishes; the wake answers over the live wait at step 3, as an answered-over await always does.
		if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{{Step: 3, EventType: EventTypeAwaitChild, RunID: "r1", Response: `{"ok":true}`, TimestampMs: t0 + 5000}}); err != nil {
			t.Fatalf("answering over the live wait: %v", err)
		}
		if got := staleChildRow(t, store, wfID, 3); got.Response != `{"ok":true}` {
			t.Fatalf("the live wait was not answered over: response %q", got.Response)
		}
		if got := staleChildRow(t, store, wfID, 0); got.Response != "" || got.Err != "" || got.TimestampMs != t0 {
			t.Errorf("the stale row at step 0 changed: response %q error %q at %+dms", got.Response, got.Err, got.TimestampMs-t0)
		}
		if err := store.(interface {
			VerifyWorkflowEvents(ctx context.Context, workflowID string) error
		}).VerifyWorkflowEvents(ctx, wfID); err != nil {
			t.Errorf("the chain does not verify after the live wait was answered over: %v", err)
		}
		// A retried flush of the whole prefix, a batch flushed twice: it must change nothing and the chain must still verify.
		var batch []EventRecord
		h, _ := store.LoadEventHistory(ctx, wfID)
		batch = append(batch, h...)
		if err := store.AppendEventHistoryBatch(ctx, wfID, batch); err != nil {
			t.Fatalf("re-flushing the whole history: %v", err)
		}
		if got := staleChildRow(t, store, wfID, 0); got.Response != "" || got.TimestampMs != t0 {
			t.Errorf("a re-flush of the whole history changed the stale row: response %q at %+dms", got.Response, got.TimestampMs-t0)
		}
		if err := store.(interface {
			VerifyWorkflowEvents(ctx context.Context, workflowID string) error
		}).VerifyWorkflowEvents(ctx, wfID); err != nil {
			t.Errorf("the chain does not verify after the whole history was flushed twice: %v", err)
		}
	})
}

// KNOWN GAP, pinned (see the file comment). Delete this test, and the NOT-covered line in the PR, when the store refuses to reopen an empty row that has later rows behind it.
func TestAStrayAnswerOverTheStaleEmptyChildRowIsAcceptedByTheStoreAndBreaksTheChainAtTheNextStep(t *testing.T) {
	eachDialectDB(t, func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore) {
		ctx := context.Background()
		wfID, t0 := seedStaleChildHistory(t, store, db, d)
		if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", Response: `{"ok":true}`, TimestampMs: t0 + 9000}}); err != nil {
			t.Fatalf("the stray write was refused with %v: the store now protects the row. Delete this test and the not-covered line in the PR", err)
		}
		got := staleChildRow(t, store, wfID, 0)
		if got.Response == "" {
			t.Fatalf("the stray answer did not land (step 0 still empty): the store now refuses it silently. Delete this test and the not-covered line in the PR")
		}
		err := store.(interface {
			VerifyWorkflowEvents(ctx context.Context, workflowID string) error
		}).VerifyWorkflowEvents(ctx, wfID)
		if err == nil {
			t.Errorf("the chain verifies after a stray answer rewrote step 0 (response %q): the gap this test pins is closed, delete it", got.Response)
		}
	})
}

// (a) of cleat-review2's ruling on #4536: the writer that could answer over the stale row is a worker that lost its lease and still holds the history as it was before the
// delivery. The production flush (Engine.flushEvent) is what such a worker writes through, and it is fenced on (assigned_to, generation). This pairs the pin above with the
// measurement it leaves open: the SAME stray answer, from a zombie whose generation is stale, is refused and the row and the chain are untouched; and, as the control that says it
// is the FENCE refusing and not something else, from the current owner the identical write lands.
//
// WHAT THIS DOES NOT SHOW, said so that nobody reads it as more. It exercises Engine.flushEvent, the path a worker flushes one event through. The adaptive batch flusher
// checks the fence in partitionFencedBatch BEFORE its statement, and MySQL and SQL Server check it with a Heartbeat BEFORE the write (flush_dialect.go says so on purpose), so
// on those paths the check and the write are two steps and a reclaim between them is a window this test does not close or measure.
func TestAZombiesStrayAnswerOverTheStaleEmptyChildRowIsRefusedByTheFlushFenceAndTheCurrentOwnersIsNot(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			wfID := newIntentWorkflow(t, ctx, store, "stale-child-fence")
			wfA, err := store.ClaimWorkflow(ctx, "worker-A")
			if err != nil || wfA == nil || wfA.ID != wfID {
				t.Fatalf("ClaimWorkflow (A): wf=%v err=%v", wfA, err)
			}
			t0 := time.Now().UnixMilli() - 20000
			for _, r := range []EventRecord{
				{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", TimestampMs: t0},
				{Step: 1, EventType: EventTypeUpdateReceived, UpdateHandlerName: "bump", UpdatePayload: `{}`, UpdateRequestID: "u1", UpdateInterruptsWait: true, TimestampMs: t0 + 10},
				{Step: 2, EventType: EventTypeUpdateCompleted, UpdateRequestID: "u1", Response: `{}`, TimestampMs: t0 + 20},
				{Step: 3, EventType: EventTypeAwaitChild, RunID: "r1", TimestampMs: t0 + 30},
			} {
				if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{r}); err != nil {
					t.Fatalf("seed step %d: %v", r.Step, err)
				}
			}
			cv := store.(interface {
				VerifyWorkflowEvents(ctx context.Context, workflowID string) error
			})
			if err := cv.VerifyWorkflowEvents(ctx, wfID); err != nil {
				t.Fatalf("PRECONDITION FAILED: the seeded history does not verify: %v", err)
			}
			staleGeneration := wfA.Generation
			if reaped, err := store.ReapStaleInstances(ctx, -1*time.Second, 0); err != nil || reaped < 1 {
				t.Fatalf("PRECONDITION FAILED: the workflow was not reclaimed (reaped %d, err %v), so there is no zombie", reaped, err)
			}
			stray := EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", Response: `{"ok":true}`, TimestampMs: t0 + 9000}

			zombie := NewEngine(nil, nil, WithDB(rawDBOf(t, store)), WithWorkflowStore(store), WithWorkerID("worker-A"), WithGeneration(staleGeneration), WithTenantID(DefaultTenantUUID))
			if flushErr := zombie.flushEvent(ctx, wfID, stray, ""); !errors.Is(flushErr, ErrFenceLost) {
				t.Fatalf("the zombie's answer over the stale row: err = %v, want ErrFenceLost", flushErr)
			}
			if got := staleChildRow(t, store, wfID, 0); got.Response != "" {
				t.Errorf("the zombie's answer landed despite the lost fence: step 0 response %q", got.Response)
			}
			if err := cv.VerifyWorkflowEvents(ctx, wfID); err != nil {
				t.Errorf("the chain does not verify after a refused zombie write: %v", err)
			}

			// CONTROL: the new owner, current generation, writes the identical record. It lands, and the chain breaks: so the refusal above was the fence.
			wfB, err := store.ClaimWorkflow(ctx, "worker-B")
			if err != nil || wfB == nil || wfB.ID != wfID {
				t.Fatalf("PRECONDITION FAILED: the new owner did not claim the run: wf=%v err=%v", wfB, err)
			}
			owner := NewEngine(nil, nil, WithDB(rawDBOf(t, store)), WithWorkflowStore(store), WithWorkerID("worker-B"), WithGeneration(wfB.Generation), WithTenantID(DefaultTenantUUID))
			if flushErr := owner.flushEvent(ctx, wfID, stray, ""); flushErr != nil {
				t.Fatalf("the current owner's write was refused with %v: the control cannot say the fence was what refused the zombie", flushErr)
			}
			if got := staleChildRow(t, store, wfID, 0); got.Response == "" {
				t.Errorf("CONTROL FAILED: the current owner's identical write did not land (step 0 still empty), so the fence is not what refused the zombie, and the row may be protected another way (delete the pin above if so)")
			}
		})
	}
}
