package engine

// B4, continued: the write-ahead call-intent path (engine/store_intent.go)
// carried no fencing token either. WriteCallIntent runs before the call is
// dispatched, so a fence check there stops a zombie from even writing the
// intent row. CompleteCallIntent and ResolveCallIntent run after, when the
// call has already happened, so a fence check there cannot un-happen the
// call -- but it stops a zombie from overwriting a pending row a new owner
// may have already acted on. See engine/store_intent.go's callIntentStore
// doc for the full reasoning.
//
// Same zombie-writer construction as flush_fence_test.go and
// fence_lost_integration_test.go's buildZombieWriterScenario: claim, capture
// the generation, ReapStaleInstances(-1s) to reclaim unconditionally (no
// sleep, no race), then act as the stale worker.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWriteCallIntent_FenceLost(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			st := intentStoreOf(t, store)

			wfID := newIntentWorkflow(t, ctx, store, "intent-write-fence-lost")

			wfA, err := store.ClaimWorkflow(ctx, "worker-A")
			if err != nil || wfA == nil || wfA.ID != wfID {
				t.Fatalf("ClaimWorkflow (A): wf=%v err=%v", wfA, err)
			}
			staleGeneration := wfA.Generation

			reaped, err := store.ReapStaleInstances(ctx, -1*time.Second)
			if err != nil {
				t.Fatalf("ReapStaleInstances: %v", err)
			}
			if reaped < 1 {
				t.Fatalf("ReapStaleInstances reclaimed %d instances, want >= 1", reaped)
			}

			intent := EventRecord{Step: 0, EventType: EventTypeCall, Service: intentService, Op: intentOperation, Request: `{"amount":100}`}
			err = st.WriteCallIntent(ctx, wfID, intent, "worker-A", staleGeneration)
			if !errors.Is(err, ErrFenceLost) {
				t.Fatalf("WriteCallIntent under a lost fence: err = %v, want ErrFenceLost", err)
			}

			// The whole point of fencing this one before the call, not just
			// after: the row must not exist at all, so the call this intent
			// would have preceded is provably never dispatched by this path.
			hist, err := store.LoadEventHistory(ctx, wfID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(hist) != 0 {
				t.Fatalf("B4 regression: zombie's WriteCallIntent persisted despite a lost fence: history = %+v", hist)
			}
		})
	}
}

func TestWriteCallIntent_FenceHeld(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			st := intentStoreOf(t, store)

			wfID := newIntentWorkflow(t, ctx, store, "intent-write-fence-held")
			wf, err := store.ClaimWorkflow(ctx, "worker-live")
			if err != nil || wf == nil || wf.ID != wfID {
				t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
			}

			intent := EventRecord{Step: 0, EventType: EventTypeCall, Service: intentService, Op: intentOperation, Request: `{"amount":100}`}
			if err := st.WriteCallIntent(ctx, wfID, intent, "worker-live", wf.Generation); err != nil {
				t.Fatalf("WriteCallIntent under a held fence: %v, want success", err)
			}

			rec := stepRecord(t, ctx, store, wfID, 0)
			if !rec.Pending {
				t.Error("a legitimately fenced WriteCallIntent did not leave a pending row")
			}
		})
	}
}

// TestCompleteCallIntent_FenceLost is the sharper half: the call already
// happened (this is what CompleteCallIntent records), so fencing here cannot
// stop the dispatch -- it stops the zombie from overwriting a pending row
// that its successor, worker-B, may since have acted on.
func TestCompleteCallIntent_FenceLost(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			st := intentStoreOf(t, store)

			wfID := newIntentWorkflow(t, ctx, store, "intent-complete-fence-lost")

			wfA, err := store.ClaimWorkflow(ctx, "worker-A")
			if err != nil || wfA == nil || wfA.ID != wfID {
				t.Fatalf("ClaimWorkflow (A): wf=%v err=%v", wfA, err)
			}
			staleGeneration := wfA.Generation

			// A writes the intent while its claim is still live -- exactly
			// what a crash between dispatch and completion leaves behind.
			intent := EventRecord{Step: 0, EventType: EventTypeCall, Service: intentService, Op: intentOperation, Request: `{"amount":100}`}
			if err := st.WriteCallIntent(ctx, wfID, intent, "worker-A", staleGeneration); err != nil {
				t.Fatalf("WriteCallIntent: %v", err)
			}

			reaped, err := store.ReapStaleInstances(ctx, -1*time.Second)
			if err != nil {
				t.Fatalf("ReapStaleInstances: %v", err)
			}
			if reaped < 1 {
				t.Fatalf("ReapStaleInstances reclaimed %d instances, want >= 1", reaped)
			}

			// A wakes up, believing it still owns the workflow, and tries to
			// complete the intent with a real (not stale) outcome.
			done := intent
			done.Response = `{"charged":true,"by":"zombie-A"}`
			done.TimestampMs = time.Now().UnixMilli()
			payload, _ := eventRecordToPayload(done)
			checksum := computeEventChecksum(done, "")
			err = st.CompleteCallIntent(ctx, wfID, done, payload, checksum, "worker-A", staleGeneration)
			if !errors.Is(err, ErrFenceLost) {
				t.Fatalf("CompleteCallIntent under a lost fence: err = %v, want ErrFenceLost", err)
			}

			// The row must still be pending -- A's outcome must not have
			// landed, so a new owner reading this row still sees ambiguity
			// rather than a completed-looking result A was not fenced to write.
			after := stepRecord(t, ctx, store, wfID, 0)
			if !after.Pending {
				t.Fatalf("B4 regression: zombie's CompleteCallIntent overwrote the pending row: %+v", after)
			}
		})
	}
}

func TestCompleteCallIntent_FenceHeld(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			st := intentStoreOf(t, store)

			wfID := newIntentWorkflow(t, ctx, store, "intent-complete-fence-held")
			wf, err := store.ClaimWorkflow(ctx, "worker-live")
			if err != nil || wf == nil || wf.ID != wfID {
				t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
			}

			intent := EventRecord{Step: 0, EventType: EventTypeCall, Service: intentService, Op: intentOperation, Request: `{"amount":100}`}
			if err := st.WriteCallIntent(ctx, wfID, intent, "worker-live", wf.Generation); err != nil {
				t.Fatalf("WriteCallIntent: %v", err)
			}

			done := intent
			done.Response = `{"charged":true}`
			done.TimestampMs = time.Now().UnixMilli()
			payload, _ := eventRecordToPayload(done)
			checksum := computeEventChecksum(done, "")
			if err := st.CompleteCallIntent(ctx, wfID, done, payload, checksum, "worker-live", wf.Generation); err != nil {
				t.Fatalf("CompleteCallIntent under a held fence: %v, want success", err)
			}

			after := stepRecord(t, ctx, store, wfID, 0)
			if after.Pending {
				t.Error("a legitimately fenced CompleteCallIntent left the row pending")
			}
		})
	}
}
