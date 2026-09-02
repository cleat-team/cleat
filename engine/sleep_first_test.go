//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// A workflow whose FIRST durable operation is a sleep never resumed, even
// after sleep started deciding from elapsed time.
//
// The rule anchors a sleep's deadline to the last recorded event. A workflow
// that sleeps first has no such event, so the anchor fell back to the process
// wall clock -- which is re-read on every segment, so the deadline moved
// forward with it and never arrived. The workflow woke on schedule,
// re-executed, and re-suspended, with next_wake_at re-armed in the past each
// time.
//
// This is not an edge case. "Wait five minutes, then do the thing" is an
// ordinary delayed job, and it is the shape most likely to be written as a
// sleep before anything else.
//
// The fix hands the engine the workflow row's created_at
// (WithWorkflowStartTime), which is fixed for the life of the run, so the
// deadline stops moving. IMPROVEMENT-PLAN 3.67.

// sleepFirstWat sleeps before doing anything else, then makes one durable
// call. The call is the proof of progress: it can only run if the sleep
// completed.
const sleepFirstWat = `(module
  (import "env" "cleat_call" (func $call (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)))
  (import "env" "cleat_sleep" (func $sleep (param i64) (result i64)))
  (memory (export "memory") 1)
  (data (i32.const 1024) "stepA")
  (data (i32.const 1152) "op")
  (data (i32.const 1216) "{}")
  (func (export "run") (param i32 i32 i32 i32) (result i64)
    (local $slept i64)
    (local.set $slept (call $sleep (i64.const 300000)))
    (if (i64.eq (i64.shr_u (local.get $slept) (i64.const 56)) (i64.const 1))
      (then unreachable))
    (drop (call $call
      (i32.const 1024) (i32.const 5)
      (i32.const 1152) (i32.const 2)
      (i32.const 1216) (i32.const 2)
      (i32.const 4096) (i32.const 256)))
    (i64.const 0))
)`

// TestSleepFirstWorkflowResumes drives the workflow the way a worker would:
// each segment gets the previous segment's history back, and the clock
// advances between them.
func TestSleepFirstWorkflowResumes(t *testing.T) {
	ctx := context.Background()
	wasmBytes := mustWat2Wasm(t, sleepFirstWat)

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	const createdAt = int64(1_700_000_000_000)
	const fiveMinutes = int64(300_000)

	// Segment 1 runs at the moment the workflow was created: nothing has
	// elapsed, so the sleep must suspend.
	caller1 := &mockCaller{}
	eng1 := NewEngine(rt, caller1,
		WithWorkflowID("wf-sleep-first"),
		WithWorkflowStartTime(createdAt),
		WithClock(func() int64 { return createdAt }))
	_, history, suspended, _, _, err := eng1.Execute(ctx, wasmBytes, "run", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	if suspended == nil {
		t.Fatal("segment 1 did not suspend at the sleep")
	}
	if got := suspended.SuspendUntil.UnixMilli(); got != createdAt+fiveMinutes {
		t.Fatalf("segment 1 suspends until %d, want %d (created_at plus the "+
			"requested five minutes)", got, createdAt+fiveMinutes)
	}
	if len(history) != 0 {
		t.Fatalf("segment 1 recorded %d events; a sleep records none, so the "+
			"history feeding segment 2 must be empty -- which is the whole "+
			"difficulty: %#v", len(history), history)
	}
	if len(servicesCalled(caller1)) != 0 {
		t.Fatalf("segment 1 ran the call before the sleep completed: %v", servicesCalled(caller1))
	}

	// Segment 2: five minutes later. The history is still empty, so the anchor
	// can only come from created_at.
	caller2 := &mockCaller{}
	eng2 := NewEngine(rt, caller2,
		WithWorkflowID("wf-sleep-first"),
		WithWorkflowStartTime(createdAt),
		WithClock(func() int64 { return createdAt + fiveMinutes }))
	_, history2, suspended2, _, _, err := eng2.Replay(ctx, wasmBytes, "run", json.RawMessage(`{}`), history)
	if err != nil {
		t.Fatalf("segment 2: %v", err)
	}

	if got := servicesCalled(caller2); len(got) != 1 || got[0] != "stepA" {
		t.Errorf("segment 2 called %v, want exactly [stepA].\n\n"+
			"The five minutes the workflow asked for have passed. If the call did "+
			"not run, the sleep re-suspended on an anchor that moved forward with "+
			"the wall clock, and the workflow is in the re-claim loop 3.67 describes.", got)
	}
	if suspended2 != nil {
		t.Errorf("segment 2 suspended again until %v; the sleep has elapsed",
			suspended2.SuspendUntil)
	}
	if len(history2) == 0 {
		t.Error("segment 2 recorded no events, so it made no progress")
	}
}

// TestSleepFirstStillWaitsWhenResumedEarly is the control.
//
// "The sleep resumes" must not become "the sleep never waits". A workflow
// handed back before its deadline -- by a reaper, a manual retry, or a NOTIFY
// -- has to suspend for the remainder, and on the original deadline rather
// than a fresh five minutes from now, or every early wake would extend the
// wait.
func TestSleepFirstStillWaitsWhenResumedEarly(t *testing.T) {
	ctx := context.Background()
	wasmBytes := mustWat2Wasm(t, sleepFirstWat)

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	const createdAt = int64(1_700_000_000_000)
	const fiveMinutes = int64(300_000)

	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithWorkflowID("wf-sleep-first-early"),
		WithWorkflowStartTime(createdAt),
		// Only one minute of the five has passed.
		WithClock(func() int64 { return createdAt + 60_000 }))
	_, _, suspended, _, _, err := eng.Replay(ctx, wasmBytes, "run", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if suspended == nil {
		t.Fatal("a workflow resumed one minute into a five-minute sleep must " +
			"suspend again")
	}
	if got := suspended.SuspendUntil.UnixMilli(); got != createdAt+fiveMinutes {
		t.Errorf("suspends until %d, want %d -- the original deadline, not a "+
			"fresh five minutes from the early wake", got, createdAt+fiveMinutes)
	}
	if got := servicesCalled(caller); len(got) != 0 {
		t.Errorf("ran %v before the sleep had elapsed", got)
	}
}

// TestSeedNowMsPrefersHistoryOverStartTime pins the precedence.
//
// created_at anchors only an empty history. Once the workflow has recorded
// anything, the first event's timestamp is the more accurate anchor and must
// win -- otherwise a long-running workflow would measure every sleep from the
// moment it was created, and deadlines minutes or days old would all read as
// already elapsed.
func TestSeedNowMsPrefersHistoryOverStartTime(t *testing.T) {
	e := NewEngine(nil, nil, WithWorkflowStartTime(1_000))
	hist := []EventRecord{{Step: 0, EventType: EventTypeCall, TimestampMs: 9_000}}

	if got := e.seedNowMs(hist); got != 9_000 {
		t.Errorf("with history, seed = %d, want the first event's timestamp 9000", got)
	}
	if got := e.seedNowMs(nil); got != 1_000 {
		t.Errorf("without history, seed = %d, want created_at 1000", got)
	}

	// No start time and no history: fall back to the process clock rather than
	// to zero, which would put every sleep deadline at the epoch and complete
	// them all instantly.
	nowMs.Store(4_242)
	e2 := NewEngine(nil, nil)
	if got := e2.seedNowMs(nil); got != 4_242 {
		t.Errorf("with neither, seed = %d, want the process clock 4242", got)
	}
}
