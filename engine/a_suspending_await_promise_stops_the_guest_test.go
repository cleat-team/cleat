//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// cleat#3383: a suspending AwaitPromise returned packAwaitPromiseResult(0, true,
// 0) -- byte-identical to a genuine timeout -- and no SDK's await_promise
// binding checked for a stop, so the guest took its timeout branch and a
// DurableCall there executed before the run suspended. Same shape as #3378,
// which fixed AwaitSignals; AwaitPromise has three suspend sites, all covered
// here, and a fourth return that really is a timeout.
//
// This drives a compiled Go SDK guest (testdata/awaitpromisebranch) through
// the wasmtime backend: CreatePromise -> AwaitPromise(60s) -> on timeout, a
// DurableCall. The defect is not dialect-specific -- it lives between the host
// call and the guest's decoder -- so the history is handed from segment to
// segment directly rather than through a store.
//
// AwaitPromise's resume path compares the recorded deadline against
// time.Now(), not the engine clock (engine/promises.go), so the segments move
// the RECORDED await's timestamp rather than a clock.

var awaitPromiseBranchInput = json.RawMessage(`{"input":{"attempt":1}}`)

func TestASuspendingAwaitPromiseStopsTheGuestBeforeItsTimeoutBranch(t *testing.T) {
	wasmPath := buildFixtureWasm(t, "awaitpromisebranch")
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	// status "" is GetPromise's pending case: never resolved, never rejected.
	promises := &mockPromiseStore{}
	newEngine := func(clockMs int64) *Engine {
		return NewEngine(rt, caller, withWasmtimeBackend(t), WithPromiseStore(promises),
			WithClock(func() int64 { return clockMs }))
	}
	kinds := func(h []EventRecord) []EventType {
		out := make([]EventType, len(h))
		for i, r := range h {
			out[i] = r.EventType
		}
		return out
	}
	awaitIndex := func(h []EventRecord) int {
		t.Helper()
		for i, r := range h {
			if r.EventType == EventTypeAwaitPromise {
				return i
			}
		}
		t.Fatalf("no await_promise record in %v", kinds(h))
		return -1
	}
	// withAwait returns a copy of the history with its await_promise record
	// altered, so one fresh execution can be resumed under several conditions.
	withAwait := func(h []EventRecord, edit func(*EventRecord)) []EventRecord {
		out := append([]EventRecord(nil), h...)
		edit(&out[awaitIndex(out)])
		return out
	}

	// ---- segment 1: first execution. The await must suspend the run before
	// the guest reaches its timeout branch.
	_, hist, susp, _, _, err := newEngine(time.Now().UnixMilli()).Execute(ctx, wasmBytes, "wait", awaitPromiseBranchInput)
	if err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	if n := len(caller.calls); n != 0 {
		t.Fatalf("segment 1 made %d durable call(s), want 0. The await had only just begun its "+
			"60s wait; a call here is the guest acting on a timeout that did not happen "+
			"(cleat#3383). History: %v", n, kinds(hist))
	}
	if susp == nil {
		t.Fatal("segment 1 did not suspend at the await")
	}
	if got := hist[awaitIndex(hist)]; got.TimeoutMs != 60_000 {
		t.Fatalf("segment 1 recorded timeout_ms=%d, want 60000", got.TimeoutMs)
	}

	// ---- segment 2: woken early. The recorded deadline is still ahead, so
	// the resumed await must suspend again, not time out.
	_, h2, susp, _, _, err := newEngine(time.Now().UnixMilli()).Replay(ctx, wasmBytes, "wait", awaitPromiseBranchInput, hist)
	if err != nil {
		t.Fatalf("segment 2: %v", err)
	}
	if n := len(caller.calls); n != 0 {
		t.Fatalf("segment 2 (woken early, deadline ahead) made %d durable call(s), want 0", n)
	}
	if susp == nil || len(h2) != len(hist) {
		t.Fatalf("segment 2: suspended=%v history=%v; want a re-suspend that records nothing", susp != nil, kinds(h2))
	}

	// ---- segment 3: an await recorded with no timeout. Nothing to measure
	// against, so the run keeps waiting -- and must not report a timeout.
	noTimeout := withAwait(hist, func(r *EventRecord) { r.TimeoutMs = 0 })
	_, _, susp, _, _, err = newEngine(time.Now().UnixMilli()).Replay(ctx, wasmBytes, "wait", awaitPromiseBranchInput, noTimeout)
	if err != nil {
		t.Fatalf("segment 3: %v", err)
	}
	if n := len(caller.calls); n != 0 {
		t.Fatalf("segment 3 (await recorded with no timeout) made %d durable call(s), want 0", n)
	}
	if susp == nil {
		t.Fatal("segment 3 did not suspend")
	}

	// ---- segment 4: past the recorded deadline. Now the timeout is real: the
	// guest takes its timeout branch and the call runs exactly once.
	expired := withAwait(hist, func(r *EventRecord) { r.TimestampMs -= 61_000 })
	result, _, susp, _, _, err := newEngine(time.Now().UnixMilli()).Replay(ctx, wasmBytes, "wait", awaitPromiseBranchInput, expired)
	if err != nil {
		t.Fatalf("segment 4: %v", err)
	}
	if susp != nil {
		t.Fatalf("segment 4 suspended: %v; the deadline had passed", susp.Reason)
	}
	if n := len(caller.calls); n != 1 {
		t.Fatalf("segment 4 made %d durable call(s) in total, want exactly 1 (the genuine timeout)", n)
	}
	if result != `"timed_out"` && result != "timed_out" {
		t.Fatalf("result = %q, want timed_out", result)
	}
}
