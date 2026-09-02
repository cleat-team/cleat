//go:build cgo

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

// A defer registered before the workflow suspended never ran.
//
// DurableDefer has two halves on the fresh path: it records an EventTypeDefer
// event AND it adds the ID to session.deferrals, the live set that the
// terminal-transition code actually iterates. Its replay-match branch did only
// the first half by omission -- it advanced the step, wrote the recorded
// DeferID back to the guest, and returned, leaving the map untouched:
//
//	if rec.EventType == EventTypeDefer {
//	    if !s.advanceReplayStep(ctx, &rec) { return 0 }
//	    written, _ := s.writeResult(ctx, m, deferIDPtr, rec.DeferID, deferIDMaxLen)
//	    return packSimpleResult(0, written)   // <- s.deferrals never written
//	}
//
// Every segment after the first replays past the registration, so that branch
// is the ONLY one a previously-registered defer ever reaches again. The defer
// was therefore dropped permanently the moment the workflow suspended once.
//
// The blast radius is the case defer exists for. A defer that never suspends
// runs in the same segment that registered it and was unaffected; a defer on a
// workflow that sleeps, waits for a signal, or awaits a child -- the
// long-running workflow whose locks and saga steps are the reason destructors
// are worth having -- was silently lost. Nothing logged: session.deferrals was
// empty, and every call site is guarded by `if len(deferrals) > 0`.
//
// Measured 2026-09-01, replaying a one-event history through the session:
//
//	after replayed DurableDefer: stepCount=1 isReplay=true deferrals=map[string]string{}
//
// The pre-existing tests could not see it. TestDurableDeferReplayMatch asserts
// stepCount and isReplay and never looks at s.deferrals;
// TestDurableDeferReplayPastEnd does assert it, but only on the fallthrough
// where replay has already ended and the fresh path runs. The one branch with
// the bug was the one branch with no assertion on the map.
//
// IMPROVEMENT-PLAN 3.66, which is 3.35 finding 4.

// deferAcrossSuspensionWat carries both segments of one workflow.
//
// Both entry points share the same prefix -- register a defer, make one
// durable call -- which is what makes the second a faithful replay of the
// first: the recorded events match position for position. They differ only
// after that point, where segment 1 suspends and segment 2 terminates.
//
// Two entry points rather than one guest run twice, because a `cleat_sleep`
// that immediately follows the last recorded event cannot currently resume:
// sleep is local and completes only when `replayJustEnded` is set, which only
// `exitReplay` sets, and `DurableSleep` never checks whether the history is
// exhausted. Measured 2026-09-01 with a `call; sleep; unreachable` guest --
// segment 1 and segment 2 both returned `events=1 suspended=true`, an
// unchanged history. Running the same entry point twice here would therefore
// test the sleep defect rather than this one. That defect is filed separately;
// it is not this test's subject, and depending on it would make this test go
// red when it is fixed.
//
// The trap in segment 2, and the trapping defer body, are the technique from
// defer_runs_once_test.go: a defer that merely returned would log nothing, so
// the trap is what makes entry into the body observable without a host handler
// or an output-ABI agreement.
const deferAcrossSuspensionWat = `(module
  (import "env" "cleat_defer" (func $defer (param i32 i32 i32 i32) (result i64)))
  (import "env" "cleat_call" (func $call (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)))
  (import "env" "cleat_sleep" (func $sleep (param i64) (result i64)))
  (memory (export "memory") 1)
  (data (i32.const 1024) "release the lock")
  (data (i32.const 1088) "svc")
  (data (i32.const 1152) "op")
  (data (i32.const 1216) "{}")
  (func $prefix
    (drop (call $defer (i32.const 1024) (i32.const 16) (i32.const 2048) (i32.const 64)))
    (drop (call $call
      (i32.const 1088) (i32.const 3)
      (i32.const 1152) (i32.const 2)
      (i32.const 1216) (i32.const 2)
      (i32.const 4096) (i32.const 256))))
  (func (export "before_the_suspension") (param i32 i32 i32 i32) (result i64)
    (call $prefix)
    (drop (call $sleep (i64.const 60000)))
    unreachable)
  (func (export "after_the_suspension") (param i32 i32 i32 i32) (result i64)
    (call $prefix)
    unreachable)
  (func (export "after_the_suspension_ok") (param i32 i32 i32 i32) (result i64)
    (call $prefix)
    (i64.const 0))
  (func (export "cleat_defer_defer-0") (param i32 i32 i32 i32) (result i64) unreachable)
)`

func newDeferSuspensionEngine(t *testing.T, logs *bytes.Buffer, opts ...EngineOption) *Engine {
	t.Helper()
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })

	opts = append([]EngineOption{
		WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		WithWorkflowID("wf-defer-suspension"),
	}, opts...)
	return NewEngine(rt, &mockCaller{}, opts...)
}

