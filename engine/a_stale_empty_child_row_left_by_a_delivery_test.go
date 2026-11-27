package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#4511, cleat-review2's SHOULD and then its ruling on #4536: "the stale empty child row stays reopenable forever under the ON CONFLICT guards."
//
// Delivering an update across a child wait leaves the wait's own row EMPTY at step N with the delivery, its completion and the guest's second wait behind it. Before this change an
// empty child row was always the LAST row of its history, so an answer landing over it was the last row's answer and benign. Now a stray ANSWER over the stale row breaks the chain at
// step N+1 (measured on PostgreSQL, MySQL and SQL Server before the guard changed), and verification then fails the run on every later replay.
//
// THE GUARD (seven copies, one rule): a pending await_child, await_promise or await_all_children row is reopened only when the NEXT step is not an update_received. Narrow on
// purpose, because the broader "only while it is the last row" would drop a case the engine documents as legitimate and the store accepts today: a pending row with a defer
// drain's own records behind it (OverPending, cleat#3429; TestAnAnswerOverAPendingChildRowWithADeferDrainsRecordsBehindItStillLands). Only this change's delivery path puts an
// update_received directly behind a pending row: a pending row suspends the guest, and a defer drain refuses fresh polls, so a drain never records one.
//
// What each test below covers, and which copy each one reaches, is in its own comment.

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

// A stray ANSWER over the stale row is REFUSED, the row and the chain are untouched. Two shapes, because the PostgreSQL store has two copies of the guard (a single-record write
// goes through appendOneEvent, two or more through the prepared statement) and every other dialect has one. The companion in the batch is a NEW step, so the only conflict is the
// stray itself; its own checksum chains from the stray's, so the chain is read only for the single-record shape.
func TestAStrayAnswerOverTheStaleEmptyChildRowIsRefusedAndTheChainStaysIntact(t *testing.T) {
	eachDialectDB(t, func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore) {
		for _, tc := range []struct {
			name      string
			companion bool
		}{{"alone", false}, {"in_a_batch_of_two", true}} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				ctx := context.Background()
				wfID, t0 := seedStaleChildHistory(t, store, db, d)
				batch := []EventRecord{{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", Response: `{"ok":true}`, TimestampMs: t0 + 9000}}
				if tc.companion {
					batch = append(batch, EventRecord{Step: 4, EventType: EventTypeCall, Service: "svc", Op: "op", Response: `{}`, TimestampMs: t0 + 40})
				}
				if err := store.AppendEventHistoryBatch(ctx, wfID, batch); err != nil {
					t.Fatalf("the stray write returned %v: a refused reopen is silent, an error here is something else", err)
				}
				if tc.companion {
					if got := staleChildRow(t, store, wfID, 4); got.EventType != EventTypeCall {
						t.Fatalf("PRECONDITION FAILED: the companion did not land (step 4 is %q), so the batch statement was not the one measured", got.EventType)
					}
				}
				if got := staleChildRow(t, store, wfID, 0); got.Response != "" || got.TimestampMs != t0 {
					t.Errorf("the stray answer over the stale row landed: step 0 response %q at %+dms", got.Response, got.TimestampMs-t0)
				}
				if !tc.companion {
					if err := store.(interface {
						VerifyWorkflowEvents(ctx context.Context, workflowID string) error
					}).VerifyWorkflowEvents(ctx, wfID); err != nil {
						t.Errorf("the chain does not verify after a refused stray answer: %v", err)
					}
				}
			})
		}
	})
}

