//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// A cleat_sleep at the replay frontier never resumed.
//
// DurableSleep records no event -- "sleep is local" -- and completed only when
// s.replayJustEnded was set. That flag has exactly one writer, exitReplay,
// which every OTHER durable call invokes when it runs past the end of history.
// DurableSleep never made that check itself, so it could resume only if some
// other call crossed the frontier first, in the same segment.
//
// In a faithful replay nothing does. Every operation before the sleep was
// recorded in the previous segment and replay-matches, so the sleep is always
// the operation that reaches the end of history. The workflow woke on time,
// re-executed to the same point, and suspended again with a byte-identical
// history.
//
// Measured 2026-09-01 with a real compiled Go SDK guest -- DurableCall,
// DurableSleep(10s), DurableCall -- through the wasmtime backend, across
// Execute plus three Replay segments:
//
//	segment 1: events=1 suspended=true  SuspendUntil=20:51:09.871  calls=[stepA.First]
//	segment 2: events=1 suspended=true  SuspendUntil=20:51:09.871  calls=[]
//	segment 3: events=1 suspended=true  SuspendUntil=20:51:09.871  calls=[]
//	segment 4: events=1 suspended=true  SuspendUntil=20:51:09.871  calls=[]
//
// The second DurableCall never ran and the result was "" every time. Against a
// real Postgres store the same workflow was re-claimed in ~2.2ms per segment
// after the first legitimate wake, because SuspendUntil was recomputed to the
// same already-elapsed instant and next_wake_at was re-armed in the past --
// a hot re-claim loop rather than a quiet stall.
//
// This is a regression with a date: commit 0a02a84 (2026-05-08) made sleep
// local, deleting EventTypeSleep from the codec and deleting DurableSleep's
// own frontier check, replacing it with a flag only other calls set.
//
// IMPROVEMENT-PLAN 3.67.

// sleepBetweenTwoCallsWat is the shape the real guest fixture has: a durable
// call, a sleep, and a second durable call whose execution is the proof that
// the workflow got past the sleep.
//
// The second call is what makes this a regression test rather than a
// suspension test. Asserting only "segment 2 did not suspend" would also pass
// for a sleep that had stopped suspending entirely, which is the most likely
// way to break the fix -- see TestFreshSleepStillSuspends.
//
// The guest branches on the sleep's status byte (bits 56-63 of the packed
// result, see packSleepResult) and traps when it reads suspend, because that
// is what a real guest does: the Go SDK panics cleat.ErrSuspend and the export
// wrapper unwinds. A guest that ignored the status and returned normally would
// report success to the engine, the suspension would not be honoured, and
// segment 1 would never establish a replay boundary at all.
const sleepBetweenTwoCallsWat = `(module
  (import "env" "cleat_call" (func $call (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)))
  (import "env" "cleat_sleep" (func $sleep (param i64) (result i64)))
  (memory (export "memory") 1)
  (data (i32.const 1024) "stepA")
  (data (i32.const 1088) "stepB")
  (data (i32.const 1152) "op")
  (data (i32.const 1216) "{}")
  (func (export "run") (param i32 i32 i32 i32) (result i64)
    (local $slept i64)
    (drop (call $call
      (i32.const 1024) (i32.const 5)
      (i32.const 1152) (i32.const 2)
      (i32.const 1216) (i32.const 2)
      (i32.const 4096) (i32.const 256)))
    (local.set $slept (call $sleep (i64.const 60000)))
    (if (i64.eq (i64.shr_u (local.get $slept) (i64.const 56)) (i64.const 1))
      (then unreachable))
    (drop (call $call
      (i32.const 1088) (i32.const 5)
      (i32.const 1152) (i32.const 2)
      (i32.const 1216) (i32.const 2)
      (i32.const 8192) (i32.const 256)))
    (i64.const 0))
)`

func servicesCalled(c *mockCaller) []string {
	out := make([]string, 0, len(c.calls))
	for _, rec := range c.calls {
		out = append(out, rec.Service)
	}
	return out
}