// TestDeferRegisteredBeforeASuspensionSurvivesIt is the regression test, run
// against a history the engine produced itself rather than one written by hand.
//
// Segment 1 establishes that registration works at all -- without it a green
// result here would also be produced by a defer that was never registered in
// the first place, which is the more likely way to break this and would look
// identical at segment 2.
func TestDeferRegisteredBeforeASuspensionSurvivesIt(t *testing.T) {
	ctx := context.Background()
	wasmBytes := mustWat2Wasm(t, deferAcrossSuspensionWat)

	var logs bytes.Buffer
	eng := newDeferSuspensionEngine(t, &logs)

	// Segment 1: runs fresh, registers the defer, suspends at the sleep.
	_, history, suspended, deferrals, _, err := eng.Execute(ctx, wasmBytes, "before_the_suspension", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	if suspended == nil {
		t.Fatal("segment 1 did not suspend, so the defer is not behind a replay " +
			"boundary and segment 2 would exercise the fresh path instead")
	}
	if _, ok := deferrals["defer-0"]; !ok {
		t.Fatalf("segment 1 did not register the defer: %#v", deferrals)
	}
	if len(history) != 2 {
		t.Fatalf("segment 1 recorded %d events, want the defer and the call: %#v", len(history), history)
	}
	if history[0].EventType != EventTypeDefer {
		t.Fatalf("segment 1's first event is %q, not a defer registration", history[0].EventType)
	}

	// Segment 2: replays that history. The defer registration is consumed by
	// the replay-match branch, which is the code under test.
	//
	// The cleanly-terminating entry point, not the trapping one: on a failure
	// executeCompiled returns a nil deferral map by design -- it has already
	// run them itself -- so a trapping segment cannot answer the question this
	// test asks. TestDeferRegisteredBeforeASuspensionActuallyRuns covers that
	// path instead, by watching the body rather than the map.
	eng2 := newDeferSuspensionEngine(t, &logs)
	_, _, _, deferrals2, _, err := eng2.Replay(ctx, wasmBytes, "after_the_suspension_ok", json.RawMessage(`{}`), history)
	if err != nil {
		t.Fatalf("segment 2: %v", err)
	}

	if _, ok := deferrals2["defer-0"]; !ok {
		t.Errorf("the defer registered in segment 1 is gone after replay: %#v\n\n"+
			"Replay re-runs the workflow body and re-registers its defers -- that is "+
			"what makes a defer a replayed continuation rather than a resurrected "+
			"closure. DurableDefer's replay branch must write s.deferrals, not just "+
			"answer the guest.", deferrals2)
	}
	if got := deferrals2["defer-0"]; got != "release the lock" {
		t.Errorf("defer description after replay = %q, want %q -- the recorded "+
			"description must survive too, it is what the failure log names", got, "release the lock")
	}
}

// TestDeferRegisteredBeforeASuspensionActuallyRuns closes the loop the test
// above stops short of: the map is the engine's output, but a defer nobody
// invokes is still not a destructor.
//
// It asserts the body was entered, via the trap it raises.
func TestDeferRegisteredBeforeASuspensionActuallyRuns(t *testing.T) {
	ctx := context.Background()
	wasmBytes := mustWat2Wasm(t, deferAcrossSuspensionWat)

	var seg1 bytes.Buffer
	eng := newDeferSuspensionEngine(t, &seg1)
	_, history, suspended, _, _, err := eng.Execute(ctx, wasmBytes, "before_the_suspension", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	if suspended == nil {
		t.Fatal("segment 1 did not suspend")
	}

	var logs bytes.Buffer
	eng2 := newDeferSuspensionEngine(t, &logs)
	if len(eng2.backends) != 0 {
		t.Fatalf("expected an engine with no backends, got %d -- this test reads the "+
			"executeCompiled branch's logs", len(eng2.backends))
	}
	_, _, _, _, _, replayErr := eng2.Replay(ctx, wasmBytes, "after_the_suspension", json.RawMessage(`{}`), history)
	if replayErr == nil {
		t.Fatal("segment 2's guest traps after the replayed prefix; Replay must report that.\n\n" +
			"Without a terminal transition no defers run and the count below is zero " +
			"whether or not the fix is present.")
	}

	ran := countByDeferID(deferLogRE, logs.String())
	notFound := countByDeferID(deferNotFoundRE, logs.String())
	if ran["defer-0"]+notFound["defer-0"] == 0 {
		t.Errorf("the defer registered in segment 1 was never invoked after the "+
			"workflow terminated in segment 2.\nlogs:\n%s", logs.String())
	}
	if ran["defer-0"] > 1 {
		t.Errorf("defer-0 body ran %d times, want 1 (see defer_runs_once_test.go)", ran["defer-0"])
	}
}

// TestDurableDeferReplayReconstructsAMissingDeferID pins the fallback.
//
// A history written before DeferID was recorded replays with rec.DeferID
// empty. Keying the map on that would register "" -- export name
// "cleat_defer_" -- so the ID is recomputed as the fresh path would have
// minted it at this step, which is the ID the guest was originally handed.
func TestDurableDeferReplayReconstructsAMissingDeferID(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{
		{Step: 0, EventType: EventTypeSleep},
		{Step: 1, EventType: EventTypeDefer, DeferDescription: "cleanup"}, // no DeferID
	}
	s.stepCount = 1

	s.DurableDefer(context.Background(), nil, "cleanup", 0, 0)

	if _, ok := s.deferrals["defer-1"]; !ok {
		t.Errorf("expected the ID to be reconstructed as defer-1 from the step it "+
			"was recorded at, got %#v", s.deferrals)
	}
	if _, ok := s.deferrals[""]; ok {
		t.Error("registered the empty defer ID, which resolves to the export name " +
			"\"cleat_defer_\"")
	}
}
