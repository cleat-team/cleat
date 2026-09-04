//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// The end-to-end half of IMPROVEMENT-PLAN 3.107, and the thing that lets
// `rust` into deferSegmentLanguages.
//
// #647 gave the Rust SDK memory::SUSPEND_STOP_BIT and stop_requested, and put
// the check on the nine host calls the host can refuse -- with unit tests in
// crates/cleat-sdk that the helper reports the bit, and a Go test reading the
// Rust source that the constant equals callSuspendSentinel. None of those runs
// a Rust guest. The fence in Execute (engine/executor.go) stayed closed on
// exactly that argument: a host that emits and a guest that never decodes are
// two green half-tests and no working feature (3.73).
//
// So this test is the crossing, and it mirrors java_defer_segment_e2e_test.go
// deliberately -- same fixture shape, same three assertions, same control.
//
// **Both tests here are named TestRust... deliberately, and the name is
// load-bearing.** e2e-cross-language.yml selects work with
// `-run "TestRust|TestPython|TestAssemblyScript|TestJava"`, so a name like
// TestARustDeferSegment... does not match: "TestARust" does not contain
// "TestRust". It would skip in every job that runs it and never run in the one
// job provisioned for it, while the suite printed ok.
//
// Why the whole toolchain rather than a synthetic module: the Rust path has
// layers a hand-built module does not. The refusal arrives as an ordinary
// result word, stop_requested has to be consulted BEFORE any field of it is
// decoded (bit 31 overlaps a real field in several layouts), the wrapper has
// to suspend on a flag rather than on a value the body discarded, and the host
// then calls __cleat_run_deferred as a second export into an instance that has
// already decided to suspend. Reading the Rust source with a regex exercises
// none of that.

// rustDeferSegment runs defer_order on the real Rust module, as a defer
// segment or not, and returns what reached the caller.
//
// Both directions matter (see the control test below), so the flag is a
// parameter rather than two near-identical setups.
func rustDeferSegment(t *testing.T, wfID string, deferPhase bool) (*mockCaller, *SuspendResult, error) {
	t.Helper()

	wasmBytes, err := os.ReadFile(buildRustWasm(t))
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	// The fence keys off what the guest declares, so a module that stopped
	// declaring `rust` would take this test somewhere else entirely -- past a
	// fence that never fired, into a segment that was never refused. Assert it
	// rather than assume it: 3.83's hazard is that DetectLanguage returns the
	// guest's own metadata field verbatim.
	if lang := wasm.DetectLanguage(wasmBytes); lang != "rust" {
		t.Fatalf("the built module declares language %q, not \"rust\".\n\n"+
			"deferSegmentLanguages is keyed by this string, so this test would "+
			"be measuring a different code path than the one it names.", lang)
	}

	ctx := context.Background()
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
	opts := []EngineOption{
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID(wfID),
	}
	if deferPhase {
		opts = append(opts, WithDeferPhase())
	}
	eng := NewEngine(rt, caller, opts...)

	_, _, susp, _, _, err := eng.Execute(ctx, wasmBytes,
		"defer_order", json.RawMessage(`{"user_id":"u","cart":[]}`))
	return caller, susp, err
}

// TestRustDeferSegmentRunsOnlyTheDefers is the Rust counterpart of
// TestADeferSegmentPastTheFrontierRunsOnlyTheDefers.
//
// The fixture registers two defers and then makes one call of its own, with
// nothing recorded, so the body's call is past the frontier the moment it is
// made. Three things must hold:
//
//   - the segment must report a suspension rather than a result, because the
//     workflow's outcome was already decided before the segment started.
//   - both cleanups MUST reach the caller, in LIFO order. On its own this
//     passes for a segment that stopped nothing and simply ran the workflow.
//   - `body` must NOT reach the caller.
//
// **Those three are not equally strong, and the third is the weak one**, for
// the same reason it is weak in the Java test: stopBeforeNewWork refuses the
// call HOST-side before the ServiceCaller is reached, so `body` stays absent
// even with the guest's decode deleted. The sentinel is what tells the guest,
// not what does the refusing. It is kept because it can still fail on a
// host-side regression, and this comment says which half it guards.
//
// Measured 2026-09-03 by making stop_requested return false unconditionally
// and rebuilding, the falsification signature is:
//
//	suspended: nil   result: {"deferred":true}   operations: []
//
// The segment reported the terminated workflow as completed, and performed
// none of its cleanup -- the #[cleat_entry] wrapper saw no suspension, took
// its ordinary success path, drained the table itself, and every body's call
// was refused in turn (3.81's consumption), leaving the host's own drain with
// nothing. The first two assertions are the ones that catch it.
func TestRustDeferSegmentRunsOnlyTheDefers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Rust WASM integration test in short mode")
	}

	caller, susp, err := rustDeferSegment(t, "wf-rust-defer-segment", true)
	if err != nil {
		t.Fatalf("the defer segment failed: %v", err)
	}
	if susp == nil {
		t.Fatalf("the Rust defer segment did not suspend.\n\n"+
			"It ran to completion and reported an outcome for a workflow whose "+
			"outcome was already decided. Operations recorded: %v",
			operationsCalled(caller))
	}

	got := operationsCalled(caller)
	for _, op := range got {
		if op == "body" {
			t.Fatalf("the Rust workflow body reached the ServiceCaller: %v.\n\n"+
				"The guest performed the workflow's own side effect during its "+
				"cleanup segment. This is the HOST half: stopBeforeNewWork "+
				"(engine/durablecalls.go) refuses a body call past the frontier "+
				"before the ServiceCaller is reached, so getting here means the "+
				"refusal itself is gone, not that the Rust SDK stopped decoding "+
				"the sentinel. See this test's doc comment.", got)
		}
	}

	want := []string{"second", "first"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the Rust defer segment recorded %v, want exactly %v.\n\n"+
			"An empty list is the 3.81 failure: the #[cleat_entry] wrapper "+
			"drained the table on its own way out, so the host's call to "+
			"__cleat_run_deferred found nothing left. The wrapper must return "+
			"the sentinel WITHOUT calling run_deferred when the segment "+
			"suspended.\n"+
			"Reversed order means the drain is running registration-order, and "+
			"a defer releases what the defer before it acquired.", got, want)
	}
}

// TestRustOrdinarySegmentRunsTheBody is the other direction, and the test
// above cannot be trusted without it.
//
// Every assertion above is satisfied by a Rust SDK that refuses every call
// unconditionally -- `body` absent, and the defers recorded because the host's
// drain runs them through a path the refusal happens not to cover. Running the
// same entry point with no defer phase separates "the stop is conditional on
// the segment" from "this guest stopped working".
func TestRustOrdinarySegmentRunsTheBody(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Rust WASM integration test in short mode")
	}

	caller, susp, err := rustDeferSegment(t, "wf-rust-ordinary", false)
	if err != nil {
		t.Fatalf("the ordinary segment failed: %v", err)
	}
	if susp != nil {
		t.Fatalf("an ordinary segment suspended; the fixture has no sleep and " +
			"nothing should stop it")
	}

	// body first, then the defers the wrapper drains on its own way out.
	want := []string{"body", "second", "first"}
	got := operationsCalled(caller)
	if len(got) != len(want) {
		t.Fatalf("an ordinary Rust segment recorded %v, want %v.\n\n"+
			"If `body` is missing here, the SDK is stopping calls that no defer "+
			"segment asked it to stop, and the test above passes for the wrong "+
			"reason.", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("an ordinary Rust segment recorded %v, want %v", got, want)
		}
	}
}
