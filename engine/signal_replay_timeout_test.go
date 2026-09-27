package engine

import (
	"context"
	"testing"
)

// TestALateSignalIsNotDeliveredToAnAwaitThatTimedOut is cleat#947.
//
// History that continues past an `await_signals` with no `signal_received`
// after it means one thing: the original execution reached that next event
// without receiving a signal, so the await TIMED OUT. Replay has to reproduce
// that. The replay arm instead polled workflow_signals, and a signal that
// arrived after the timeout is exactly what it found.
//
// THE RULE IS ALREADY IN THE TREE, for the sibling mechanism.
// DurableAwaitUpdate's doc derives it from a property of recordEvent rather
// than anything about updates:
//
//	recordEvent APPENDS (`s.history = append(s.history, rec)`), so an event
//	can only ever land at the frontier. There is no way to insert a delivery
//	in the middle of an existing history
//
//	- replaying: return history[stepCount] if it is an update_received, else
//	  report nothing found. The table is NOT consulted -- a request that
//	  arrived later must not be delivered at an earlier step.
//
// and the line after that says "This is DurableAwaitSignals' shape, for the
// same reason." It was not. That is why this defect survived: the correct
// behaviour was specified, implemented next door, and documented as shared.
//
// FOUR THINGS WENT WRONG IN ONE CALL, and the test asserts all four, because
// three of them are silent and the fourth is only visible later:
//
//  1. the guest is handed a signal the original run never saw -- a replay
//     divergence, with nothing detecting it
//  2. the delivery is consumed, so a later await that legitimately wants it
//     cannot have it
//  3. the appended record's Step is s.stepCount while the append lands at
//     len(s.history), so Step COLLIDES with the event already there
//  4. s.stepCount ends one past the record replay should read next, so every
//     later host call is matched against the wrong history entry
//
// No database: the divergence is in the in-memory replay logic and is
// dialect-independent.
func TestALateSignalIsNotDeliveredToAnAwaitThatTimedOut(t *testing.T) {
	store := newMockSignalWorkflowStore()
	ctx := context.Background()
	if err := store.DeliverSignal(ctx, "", "a", `{"late":1}`); err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	s := newTestExecSession()
	s.engine.signalStore = store
	s.isReplay = true
	// The original run awaited, timed out, and went on to make a call. The
	// `call` at step 1 is the whole signal that the await did not receive
	// anything: had it, a signal_received would sit between the two.
	s.history = []EventRecord{
		{Step: 0, EventType: EventTypeAwaitSignals, SignalNames: `["a"]`, TimeoutMs: 1000},
		{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "r"},
	}

	buf := make([]byte, 512)
	packed := s.DurableAwaitSignals(contextWithRawMemBuf(ctx, buf), nil,
		`["a"]`, 1000, 0, 200, 256, 200)

	nameLen := uint32((packed >> 48) & 0xFFFFFFFF)
	timedOut := uint16((packed>>16)&0xFFFF) != 0

	// (1) The guest must see what the original run saw: a timeout.
	if got := string(buf[:nameLen]); got != "" {
		t.Errorf("the replayed await returned signal %q. The original execution did "+
			"not receive it -- history goes straight from the await to a %s -- so the "+
			"guest is now on a branch that run never took, and nothing detects it.",
			got, s.history[1].EventType)
	}
	if !timedOut {
		t.Errorf("the replayed await did not report a timeout, but the original " +
			"execution timed out here: history continues past the await with no " +
			"signal_received")
	}

	// (2) The delivery belongs to whatever await legitimately receives it next.
	if _, found, err := store.PollSignal(ctx, "", "a"); err != nil || !found {
		t.Errorf("the delivery was consumed by an await that timed out (found=%v err=%v). "+
			"It arrived after that await gave up, so it is still owed to a later one.",
			found, err)
	}

	// (3) and (4) are about history bookkeeping, and they are what makes this
	// worse than a wrong answer: they corrupt the replay of everything after.
	if len(s.history) != 2 {
		t.Errorf("replay appended to history (now %d entries: %v). Replay reproduces "+
			"recorded work; it must not record any.", len(s.history), eventTypesOf(s.history))
		for i, h := range s.history {
			if h.Step != i {
				t.Errorf("  history[%d] carries Step=%d. Step comes from s.stepCount while "+
					"the append lands at len(s.history), so the two disagree the moment "+
					"anything is appended mid-replay -- and this one collides with the "+
					"event already at step %d.", i, h.Step, h.Step)
			}
		}
	}
	if s.stepCount != 1 {
		t.Errorf("stepCount is %d after replaying one await, want 1 -- the next host call "+
			"must read history[1], the recorded %s. At %d it reads past it, and every "+
			"later call is matched against the wrong entry.",
			s.stepCount, s.history[1].EventType, s.stepCount)
	}
	if !s.isReplay {
		t.Errorf("replay ended at an await whose history CONTINUES; the remaining %d "+
			"event(s) would then be re-executed as fresh work rather than replayed",
			len(s.history)-s.stepCount)
	}
}

// TestAnExhaustedHistoryStillPollsTheStore is the positive control.
//
// Without it the test above passes against a DurableAwaitSignals that never
// polls at all -- which would break the ordinary case this whole path exists
// for: a workflow suspended on an await, woken because a signal arrived into
// workflow_signals after it stopped. The difference between the two is
// whether history CONTINUES past the await, and nothing else.
func TestAnExhaustedHistoryStillPollsTheStore(t *testing.T) {
	store := newMockSignalWorkflowStore()
	ctx := context.Background()
	if err := store.DeliverSignal(ctx, "", "a", `{"p":1}`); err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	s := newTestExecSession()
	s.engine.signalStore = store
	s.isReplay = true
	// Identical to the case above except that the await is the LAST event.
	s.history = []EventRecord{
		{Step: 0, EventType: EventTypeAwaitSignals, SignalNames: `["a"]`, TimeoutMs: 1000},
	}

	buf := make([]byte, 512)
	packed := s.DurableAwaitSignals(contextWithRawMemBuf(ctx, buf), nil,
		`["a"]`, 1000, 0, 200, 256, 200)

	nameLen := uint32((packed >> 48) & 0xFFFFFFFF)
	if got := string(buf[:nameLen]); got != "a" {
		t.Fatalf("a workflow suspended on an await, with the delivery waiting in the "+
			"store, got %q instead of \"a\". This is the ordinary resumption path and "+
			"the only way a signal reaches a suspended workflow.", got)
	}
	if s.isReplay {
		t.Errorf("the session is still replaying after consuming a delivery from the " +
			"store; that is new work, and recordEvent persists nothing while replaying " +
			"(cleat#933)")
	}
}
