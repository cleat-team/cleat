package engine

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// cleat#4160. A recorded event whose durable write failed used to leave the guest running. Measured with a mock database (retry window
// 1 ms), two durable calls: with the first event's write failing past the retry window both calls executed, two events were in memory,
// one was durably written (step 1 without step 0) and nothing suspended; with a permanent write error both executed and none was written.
// A worker lost before the segment-end write then replays the persisted prefix, reaches the hole and re-executes everything after it.
//
// Now a failed write (other than a lost fence) stops the session at its next fresh host call and suspends the run, and a quota refusal
// is answered by the event cap's continue-as-new instead.

// writeFailureSession builds a session over a mock database whose event writes fail `fails` times and then succeed.
func writeFailureSession(t *testing.T, fails int, failure error, extra ...EngineOption) (*execSession, *mockCaller, *atomic.Int64) {
	t.Helper()
	var persisted atomic.Int64
	execs := make([]mockExecResult, 0, fails+1)
	for i := 0; i < fails; i++ {
		execs = append(execs, mockExecResult{match: "INSERT INTO event_history", err: failure, consume: true})
	}
	execs = append(execs, mockExecResult{match: "INSERT INTO event_history", affected: 1,
		sideEffect: func(_ *[]mockRowsResult, _ *[]mockExecResult) { persisted.Add(1) }})
	db := newMockDBForPostgres(t, nil, execs)
	t.Cleanup(func() { db.Close() })
	caller := &mockCaller{}
	opts := append([]EngineOption{WithDB(db), WithWorkflowID("wf-4160"), WithFlushRetryWindow(time.Millisecond)}, extra...)
	return &execSession{
		engine:     NewEngine(nil, caller, opts...),
		workflowID: "wf-4160", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
	}, caller, &persisted
}

func TestAFailedEventWriteStopsTheGuestAtItsNextHostCall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fails int
	}{
		{"an outage that outlasts the retry window", 3},
		{"a permanent write error", 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, caller, _ := writeFailureSession(t, tc.fails, errors.New("connection refused"))
			// The first call already ran when its event failed to write: it returns what it did.
			if got := s.DurableCall(hostCtx(), nil, "work", "charge", `{}`, 0, 256); got == callSuspendSentinel {
				t.Fatal("the call whose event failed to write was itself refused; its effect had already happened")
			}
			if s.eventWriteFailure == nil {
				t.Fatal("the failed write was not noted on the session")
			}
			var sus *SuspendError
			if !errors.As(s.suspendErr, &sus) || time.Until(sus.Until) <= 0 {
				t.Fatalf("the run was not suspended into the future: %v", s.suspendErr)
			}
			// The NEXT fresh call is refused with the stop and does not execute.
			if got := s.DurableCall(hostCtx(), nil, "work", "refund", `{}`, 0, 256); got != callSuspendSentinel {
				t.Fatalf("the call after the failed write returned %#x, want the stop", got)
			}
			if len(caller.calls) != 1 {
				t.Fatalf("%d calls executed, want 1: the guest went on past an unpersisted step", len(caller.calls))
			}
		})
	}
}

// CONTROL: a healthy database stops nothing, and both calls run.
func TestAHealthyEventWriteStopsNothing(t *testing.T) {
	s, caller, persisted := writeFailureSession(t, 0, nil)
	for _, op := range []string{"charge", "refund"} {
		if got := s.DurableCall(hostCtx(), nil, "work", op, `{}`, 0, 256); got == callSuspendSentinel {
			t.Fatalf("%s was refused with a healthy database", op)
		}
	}
	if s.eventWriteFailure != nil || s.suspendErr != nil || len(caller.calls) != 2 || persisted.Load() != 2 {
		t.Fatalf("failure=%v suspendErr=%v calls=%d persisted=%d; want none, none, 2, 2", s.eventWriteFailure, s.suspendErr, len(caller.calls), persisted.Load())
	}
}

// A lost fence keeps its existing handling: another worker owns the run, so this session is not stopped by it here.
func TestALostFenceIsNotAFailedEventWrite(t *testing.T) {
	s, _, _ := writeFailureSession(t, 1000, ErrFenceLost)
	s.DurableCall(hostCtx(), nil, "work", "charge", `{}`, 0, 256)
	if s.eventWriteFailure != nil || s.suspendErr != nil {
		t.Fatalf("a lost fence was handled as a failed write: failure=%v suspendErr=%v", s.eventWriteFailure, s.suspendErr)
	}
}

// A quota refusal is permanent: it is not retried, and the answer is the event cap's continue-as-new, not a suspend the quota would refuse again.
func TestAQuotaRefusedEventWriteContinuesAsNewAndIsNotRetried(t *testing.T) {
	var attempts atomic.Int64
	db := newMockDBForPostgres(t, []mockRowsResult{{match: "event_count", data: [][]driverValue{{int64(100)}}}},
		[]mockExecResult{{match: "INSERT INTO event_history", affected: 1, sideEffect: func(_ *[]mockRowsResult, _ *[]mockExecResult) { attempts.Add(1) }}})
	defer db.Close()
	caller := &mockCaller{}
	s := &execSession{
		engine: NewEngine(nil, caller, WithDB(db), WithWorkflowID("wf-4160"), WithMaxQuotaEvents(50), WithWorkflowStore(&stubWorkflowStore{}),
			WithFlushRetryWindow(time.Second)),
		workflowID: "wf-4160", nowMs: time.Now().UnixMilli(), originalInput: `{}`,
		deferrals: map[string]string{}, queryState: map[string]string{},
	}
	began := time.Now()
	s.DurableCall(hostCtx(), nil, "work", "charge", `{}`, 0, 256)
	if time.Since(began) > 500*time.Millisecond {
		t.Fatalf("a quota refusal took %v: it was retried as if transient", time.Since(began))
	}
	if !errors.Is(s.eventWriteFailure, ErrEventQuotaExceeded) || !s.autoContinueAsNewTriggered {
		t.Fatalf("failure=%v triggered=%v; want the quota refusal and a continue-as-new", s.eventWriteFailure, s.autoContinueAsNewTriggered)
	}
	// Asserted non-nil BEFORE it is read: a continuation the session refused itself left this nil (#4161's refusal at the top of
	// ContinueAsNew read the failure flag), and the run carried on.
	if s.suspendErr == nil {
		t.Fatalf("suspendErr is nil: the continue-as-new that is the remedy for a refused quota was refused or never ran")
	}
	var sus *SuspendError
	if !errors.As(s.suspendErr, &sus) || sus.Reason != "continue_as_new" {
		t.Fatalf("suspendErr = %v, want the continue-as-new", s.suspendErr)
	}
	// and the run is still stopped for new work afterwards: the flag is set once the continuation has been decided.
	if !s.stopBeforeNewWork() {
		t.Fatalf("stopBeforeNewWork() = false after a refused quota: further calls would go on top of the hole")
	}
}