// THE PRODUCTION FLUSH PATH (Engine.flushEvent: flush.go's statement on PostgreSQL, flushEventForStep on MySQL and SQL Server). Three writers, one history:
//   - a lease-lost worker's stray answer over the stale row: refused by the FENCE (ErrFenceLost), as before this change;
//   - the CURRENT owner's identical write: not an error, and refused by the GUARD, so the row stays empty and the chain verifies (the case the fence cannot cover, because the
//     owner is the lease holder);
//   - the current owner's answer over its own LIVE pending row (step 3, no update_received behind it): lands, and the chain verifies. That is the control that says the guard
//     refuses the stale row and not every answer.
//
// What this does not show: the adaptive flusher and MySQL/SQL Server check the fence BEFORE the write, so a reclaim between the check and the write is not measured here.
func TestTheFlushRefusesAStrayAnswerOverTheStaleEmptyChildRowFromAZombieAndFromTheOwnerAndStillAnswersTheLiveWait(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			wfID := newIntentWorkflow(t, ctx, store, "stale-child-flush")
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
			verify := store.(interface {
				VerifyWorkflowEvents(ctx context.Context, workflowID string) error
			})
			if err := verify.VerifyWorkflowEvents(ctx, wfID); err != nil {
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

			wfB, err := store.ClaimWorkflow(ctx, "worker-B")
			if err != nil || wfB == nil || wfB.ID != wfID {
				t.Fatalf("PRECONDITION FAILED: the new owner did not claim the run: wf=%v err=%v", wfB, err)
			}
			owner := NewEngine(nil, nil, WithDB(rawDBOf(t, store)), WithWorkflowStore(store), WithWorkerID("worker-B"), WithGeneration(wfB.Generation), WithTenantID(DefaultTenantUUID))
			if flushErr := owner.flushEvent(ctx, wfID, stray, ""); flushErr != nil {
				t.Fatalf("the current owner's stray write returned %v: a refused reopen is not an error", flushErr)
			}
			if got := staleChildRow(t, store, wfID, 0); got.Response != "" || got.TimestampMs != t0 {
				t.Errorf("the current owner's stray answer over the stale row landed: step 0 response %q at %+dms", got.Response, got.TimestampMs-t0)
			}
			if err := verify.VerifyWorkflowEvents(ctx, wfID); err != nil {
				t.Errorf("the chain does not verify after both refused writes: %v", err)
			}

			// CONTROL: the live pending row at step 3 is answered over by its owner, through the same path.
			live := EventRecord{Step: 3, EventType: EventTypeAwaitChild, RunID: "r1", Response: `{"ok":true}`, TimestampMs: t0 + 9000}
			// PostgreSQL's single-statement flush chains from the checksum the CALLER hands it (the session's, in production); MySQL and SQL Server read it in the store and ignore it.
			prev := ""
			if backend.Name() == "postgres" {
				if err := rawDBOf(t, store).QueryRowContext(ctx, `SELECT checksum FROM event_history WHERE workflow_id = $1 AND step = 2`, wfID).Scan(&prev); err != nil {
					t.Fatalf("PRECONDITION FAILED: reading step 2's stored checksum: %v", err)
				}
			}
			if flushErr := owner.flushEvent(ctx, wfID, live, prev); flushErr != nil {
				t.Fatalf("the owner's answer over its live pending row: %v", flushErr)
			}
			if got := staleChildRow(t, store, wfID, 3); got.Response != `{"ok":true}` {
				t.Errorf("CONTROL FAILED: the owner's answer over its LIVE wait did not land (response %q): the guard refuses more than the stale row", got.Response)
			}
			if err := verify.VerifyWorkflowEvents(ctx, wfID); err != nil {
				t.Errorf("the chain does not verify after the live wait was answered over: %v", err)
			}
		})
	}
}

