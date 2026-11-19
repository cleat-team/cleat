//go:build cgo

package engine

import (
	"context"
	"os"
	"testing"
	"time"
)

// cleat#4177. An await_promise that suspended and later resumed left replay at its OWN pending record on every later replay, with recorded
// history still unread, so every durable call recorded after the await ran again, and a promise that had settled since turned a recorded
// timeout into `resolved`. Two facts make history continue past that record: a completed await records its outcome at the NEXT step (the
// pending row stays), and a timed-out positive-timeout await records nothing, so the guest's next call lands directly after the pending row.
// Flush's immutability guard keeps the duplicate events out of the stored history, so the stored history looked right while the effects ran twice.
//
// This drives the real Go SDK guest of TestASuspendingAwaitPromiseStopsTheGuestBeforeItsTimeoutBranch (testdata/awaitpromisebranch:
// CreatePromise -> AwaitPromise(60s) -> on timeout, a DurableCall) through the wasmtime backend. The history is handed from segment to segment
// directly, so what is measured is the host call and the guest's decoder and not a store.
//
// Each row replays a history a LATER execution would load, and reads three things the original run fixed: the result, the number of durable
// calls in total, and the history that comes back (a replay must neither add an event nor repeat a step).

func TestAnAwaitPromiseReplayedAfterItsResumeDoesNotRerunLaterCalls(t *testing.T) {
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "awaitpromisebranch"))
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	kinds := func(h []EventRecord) []EventType {
		out := make([]EventType, len(h))
		for i, r := range h {
			out[i] = r.EventType
		}
		return out
	}
	steps := func(h []EventRecord) []int {
		out := make([]int, len(h))
		for i, r := range h {
			out[i] = r.Step
		}
		return out
	}
	newEngine := func(caller *mockCaller, promises *mockPromiseStore) *Engine {
		return NewEngine(rt, caller, withWasmtimeBackend(t), WithPromiseStore(promises),
			WithClock(func() int64 { return time.Now().UnixMilli() }))
	}
	run := func(t *testing.T, caller *mockCaller, promises *mockPromiseStore, hist []EventRecord) (string, []EventRecord, bool) {
		t.Helper()
		var (
			res  string
			out  []EventRecord
			susp *SuspendResult
			err  error
		)
		if hist == nil {
			res, out, susp, _, _, err = newEngine(caller, promises).Execute(ctx, wasmBytes, "wait", awaitPromiseBranchInput)
		} else {
			res, out, susp, _, _, err = newEngine(caller, promises).Replay(ctx, wasmBytes, "wait", awaitPromiseBranchInput, hist)
		}
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return res, out, susp != nil
	}

	// Segment 1: the first execution suspends at the await. Its history is [create_promise, await_promise].
	c1 := &mockCaller{}
	_, first, suspended := run(t, c1, &mockPromiseStore{}, nil)
	if !suspended || len(c1.calls) != 0 {
		t.Fatalf("PRECONDITION FAILED: segment 1 suspended=%v with %d call(s), want a suspension and none: %v", suspended, len(c1.calls), kinds(first))
	}

	t.Run("a timed-out await: a later replay neither re-runs the call nor changes its answer", func(t *testing.T) {
		// Segment 4: resumed past the deadline. The timeout is real, the guest's call runs once.
		expired := append([]EventRecord(nil), first...)
		for i := range expired {
			if expired[i].EventType == EventTypeAwaitPromise {
				expired[i].TimestampMs -= 61_000
			}
		}
		caller := &mockCaller{}
		promises := &mockPromiseStore{}
		res4, h4, susp := run(t, caller, promises, expired)
		if susp || len(caller.calls) != 1 || (res4 != `"timed_out"` && res4 != "timed_out") {
			t.Fatalf("PRECONDITION FAILED: the resume past the deadline gave result %q, %d call(s), suspended=%v; want timed_out, 1, false (history %v)",
				res4, len(caller.calls), susp, kinds(h4))
		}

		// Segment 5: a later replay of segment 4's history, the promise still pending.
		res5, h5, susp := run(t, caller, promises, h4)
		if susp || len(caller.calls) != 1 || res5 != res4 || len(h5) != len(h4) {
			t.Errorf("a later replay of a timed-out await gave result %q (recorded %q), %d durable call(s) in total (want 1), history %v -> %v, suspended=%v",
				res5, res4, len(caller.calls), kinds(h4), kinds(h5), susp)
		}

		// Segment 6: the same replay after the promise settled. The recorded answer was timed_out, and a replay must give it again.
		promises.status, promises.result = "resolved", `"late"`
		res6, h6, _ := run(t, caller, promises, h4)
		if res6 != res4 || len(caller.calls) != 1 || len(h6) != len(h4) {
			t.Errorf("a replay after the promise settled gave result %q where the original run said %q, %d durable call(s) in total (want 1), history %v -> %v",
				res6, res4, len(caller.calls), kinds(h4), kinds(h6))
		}
	})

	t.Run("a resolved await: a later replay repeats no step and records nothing", func(t *testing.T) {
		caller := &mockCaller{}
		promises := &mockPromiseStore{status: "resolved", result: `"early"`}
		resR, hR, susp := run(t, caller, promises, first)
		if susp || len(caller.calls) != 0 {
			t.Fatalf("PRECONDITION FAILED: the resolved resume suspended=%v with %d call(s): %v", susp, len(caller.calls), kinds(hR))
		}
		// Segment 7: a later replay of that history.
		resL, hL, susp := run(t, caller, promises, hR)
		if susp || resL != resR || len(hL) != len(hR) {
			t.Errorf("a later replay of a resolved await gave result %q (recorded %q), history %v steps %v -> %v steps %v, suspended=%v",
				resL, resR, kinds(hR), steps(hR), kinds(hL), steps(hL), susp)
		}
		if len(caller.calls) != 0 {
			t.Errorf("a later replay of a resolved await made %d durable call(s), want 0", len(caller.calls))
		}
	})
}
