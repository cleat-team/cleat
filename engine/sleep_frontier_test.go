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
	// Simulate the sleep having elapsed. The decision is now a function of real
	// time, so a test that wants to exercise resume must say the time passed --
	// otherwise it would have to sleep for 60 seconds, and a test that waits on
	// a wall clock is the kind of timing dependency CLAUDE.md says to remove
	// rather than widen.
	wakeAt := history[0].TimestampMs + 60_000 + 1
	caller2 := &mockCaller{}
	eng2 := NewEngine(rt, caller2, WithWorkflowID("wf-sleep-frontier"),
		WithClock(func() int64 { return wakeAt }))
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

// TestConsecutiveSleepsEachWaitTheirOwnTurn is the property the accumulator
// exists for, and the one the previous fix did not have.
//
// Now() reads history[stepCount-1] while stepCount is within history, and
// sleeps do not advance stepCount. So two sleeps in a row read the SAME anchor
// unless the deadline carries forward. Without max(), the second sleep would
// compute the first sleep's deadline, find it already passed, and complete a
// wait it never performed -- the same "completing on someone else's evidence"
// failure as the bug being replaced, one step further along.
func TestConsecutiveSleepsEachWaitTheirOwnTurn(t *testing.T) {
	ctx := context.Background()
	anchor := int64(1_000_000)

	newSession := func(now int64) *execSession {
		s := newTestExecSession()
		s.history = []EventRecord{{Step: 0, EventType: EventTypeCall, TimestampMs: anchor}}
		s.stepCount = 1
		s.nowMs = anchor
		s.engine.nowFn = func() int64 { return now }
		return s
	}

	// One hour has passed: the first sleep is served, the second is not.
	s := newSession(anchor + 3_600_000)
	if status := byte(s.DurableSleep(ctx, nil, 3_600_000) >> 56); status != sleepStatusCompleted {
		t.Errorf("sleep 1 status %d, want completed(%d)", status, sleepStatusCompleted)
	}
	if status := byte(s.DurableSleep(ctx, nil, 3_600_000) >> 56); status != sleepStatusSuspend {
		t.Errorf("sleep 2 status %d, want suspend(%d).\n\n"+
			"Only one hour has elapsed and the workflow asked for two. Completing "+
			"here discards an hour the workflow was told it would wait.",
			status, sleepStatusSuspend)
	}
	if s.suspendErr == nil {
		t.Fatal("sleep 2 did not suspend")
	}
	if got := s.suspendErr.Until.UnixMilli(); got != anchor+7_200_000 {
		t.Errorf("sleep 2 suspends until %d, want %d -- the deadline must accumulate "+
			"across both sleeps", got, anchor+7_200_000)
	}

	// Two hours have passed: both are served and the workflow runs on.
	s2 := newSession(anchor + 7_200_000)
	if status := byte(s2.DurableSleep(ctx, nil, 3_600_000) >> 56); status != sleepStatusCompleted {
		t.Errorf("sleep 1 status %d, want completed", status)
	}
	if status := byte(s2.DurableSleep(ctx, nil, 3_600_000) >> 56); status != sleepStatusCompleted {
		t.Errorf("sleep 2 status %d, want completed after two hours", status)
	}
	if s2.suspendErr != nil {
		t.Errorf("workflow still suspended after both sleeps were served: %v", s2.suspendErr)
	}
	if s2.nowMs != anchor+7_200_000 {
		t.Errorf("virtual clock = %d, want %d", s2.nowMs, anchor+7_200_000)
	}
}

// TestInteriorSleepOnReplayCompletes covers the case that needs no special
// handling and must not acquire any.
//
// A sleep with a recorded event after it already finished: that event could
// only have been written once the sleep returned, so its timestamp is at or
// beyond the sleep's deadline, and real time is beyond that again. The rule
// therefore completes it without a "am I replaying?" branch.
func TestInteriorSleepOnReplayCompletes(t *testing.T) {
	ctx := context.Background()
	anchor := int64(1_000_000)

	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{
		{Step: 0, EventType: EventTypeCall, TimestampMs: anchor},
		{Step: 1, EventType: EventTypeCall, TimestampMs: anchor + 3_600_000},
	}
	s.stepCount = 1 // first call replayed; the sleep is next, with an event still ahead
	s.nowMs = anchor
	s.engine.nowFn = func() int64 { return anchor + 7_200_000 }

	if status := byte(s.DurableSleep(ctx, nil, 3_600_000) >> 56); status != sleepStatusCompleted {
		t.Errorf("an interior sleep returned status %d, want completed(%d).\n\n"+
			"There is a recorded event after this sleep, so the workflow demonstrably "+
			"got past it. Suspending here would re-run history that already happened.",
			status, sleepStatusCompleted)
	}
	if s.suspendErr != nil {
		t.Errorf("interior sleep suspended: %v", s.suspendErr)
	}
}