// THE ADAPTIVE BATCH FLUSHER (PostgreSQL only: batch_flush_is_postgres_only_test.go), which has two copies of the statement: the first attempt (Flush, through flushAndNotify) and the
// retry (retryBatchFlush), reached directly here because it runs only after a first failure. Each gets the stray answer over the stale row and the live row's answer as the control.
func TestTheAdaptiveBatchFlusherRefusesAStrayAnswerOverTheStaleEmptyChildRowAndStillAnswersTheLiveWait(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	store := NewPostgresStore(db)
	ctx := context.Background()
	af := NewAdaptiveFlusher(db, DefaultTenantUUID, 8*time.Millisecond, 1, 0, 0, 0)
	af.mu.Lock()
	af.batchMode = true
	af.mu.Unlock()

	for _, path := range []string{"the_first_attempt", "the_retry"} {
		path := path
		t.Run(path, func(t *testing.T) {
			wfID, t0 := seedStaleChildHistory(t, store, db, testutil.DialectPostgres)
			write := func(rec EventRecord) {
				t.Helper()
				if path == "the_first_attempt" {
					done, queued := af.Flush(ctx, wfID, rec, computeEventChecksum(rec, ""), "", 0)
					if !queued {
						t.Fatalf("PRECONDITION FAILED: the flusher did not take the event onto the batch arm")
					}
					if err := <-done; err != nil {
						t.Fatalf("the batch flush of step %d returned %v", rec.Step, err)
					}
					return
				}
				entry, err := af.prepareEntry(wfID, rec, computeEventChecksum(rec, ""))
				if err != nil {
					t.Fatalf("prepareEntry: %v", err)
				}
				body, err := json.Marshal([]map[string]interface{}{batchEntryJSONRow(entry)})
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if err := retryBatchFlush(ctx, af, body, 1); err != nil {
					t.Fatalf("the retry statement for step %d returned %v", rec.Step, err)
				}
			}
			write(EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", Response: `{"ok":true}`, TimestampMs: t0 + 9000})
			if got := staleChildRow(t, store, wfID, 0); got.Response != "" || got.TimestampMs != t0 {
				t.Errorf("the stray answer over the stale row landed through %s: step 0 response %q at %+dms", path, got.Response, got.TimestampMs-t0)
			}
			write(EventRecord{Step: 3, EventType: EventTypeAwaitChild, RunID: "r1", Response: `{"ok":true}`, TimestampMs: t0 + 9000})
			if got := staleChildRow(t, store, wfID, 3); got.Response != `{"ok":true}` {
				t.Errorf("CONTROL FAILED: the answer over the LIVE wait did not land through %s (response %q)", path, got.Response)
			}
		})
	}
}

// THE DRAIN CASE, measured before the guard changes (cleat-review2's first condition on #4536): a pending child row with a defer drain's OWN records behind it is a state the
// engine documents as legitimate (step_dispatcher.go, OverPending: "a defer drain's own records (cleat#3429)"), so a guard that refused every reopen with a row behind it would drop its
// answer. The record behind it here is the shape a drain writes: a call stamped InDeferPhase. The answer over the pending row must land.
func TestAnAnswerOverAPendingChildRowWithADeferDrainsRecordsBehindItStillLands(t *testing.T) {
	eachDialectDB(t, func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore) {
		ctx := context.Background()
		wfID := uniqueRunID("drain-behind-" + string(d))
		seedWorkflowInstance(t, db, d, wfID)
		t0 := time.Now().UnixMilli() - 20000
		for _, r := range []EventRecord{
			{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", TimestampMs: t0},
			{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "cleanup", Response: `{}`, InDeferPhase: true, TimestampMs: t0 + 10},
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
		if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", Response: `{"ok":true}`, TimestampMs: t0 + 5000}}); err != nil {
			t.Fatalf("answering over the pending row: %v", err)
		}
		got := staleChildRow(t, store, wfID, 0)
		t.Logf("drain case: step 0 response %q; verify after: %v", got.Response, cv.VerifyWorkflowEvents(ctx, wfID))
		if got.Response != `{"ok":true}` {
			t.Errorf("the answer over a pending child row with a defer drain's record behind it did not land: response %q", got.Response)
		}
	})
}

// The guard's list is await_child, await_promise and await_all_children, and a delivery can interrupt a wait of any of them (await_any_child records its outcome at a NEW step and is not
// in the list). The same stray answer over each kind of stale row, one record at a time: refused, row and chain untouched. The promise row is the timed one, which the fix also handles
// (its stale row keeps the wait's deadline), so it is the one a reviewer asked about by name.
func TestAStrayAnswerOverAnyKindOfStalePendingAwaitRowIsRefused(t *testing.T) {
	for _, kind := range []struct {
		name            string
		pending, answer func(t0 int64) EventRecord
		answered        func(r EventRecord) bool
	}{
		{"await_child",
			func(t0 int64) EventRecord {
				return EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", TimestampMs: t0}
			},
			func(t0 int64) EventRecord {
				return EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "r1", Response: `{"ok":true}`, TimestampMs: t0 + 9000}
			},
			func(r EventRecord) bool { return r.Response != "" }},
		{"await_promise",
			func(t0 int64) EventRecord {
				return EventRecord{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: "p1", TimeoutMs: 3_600_000, TimestampMs: t0}
			},
			func(t0 int64) EventRecord {
				return EventRecord{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: "p1", TimeoutMs: 3_600_000, PromiseResult: `{"v":1}`, TimestampMs: t0 + 9000}
			},
			func(r EventRecord) bool { return r.PromiseResult != "" }},
		{"await_all_children",
			func(t0 int64) EventRecord {
				return EventRecord{Step: 0, EventType: EventTypeAwaitAllChildren, Request: `["c1"]`, TimestampMs: t0}
			},
			func(t0 int64) EventRecord {
				return EventRecord{Step: 0, EventType: EventTypeAwaitAllChildren, Request: `["c1"]`, Response: `[{"run_id":"c1"}]`, TimestampMs: t0 + 9000}
			},
			func(r EventRecord) bool { return r.Response != "" }},
	} {
		kind := kind
		t.Run(kind.name, func(t *testing.T) {
			eachDialectDB(t, func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore) {
				ctx := context.Background()
				wfID := uniqueRunID("stale-" + kind.name + "-" + string(d))
				seedWorkflowInstance(t, db, d, wfID)
				t0 := time.Now().UnixMilli() - 20000
				for _, r := range []EventRecord{
					kind.pending(t0),
					{Step: 1, EventType: EventTypeUpdateReceived, UpdateHandlerName: "bump", UpdatePayload: `{}`, UpdateRequestID: "u1", UpdateInterruptsWait: true, TimestampMs: t0 + 10},
					{Step: 2, EventType: EventTypeUpdateCompleted, UpdateRequestID: "u1", Response: `{}`, TimestampMs: t0 + 20},
				} {
					if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{r}); err != nil {
						t.Fatalf("seed step %d: %v", r.Step, err)
					}
				}
				verify := store.(interface {
					VerifyWorkflowEvents(ctx context.Context, workflowID string) error
				})
				if err := verify.VerifyWorkflowEvents(ctx, wfID); err != nil {
					t.Fatalf("PRECONDITION FAILED: the seeded history does not verify: %v", err)
				}
				if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{kind.answer(t0)}); err != nil {
					t.Fatalf("the stray write returned %v: a refused reopen is silent", err)
				}
				if got := staleChildRow(t, store, wfID, 0); kind.answered(got) || got.TimestampMs != t0 {
					t.Errorf("the stray answer over a stale %s row landed: %+v", kind.name, got)
				}
				if err := verify.VerifyWorkflowEvents(ctx, wfID); err != nil {
					t.Errorf("the chain does not verify after a refused stray answer over a stale %s row: %v", kind.name, err)
				}
			})
		})
	}
}