// TestSleepAtTheReplayFrontierResumes is the regression test.
//
// Segment 1 is not scaffolding: it establishes that the sleep suspends in the
// first place, and it is the engine's own recorded history -- not one written
// by hand -- that segment 2 replays.
func TestSleepAtTheReplayFrontierResumes(t *testing.T) {
	ctx := context.Background()
	wasmBytes := mustWat2Wasm(t, sleepBetweenTwoCallsWat)

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller1 := &mockCaller{}
	eng1 := NewEngine(rt, caller1, WithWorkflowID("wf-sleep-frontier"))
	_, history, suspended, _, _, err := eng1.Execute(ctx, wasmBytes, "run", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	if suspended == nil {
		t.Fatal("segment 1 did not suspend at the sleep, so there is no replay " +
			"boundary and segment 2 would not exercise the frontier at all")
	}
	if got := servicesCalled(caller1); len(got) != 1 || got[0] != "stepA" {
		t.Fatalf("segment 1 called %v, want exactly [stepA]", got)
	}
	if len(history) != 1 {
		t.Fatalf("segment 1 recorded %d events, want 1 (sleep is local and records "+
			"nothing): %#v", len(history), history)
	}

	// Segment 2: the sleep has elapsed and the workflow is rescheduled. Replay
	// consumes the recorded call, then reaches the sleep with history
	// exhausted -- the frontier case.
	caller2 := &mockCaller{}
	eng2 := NewEngine(rt, caller2, WithWorkflowID("wf-sleep-frontier"))
	_, history2, suspended2, _, _, err := eng2.Replay(ctx, wasmBytes, "run", json.RawMessage(`{}`), history)
	if err != nil {
		t.Fatalf("segment 2: %v", err)
	}

	if got := servicesCalled(caller2); len(got) != 1 || got[0] != "stepB" {
		t.Errorf("segment 2 called %v, want exactly [stepB].\n\n"+
			"stepB is the operation after the sleep. If it did not run, the workflow "+
			"re-executed to the same suspend point and made no progress -- and since "+
			"SuspendUntil is recomputed to the same elapsed instant, the worker "+
			"re-claims it immediately and forever. stepA must NOT reappear either: "+
			"it is in the history and replay must not re-fire it.", got)
	}
	if suspended2 != nil {
		t.Errorf("segment 2 suspended again at %v; the sleep it is waiting on has "+
			"already elapsed", suspended2.SuspendUntil)
	}
	if len(history2) <= len(history) {
		t.Errorf("history did not grow: %d events before, %d after.\n\n"+
			"A byte-identical history across segments is the signature of this bug.",
			len(history), len(history2))
	}
}

// TestFreshSleepStillSuspends is the control, and it is doing real work.
//
// "The sleep resumes" is trivially satisfiable by a sleep that never suspends,
// which would turn every DurableSleep into a no-op and silently delete the
// feature. This pins the other half: on first execution the sleep must still
// stop the workflow.
func TestFreshSleepStillSuspends(t *testing.T) {
	ctx := context.Background()
	wasmBytes := mustWat2Wasm(t, sleepBetweenTwoCallsWat)

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	eng := NewEngine(rt, caller, WithWorkflowID("wf-sleep-fresh"))
	_, _, suspended, _, _, err := eng.Execute(ctx, wasmBytes, "run", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if suspended == nil {
		t.Fatal("a sleep on first execution must suspend the workflow")
	}
	if got := servicesCalled(caller); len(got) != 1 || got[0] != "stepA" {
		t.Errorf("first execution called %v, want [stepA]: the workflow ran past a "+
			"sleep it should have suspended at", got)
	}
}

// TestReplayedSleepDoesNotSkipALaterFreshSleep pins the interaction between
// the two completion paths.
//
// exitReplay arms replayJustEnded for "the next sleep". The frontier branch
// completes the sleep that armed it, so it must consume the flag -- otherwise
// a second, genuinely new sleep later in the same segment would complete
// without ever waiting, silently dropping a real delay.
func TestReplayedSleepDoesNotSkipALaterFreshSleep(t *testing.T) {
	ctx := context.Background()

	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{Step: 0, EventType: EventTypeCall}}
	s.stepCount = 1 // the recorded call has been replayed; the sleep is next

	first := s.DurableSleep(ctx, nil, 1000)
	if status := byte(first >> 56); status != sleepStatusCompleted {
		t.Fatalf("the sleep at the frontier returned status %d, want completed(%d)",
			status, sleepStatusCompleted)
	}
	if s.replayJustEnded {
		t.Error("the frontier branch left replayJustEnded set after consuming it")
	}

	second := s.DurableSleep(ctx, nil, 1000)
	if status := byte(second >> 56); status != sleepStatusSuspend {
		t.Errorf("a second, genuinely new sleep returned status %d, want suspend(%d).\n\n"+
			"It has never run before, so completing it discards a real delay the "+
			"workflow asked for.", status, sleepStatusSuspend)
	}
}
