package engine

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// cleat#4210, a gap in cleat#4160's fix. A retried call (DurableCallWithRetry) makes SEVERAL service attempts inside ONE host call, and records
// each failed attempt as its own event. #4160 stopped the run at its next host call once an event write was refused, but the per-attempt loop
// never looked: the refused write of the first failed attempt left the loop to make the next attempt, perform the call for real, and report success
// to the guest, with none of it in event_history. A reclaim then performed every one of those attempts again.
//
// Found by the fault-injection harness (cleat#4169): scenario `retry`, fault `dbwrite` at the call's first and second attempt, both on every run.

func retryOverRefusedWrites(t *testing.T, refuse bool) (*execSession, *scriptedCaller, int64) {
	t.Helper()
	var execs []mockExecResult
	if refuse {
		// Every write fails, past the 1 ms flush window: the outage that outlasts the retry window (the harness's 5 s hold, in miniature).
		for i := 0; i < 1000; i++ {
			execs = append(execs, mockExecResult{match: "INSERT INTO event_history", err: errors.New("permission denied for table event_history"), consume: true})
		}
	}
	var persisted atomic.Int64
	execs = append(execs, mockExecResult{match: "INSERT INTO event_history", affected: 1,
		sideEffect: func(_ *[]mockRowsResult, _ *[]mockExecResult) { persisted.Add(1) }})
	db := newMockDBForPostgres(t, nil, execs)
	t.Cleanup(func() { db.Close() })
	caller := &scriptedCaller{failFirst: 1}
	s := &execSession{
		engine:     NewEngine(nil, caller, WithDB(db), WithWorkflowID("wf-4210"), WithFlushRetryWindow(time.Millisecond)),
		workflowID: "wf-4210", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
	}
	// Three attempts allowed, the first one fails, 1 ms backoff: with a healthy store the second attempt succeeds.
	got := s.DurableCallWithRetry(hostCtx(), nil, "svc", "op", `{}`, 3, 1, 100, 1, `[]`, 0, 4096)
	return s, caller, got
}

// CONTROL: with a healthy store the first attempt fails and the second succeeds. Without it the test below could pass by a retry that never ran.
func TestARetriedCallWithAHealthyStoreMakesItsSecondAttempt(t *testing.T) {
	s, caller, got := retryOverRefusedWrites(t, false)
	if caller.attempts != 2 || !callSucceeded(got) {
		t.Fatalf("control: attempts=%d succeeded=%v, want 2 attempts and a success", caller.attempts, callSucceeded(got))
	}
	if s.eventWriteFailure != nil {
		t.Fatalf("control: a healthy store noted a write failure: %v", s.eventWriteFailure)
	}
}

func TestARetriedCallStopsAfterItsFailedAttemptEventWasRefused(t *testing.T) {
	s, caller, got := retryOverRefusedWrites(t, true)
	if s.eventWriteFailure == nil {
		t.Fatal("the refused write was not noted on the session, so this test does not reach the case")
	}
	if caller.attempts != 1 {
		t.Fatalf("%d attempts were made, want 1: the loop went on to perform an attempt on top of an event it could not write", caller.attempts)
	}
	if got != callSuspendSentinel {
		t.Fatalf("the call returned %#x, want the stop: the guest must not be told the call succeeded or failed", got)
	}
}
