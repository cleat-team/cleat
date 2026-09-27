package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// IMPROVEMENT-PLAN 3.24: an ambiguous outcome was classified `unknown`.
//
// When replay finds a call that was dispatched but whose response was never
// recorded, the engine cannot know whether the side effect happened -- whether
// the customer was charged. That is the one failure class that needs a human to
// go and look at the external service, and it was stored with
// error_code='unknown', the same value as every ordinary bug, so it could not
// be queried for.
//
// The condition was recorded only as an English sentence in the guest-visible
// error text. Every consumer that wanted to detect it did so by substring --
// tests/integrity/ambiguity_detection_test.go still contains
// `strings.Contains(replayResult, "[AMBIGUOUS]")`. Rewording the message would
// have silently disabled the detection.
//
// These tests assert the structured channel: a failure that came from an
// unresolved pending intent carries ErrAmbiguous, reachable with errors.As,
// which is what cmd/cleat-worker/setup.go turns into the stored error_code.

// replayWithPendingIntentAtStep runs the `basic` fixture to completion, marks
// one step of the resulting history as a pending intent, and replays. It
// returns the replay's error.
//
// Step 3 is payments.Charge, chosen deliberately: it is the case the whole
// feature exists for, and the fixture propagates its error to the top level
// (see the per-step table in tests/integrity/ambiguity_detection_test.go).
func replayWithPendingIntentAtStep(t *testing.T, step int) error {
	t.Helper()

	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "basic"))
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	backend, err := NewWasmtimeBackend(ctx)
	if err != nil {
		// Not a t.Skip. The classification this test covers happens on the
		// wasmtime path, and a CGO-less build has no backend at all -- skipping
		// would report the suite green having executed nothing. Same reasoning
		// as guest_failure_test.go.
		t.Fatalf("wasmtime backend unavailable: %v (if this build disabled CGO, "+
			"that is the defect: it removes the primary backend entirely)", err)
	}
	defer backend.Close(ctx)

	input := []byte(`{"userID":"test-user","cart":[{"sku":"ABC-123","qty":1}]}`)

	// ---- Fresh run, to get a real history ----
	eng := NewEngine(rt, &mockCaller{}, WithBackend("go", backend))
	_, history, _, _, _, err := eng.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Fatalf("fresh execute: %v", err)
	}
	if len(history) <= step {
		t.Fatalf("history has %d events, need more than %d", len(history), step)
	}

	// ---- Leave one call mid-flight ----
	modified := make([]EventRecord, len(history))
	copy(modified, history)
	modified[step].Pending = true
	modified[step].Response = ""

	rt2, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt2.Close(ctx)

	eng2 := NewEngine(rt2, &mockCaller{}, WithBackend("go", backend))
	_, _, _, _, _, replayErr := eng2.Replay(ctx, wasmBytes, "place_order", input, modified)
	return replayErr
}

// TestAmbiguousReplayCarriesErrAmbiguous is the regression test for 3.24.
//
// Reverting the fix -- dropping the session.classifyFailure call in
// executor.go, or the s.recordAmbiguity call in durablecalls.go -- fails this
// with code "unknown", which is the defect.
func TestAmbiguousReplayCarriesErrAmbiguous(t *testing.T) {
	const chargeStep = 3

	err := replayWithPendingIntentAtStep(t, chargeStep)
	if err == nil {
		t.Fatalf("replay of a pending payments.Charge returned no error; " +
			"the ambiguity was swallowed entirely")
	}

	var ce *CleatError
	if !errors.As(err, &ce) {
		t.Fatalf("replay error is not a *CleatError, so cmd/cleat-worker/setup.go "+
			"stores error_code='unknown' for it.\n  got %T: %v", err, err)
	}
	if ce.Code != ErrAmbiguous {
		t.Errorf("error_code = %q, want %q.\n  err: %v",
			ce.Code.String(), ErrAmbiguous.String(), err)
	}

	// The stored value is what an operator queries, so assert the string that
	// actually reaches the column rather than only the enum.
	if got := ce.Code.String(); got != "ambiguous" {
		t.Errorf("stored error_code = %q, want \"ambiguous\"", got)
	}
}

