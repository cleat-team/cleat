package cleattest

import (
	"testing"
	"time"
)

// TestAnUpdateRunsItsHandlerAndSettlesTheCallersPromise is the end-to-end
// property updates exist for: a caller reaches into a running workflow, the
// workflow's state changes, and the caller gets an answer back.
//
// Before updates were implemented end to end, none of that happened. Handlers
// were registered into a map that only a test harness ever read, the worker
// never configured an update handler on the engine, and no guest entry point
// existed -- so POST /update returned 202 with a promise_id and nothing ever
// settled it (IMPROVEMENT-PLAN 3.238, cleat#849).
func TestAnUpdateRunsItsHandlerAndSettlesTheCallersPromise(t *testing.T) {
	env := NewTestEnv()
	h := env.H()

	total := 0
	h.RegisterUpdateHandler("add",
		func(payload string) (string, error) {
			total += len(payload) // any state change; length keeps it payload-derived
			return `{"ok":true}`, nil
		},
		nil,
	)

	promiseID, err := h.CreatePromise("caller")
	if err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}
	env.EnqueueUpdate("add", "12345", promiseID)

	// Explicit dispatch here. That the SUSPENSION POINTS also dispatch is a
	// separate property with its own test below -- cleattest's DurableSleep
	// blocks until the mock clock is advanced, which would only obscure what
	// this test is about.
	h.DispatchUpdates()

	if total != 5 {
		t.Errorf("the handler did not change workflow state: total = %d, want 5", total)
	}

	done := env.CompletedUpdates()
	if len(done) != 1 {
		t.Fatalf("completed %d updates, want 1", len(done))
	}
	if done[0].Name != "add" || done[0].Result != `{"ok":true}` || done[0].Error != "" {
		t.Errorf("unexpected outcome: %+v", done[0])
	}

	got, timedOut, err := h.AwaitPromise(promiseID, time.Second)
	if err != nil {
		t.Fatalf("AwaitPromise: %v", err)
	}
	if timedOut {
		t.Fatal("the caller's promise never settled -- this is the defect updates were built to fix")
	}
	if got != `{"ok":true}` {
		t.Errorf("the caller got %q, want %q", got, `{"ok":true}`)
	}
}

// TestAValidatorRefusalNeverReachesTheHandler: the validator is the half of the
// API that makes an update different from a signal, and it is read-only by
// design -- a bad request is refused before any state changes.
func TestAValidatorRefusalNeverReachesTheHandler(t *testing.T) {
	env := NewTestEnv()
	h := env.H()

	ran := false
	h.RegisterUpdateHandler("approve",
		func(payload string) (string, error) { ran = true; return "approved", nil },
		func(payload string) error { return errValidator },
	)

	promiseID, _ := h.CreatePromise("caller")
	env.EnqueueUpdate("approve", `{"amount":-1}`, promiseID)
	h.DispatchUpdates()

	if ran {
		t.Error("the handler ran despite the validator refusing")
	}
	done := env.CompletedUpdates()
	if len(done) != 1 {
		t.Fatalf("completed %d updates, want 1 -- a refused update must still answer the caller", len(done))
	}
	if done[0].Error == "" {
		t.Error("a refused update was completed with no error, so the caller is told it succeeded")
	}

	_, timedOut, err := h.AwaitPromise(promiseID, time.Second)
	if timedOut {
		t.Fatal("a refused update left the caller's promise pending")
	}
	if err == nil {
		t.Error("the caller's promise resolved rather than rejecting")
	}
}

// TestAnUnregisteredUpdateAnswersRatherThanHanging. The name is chosen by the
// caller, so it can name a handler this workflow does not have. That must be an
// answer, not silence -- the whole point of the feature is that a caller stops
// waiting.
func TestAnUnregisteredUpdateAnswersRatherThanHanging(t *testing.T) {
	env := NewTestEnv()
	h := env.H()

	promiseID, _ := h.CreatePromise("caller")
	env.EnqueueUpdate("no-such-handler", "{}", promiseID)
	h.DispatchUpdates()

	done := env.CompletedUpdates()
	if len(done) != 1 || done[0].Error == "" {
		t.Fatalf("an update naming no registered handler must be completed with an error, got %+v", done)
	}
	if _, timedOut, err := h.AwaitPromise(promiseID, time.Second); timedOut || err == nil {
		t.Error("the caller's promise did not reject")
	}
}

