//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// cleat#4511 on the real backend and every configured dialect: a real guest (testdata/updatelongwait, ONE AwaitSignals of an hour in a loop) is suspended in its
// wait, an update is created AFTER it suspended, and the run is woken the way the worker wakes it (a replay of the history it wrote). The update must be handled:
// the handler runs, the caller's promise resolves with its result, and nothing is left pending. Three updates, one wait, no slicing; then a signal ends the wait
// (the guest reads its loop's condition when its wait returns, so a handled update does not end it).
//
// testdata/updatedispatch waits in one-second slices and so took the path that always worked, which is why no test saw this. The row the issue reported is
// the FIRST update here, arriving after the first segment ended.
//
// It uses the store's own update calls (CreatePromise and CreateUpdateRequest, as the API does; GetPendingUpdateRequests and CompleteUpdateRequest through the
// engine), so what differs per dialect is read from the dialect.

// updLongWaitSegment runs one segment of the guest: the first (no history) or a replay of what the previous ones wrote.
func updLongWaitSegment(t *testing.T, rt *Runtime, wt *wasmtimeBackend, wasm []byte, store WorkflowStore, wf string, hist []EventRecord) (string, []EventRecord, *SuspendResult) {
	t.Helper()
	us, ok := store.(UpdateStore)
	if !ok {
		t.Fatalf("PRECONDITION FAILED: %T is not an UpdateStore, so the engine would poll no updates", store)
	}
	ps, ok := store.(PromiseStore)
	if !ok {
		t.Fatalf("PRECONDITION FAILED: %T is not a PromiseStore", store)
	}
	eng := NewEngine(rt, &mockCaller{}, WithBackends(WasmtimeLanguages, wt), WithWorkflowID(wf), WithSignalStore(store), WithUpdateStore(us), WithPromiseStore(ps))
	var (
		res  string
		out  []EventRecord
		susp *SuspendResult
		err  error
	)
	if hist == nil {
		res, out, susp, _, _, err = eng.Execute(context.Background(), wasm, "update_in_a_long_wait", json.RawMessage(`{}`))
	} else {
		res, out, susp, _, _, err = eng.Replay(context.Background(), wasm, "update_in_a_long_wait", json.RawMessage(`{}`), hist)
	}
	if err != nil {
		t.Fatalf("segment: %v", err)
	}
	return res, out, susp
}

func TestAnUpdateThatArrivesWhileARunIsSuspendedInOneLongWaitIsHandledOnEveryDialect(t *testing.T) {
	if testing.Short() {
		t.Skip("no database in -short mode; every per-dialect subtest would skip")
	}
	wasm, err := os.ReadFile(buildFixtureWasm(t, "updatelongwait"))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	rt, wt := newResidentBackend(t, 30*time.Second)
	var ran []string
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()
			ps, _ := store.(PromiseStore)
			us, _ := store.(UpdateStore)
			if ps == nil || us == nil {
				t.Fatalf("PRECONDITION FAILED: %T lacks the promise or update store", store)
			}
			wf := startRealRun(t, store, "upd-long-wait-"+backend.Name())

			// The first segment runs to the wait and suspends in it. PRECONDITION: it really did, and the last row is the wait's own.
			res, hist, susp := updLongWaitSegment(t, rt, wt, wasm, store, wf, nil)
			if susp == nil || !strings.Contains(susp.Reason, "await_signals") {
				t.Fatalf("PRECONDITION FAILED: the first segment ended with result %q and suspension %+v, not suspended in await_signals", res, susp)
			}
			if len(hist) == 0 || hist[len(hist)-1].EventType != EventTypeAwaitSignals {
				t.Fatalf("PRECONDITION FAILED: the history ends in %v, not in the wait's own row", evTypesOf(hist))
			}
			firstWait := hist[len(hist)-1]
			firstDeadline := firstWait.TimestampMs + firstWait.TimeoutMs

			for i := 1; i <= 3; i++ {
				// The update arrives AFTER the run suspended: the API's two writes, then the wake is a replay of the recorded history.
				promiseID := "upd-promise-" + backend.Name() + "-" + string(rune('0'+i))
				if err := ps.CreatePromise(ctx, wf, "update:bump", promiseID); err != nil {
					t.Fatalf("CreatePromise: %v", err)
				}
				if err := store.CreateUpdateRequest(ctx, wf, "bump", `{"i":`+string(rune('0'+i))+`}`, promiseID); err != nil {
					t.Fatalf("CreateUpdateRequest %d: %v", i, err)
				}
				res, hist, susp = updLongWaitSegment(t, rt, wt, wasm, store, wf, hist)

				status, result, errMsg, err := ps.GetPromise(ctx, wf, promiseID)
				if err != nil {
					t.Fatalf("GetPromise %d: %v", i, err)
				}
				if status != "resolved" || !strings.Contains(result, `"n":`+string(rune('0'+i))) {
					t.Fatalf("update %d: its promise is %q (result %q, error %q), want resolved with n=%d. The run replayed, reached the same wait and suspended again WITHOUT handling the update (cleat#4511); history now %v",
						i, status, result, errMsg, i, evTypesOf(hist))
				}
				if pending, err := us.GetPendingUpdateRequests(ctx, wf); err != nil || len(pending) != 0 {
					t.Fatalf("update %d: %d request(s) still pending (err %v)", i, len(pending), err)
				}
				if i < 3 {
					if susp == nil || !strings.Contains(susp.Reason, "await_signals") {
						t.Fatalf("update %d: the run did not go back to its wait (result %q, suspension %+v)", i, res, susp)
					}
					// The guest called AwaitSignals ONCE: the SDK's second wait is the same wait, and an update must not restart its hour.
					last := hist[len(hist)-1]
					if last.EventType != EventTypeAwaitSignals {
						t.Fatalf("update %d: the history ends in %s, want the second await_signals", i, last.EventType)
					}
					if d := last.TimestampMs + last.TimeoutMs; d != firstDeadline {
						t.Errorf("update %d: the wait's deadline is %d, the first wait's was %d: the update restarted the guest's timeout", i, d, firstDeadline)
					}
				}
			}
			// The guest reads its loop's condition when its wait returns, so the run ends on a signal. This is also the replay of three marked deliveries
			// followed by a delivery to the wait that came after them.
			if err := store.DeliverSignal(ctx, wf, "done", `{}`); err != nil {
				t.Fatalf("DeliverSignal(done): %v", err)
			}
			res, hist, susp = updLongWaitSegment(t, rt, wt, wasm, store, wf, hist)
			if susp != nil || !strings.Contains(res, `"n":3`) {
				t.Errorf("after three updates and the signal the run ended with result %q and suspension %+v, want it complete with n=3", res, susp)
			}
			marked := 0
			for _, r := range hist {
				if r.EventType == EventTypeUpdateReceived && r.UpdateInterruptsWait {
					marked++
				}
			}
			if marked != 3 {
				t.Errorf("%d of the three deliveries are marked as having interrupted a wait, want 3: the history that comes back from the store must carry the marker", marked)
			}
			ran = append(ran, backend.Name())
		})
	}
	if len(ran) == 0 {
		t.Fatal("no backend ran, so this test measured nothing")
	}
}