// TestAmbiguousClassificationPreservesMessage guards the wrap itself.
//
// The classification is carried by a CleatError with no Op and no WorkflowID,
// which CleatError.Error passes through untouched. If that ever starts
// prefixing, the message gains a third redundant prefix on top of the two
// IMPROVEMENT-PLAN 3.23 already complains about, and the "[AMBIGUOUS]" text
// that tests/integrity matches by substring could move or be mangled.
func TestAmbiguousClassificationPreservesMessage(t *testing.T) {
	const chargeStep = 3

	err := replayWithPendingIntentAtStep(t, chargeStep)
	if err == nil {
		t.Fatal("replay returned no error")
	}

	msg := err.Error()
	if !strings.Contains(msg, "[AMBIGUOUS]") {
		t.Errorf("message lost the [AMBIGUOUS] marker that tests/integrity "+
			"matches on:\n  %s", msg)
	}
	if strings.HasPrefix(msg, ": ") {
		t.Errorf("classification wrap added an empty prefix:\n  %s", msg)
	}
}

// TestClassifyFailureLeavesOtherFailuresAlone pins the negative case directly.
//
// A session that never saw a pending intent must not tag anything, and a
// workflow that caught the ambiguous call and completed must not be reported as
// a failure. Without the second check, classifyFailure could tag a nil error
// and turn a success into an ambiguous failure.
func TestClassifyFailureLeavesOtherFailuresAlone(t *testing.T) {
	base := errors.New("host: workflow wf-1: execution failed: cart is empty")

	t.Run("no ambiguity recorded", func(t *testing.T) {
		s := &execSession{}
		got := s.classifyFailure(base)
		if got != base {
			t.Errorf("classified an unrelated failure: %v", got)
		}
		var ce *CleatError
		if errors.As(got, &ce) {
			t.Errorf("unrelated failure gained code %q", ce.Code.String())
		}
	})

	t.Run("ambiguity recorded but execution succeeded", func(t *testing.T) {
		s := &execSession{ambiguity: &ambiguousCall{Step: 3, Service: "payments", Op: "Charge"}}
		if got := s.classifyFailure(nil); got != nil {
			t.Errorf("classifyFailure(nil) = %v, want nil; a workflow that "+
				"handled the ambiguous call and completed is not a failure", got)
		}
	})

	t.Run("ambiguity recorded and execution failed", func(t *testing.T) {
		s := &execSession{ambiguity: &ambiguousCall{Step: 3, Service: "payments", Op: "Charge"}}
		got := s.classifyFailure(base)
		var ce *CleatError
		if !errors.As(got, &ce) || ce.Code != ErrAmbiguous {
			t.Fatalf("failure was not classified ambiguous: %T %v", got, got)
		}
		if got.Error() != base.Error() {
			t.Errorf("message changed by the wrap:\n  got  %s\n  want %s", got.Error(), base.Error())
		}
		if !errors.Is(got, base) {
			t.Error("wrap broke the error chain; errors.Is no longer finds the cause")
		}
	})
}

// TestRecordAmbiguityKeepsTheFirst pins the first-one-wins rule. The earliest
// unresolved call is the one whose side effect has been in doubt longest, and
// naming a later one would point reconciliation at the wrong operation.
func TestRecordAmbiguityKeepsTheFirst(t *testing.T) {
	s := &execSession{}
	s.recordAmbiguity(EventRecord{Step: 3, Service: "payments", Op: "Charge"})
	s.recordAmbiguity(EventRecord{Step: 7, Service: "shipping", Op: "CreateShipment"})

	if s.ambiguity == nil {
		t.Fatal("nothing recorded")
	}
	if s.ambiguity.Step != 3 || s.ambiguity.Service != "payments" {
		t.Errorf("recorded %+v, want the first call (step 3, payments.Charge)", *s.ambiguity)
	}
}