// TestUpdatesAreNotDispatchedWithoutADispatchPoint is the negative control for
// the whole design. Dispatch happens at fixed program positions, not on
// arrival; a workflow that never reaches one never services updates. If this
// ever fails, dispatch has become time-driven or host-driven, and the
// interleaving replay depends on is no longer a property of the program.
func TestUpdatesAreNotDispatchedWithoutADispatchPoint(t *testing.T) {
	env := NewTestEnv()
	h := env.H()

	ran := false
	h.RegisterUpdateHandler("add",
		func(payload string) (string, error) { ran = true; return "", nil }, nil)
	env.EnqueueUpdate("add", "x", "")

	// No suspension, no explicit DispatchUpdates.
	if ran {
		t.Error("an update ran without the workflow reaching a dispatch point")
	}
	if n := len(env.CompletedUpdates()); n != 0 {
		t.Errorf("completed %d updates before any dispatch point", n)
	}

	// The explicit call is the other half of the contract.
	h.DispatchUpdates()
	if !ran {
		t.Error("DispatchUpdates did not dispatch")
	}
}

type validatorError struct{}

func (validatorError) Error() string { return "amount must be positive" }

var errValidator = validatorError{}

// TestEachSuspensionPointIsADispatchPoint asserts the wiring the design rests
// on: a workflow that never calls DispatchUpdates itself still services
// updates, because the SDK dispatches before each suspension.
//
// This is the property that makes updates usable at all. Without it an update
// is only handled by a workflow whose author remembered to ask, and a workflow
// sleeping for an hour never handles one.
//
// Each case invokes its suspension point in a way that returns promptly, since
// what is under test is that the dispatch happens BEFORE the suspension, not
// the suspension itself.
func TestEachSuspensionPointIsADispatchPoint(t *testing.T) {
	cases := []struct {
		name    string
		suspend func(t *testing.T, env *TestEnv)
	}{
		{
			name: "DurableSleep",
			suspend: func(t *testing.T, env *TestEnv) {
				// cleattest's sleep blocks until the mock clock moves, so the
				// sleep runs on its own goroutine and the test advances time.
				// Same idiom as TestSendSignalAndWaitTimeout.
				done := make(chan struct{})
				go func() { defer close(done); env.H().DurableSleep(10 * time.Millisecond) }()
				time.Sleep(20 * time.Millisecond)
				env.AdvanceTime(20 * time.Millisecond)
				<-done
			},
		},
		{
			name: "AwaitPromise",
			suspend: func(t *testing.T, env *TestEnv) {
				// An already-settled promise returns without waiting.
				id, err := env.H().CreatePromise("settled")
				if err != nil {
					t.Fatalf("CreatePromise: %v", err)
				}
				env.ResolvePromise(id, "done")
				if _, _, err := env.H().AwaitPromise(id, time.Second); err != nil {
					t.Fatalf("AwaitPromise: %v", err)
				}
			},
		},
		{
			name: "AwaitSignals",
			suspend: func(t *testing.T, env *TestEnv) {
				env.Signal("go", "{}")
				if sig := env.H().AwaitSignals([]string{"go"}, time.Second); sig.TimedOut {
					t.Fatal("the signal was not delivered")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewTestEnv()
			h := env.H()

			ran := false
			h.RegisterUpdateHandler("ping",
				func(payload string) (string, error) { ran = true; return "pong", nil }, nil)
			env.EnqueueUpdate("ping", "{}", "")

			tc.suspend(t, env)

			if !ran {
				t.Errorf("%s did not dispatch pending updates. Every suspension point must, "+
					"or a workflow that suspends without asking for updates never services "+
					"them -- see HostCallsImpl.DispatchUpdates.", tc.name)
			}
		})
	}
}
