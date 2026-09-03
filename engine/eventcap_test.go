//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// The event cap -- --max-quota-events on the worker -- is supposed to stop a
// workflow that has recorded too many events and continue it as a fresh run.
//
// It crashed the worker process instead, on the only backend a worker has, and
// these tests are the two halves of that: that the crash is gone, and that what
// replaced it is a continue_as_new suspension rather than some other survival.
//
// The crash needed two nil dereferences in sequence, which is why neither is
// obvious in isolation:
//
//  1. freshCall called m.CloseWithExitCode on the api.Module it was handed.
//     wasmtime_hostfuncs.go passes nil for that argument -- every wasmtime host
//     function does -- so this was a method call on a nil interface.
//  2. The panic did not fail the workflow. ContinueAsNew had already set
//     session.suspendErr, so executor.go's error branch is guarded by
//     "callErr != nil && suspendErr == nil" and declined it. Control reached
//     `res.Suspended` with res nil, because every error return in the wasmtime
//     backend is `return nil, err`. That dereference is inside no recover.
//
// So the observable was not a failed workflow but a dead process -- and a
// workflow that is still in the queue when the worker restarts, so the next
// worker to claim it dies too.

// eventCapEngine builds a real Go SDK guest on wasmtime with the cap already
// reached: initialEventCount == maxQuotaEvents is what the worker constructs
// for a workflow whose stored event_count has hit the limit
// (cmd/cleat-worker/setup.go seeds WithInitialEventCount from GetEventCount),
// so the first fresh call of the segment trips the cap.
func eventCapEngine(t *testing.T, wfID string, cap int) ([]byte, *Engine, *mockCaller) {
	t.Helper()
	ctx := context.Background()

	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "deferfunc"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID(wfID),
		WithMaxQuotaEvents(cap),
		WithInitialEventCount(cap))
	return wasmBytes, eng, caller
}

// TestTheEventCapContinuesAsNewInsteadOfKillingTheWorker is the whole finding.
//
// Before the fix this test does not fail -- it takes the process down with
// SIGSEGV, so `go test` reports the package as failed with no test result at
// all. That is worth knowing when re-proving it: revert a guard and the signal,
// not a red assertion, is the evidence.
//
// Both guards are load-bearing, measured separately 2026-09-02 by reverting one
// at a time. Reverting only executor.go's `res != nil` still segfaults with the
// refusal in place, because the refused guest returns an error and the backend
// answers `return nil, err` for that too -- so the nil res reaches line 296
// whether the first dereference happened or not. Neither fix covers for the
// other.
//
// Measured 2026-09-02 on wasmtime with the Go SDK.
func TestTheEventCapContinuesAsNewInsteadOfKillingTheWorker(t *testing.T) {
	wasmBytes, eng, _ := eventCapEngine(t, "wf-event-cap", 1)

	out, _, susp, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_order", json.RawMessage(`{}`))

	if err != nil {
		t.Fatalf("execute failed: %v\n\n"+
			"Reaching the cap is not a workflow failure. The engine recorded a "+
			"ContinueAsNew before refusing the call, so the run must come back as "+
			"a suspension the worker can act on.", err)
	}
	if susp == nil {
		t.Fatalf("no suspension; the run returned %q as if it had completed.\n\n"+
			"A workflow that hit the event cap has not finished -- reporting it "+
			"complete drops the rest of its body silently.", out)
	}
	if susp.Reason != "continue_as_new" {
		t.Fatalf("suspension reason %q, want \"continue_as_new\".\n\n"+
			"Any other reason means the worker reschedules this run instead of "+
			"starting a fresh one, and the fresh run is the only thing that "+
			"resets the event count -- so it would hit the cap again forever.",
			susp.Reason)
	}
}

// TestTheEventCapDoesNotDispatchTheCallItRefused is the other half: a refusal
// must not have a side effect.
//
// The cap branch runs before the call is dispatched, so the service must never
// see it. This is what would catch a future rewrite that refuses the guest but
// dispatches anyway -- the continued run makes the same call for real, so the
// side effect would happen twice.
//
// The refused call is the workflow body's, and identifying it needs the fix in
// executeWithBackend as well: the cap is seeded from initialEventCount only
// once the backend path sets it, and without that seed the count restarted at
// zero each segment and the trip landed on a defer body instead.
func TestTheEventCapDoesNotDispatchTheCallItRefused(t *testing.T) {
	wasmBytes, eng, caller := eventCapEngine(t, "wf-event-cap-nodispatch", 1)

	if _, _, susp, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_order", json.RawMessage(`{}`)); err != nil || susp == nil {
		t.Fatalf("setup: susp=%v err=%v", susp, err)
	}

	for _, c := range caller.calls {
		if c.Service == "work" && c.Op == "body" {
			t.Fatalf("the workflow body's call was dispatched after the cap refused it.\n\n"+
				"The continued run will make this call again, so the side effect "+
				"happens twice. dispatched=%v", opsOf(caller.calls))
		}
	}
	// The defer bodies' calls are dispatched, and must be: a defer runs at a
	// continue-as-new boundary exactly as it does for an explicit
	// ContinueAsNew, where the guest returns normally through the wrapper that
	// drains them. Asserting only the absence above would pass just as well
	// against a guest that was killed outright and ran no cleanup at all.
	if got := len(caller.calls); got != 2 {
		t.Fatalf("%d calls dispatched, want 2 (both defer bodies).\n\n"+
			"0 means the guest never drained its defer table -- the cleanup a "+
			"workflow registered did not run. dispatched=%v", got, opsOf(caller.calls))
	}
}

// TestTheContinuedRunKeepsTheWorkflowInput is the data-loss half.
//
// freshCall records ContinueAsNew with session.originalInput, and
// executeWithBackend -- the path a worker takes -- never set that field. It
// therefore recorded an empty input, so a workflow the cap continued restarted
// with no input at all. Nothing reports this: the continued run is a valid run
// of the same workflow, just one that was handed "" instead of its arguments.
func TestTheContinuedRunKeepsTheWorkflowInput(t *testing.T) {
	const input = `{"order_id":"A-17","items":3}`
	wasmBytes, eng, _ := eventCapEngine(t, "wf-event-cap-input", 1)

	_, _, susp, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_order", json.RawMessage(input))
	if err != nil || susp == nil {
		t.Fatalf("setup: susp=%v err=%v", susp, err)
	}

	if susp.NewInput != input {
		t.Fatalf("the continued run's input is %q, want %q.\n\n"+
			"An empty input means the workflow restarts having lost its "+
			"arguments -- and it restarts successfully, so there is nothing to "+
			"see beyond the wrong behaviour.", susp.NewInput, input)
	}
}

func opsOf(calls []CallRecord) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Service+"/"+c.Op)
	}
	return out
}
