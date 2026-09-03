//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// The end-to-end half of IMPROVEMENT-PLAN 3.105, and the thing that lets
// `java` into deferSegmentLanguages.
//
// #644 gave the Java SDK Memory.throwIfStopped and put it on all ten host
// calls the host can refuse, checked from both ends -- JUnit tests that the
// helper throws on bit 31 and Go tests reading the Java source that every
// method calls it before decoding. Neither of those runs a Java guest. The
// fence in Execute (engine/executor.go) stayed closed on exactly that
// argument: "a host that emits and a guest that never decodes are two green
// half-tests and no working feature".
//
// So this test is the crossing. It builds the real TeaVM module, runs it as a
// defer segment, and measures what reached the ServiceCaller.
//
// **Both tests here are named TestJava... deliberately, and the name is
// load-bearing.** e2e-cross-language.yml is the only job that installs Java,
// and it selects work with `-run "TestRust|TestPython|TestAssemblyScript|TestJava"`.
// These were first written as TestAJavaDeferSegment... and TestAnOrdinaryJava...,
// which that regex does not match: "TestAJava" does not contain "TestJava". They
// would then have skipped in every job that runs them and never run in the one
// job that could, while the suite printed ok -- a green result that measured
// nothing, from a test written specifically to stop that happening. A
// cross-language test whose name does not start with the language prefix is
// dead code with a skip in front of it.
//
// Why the whole toolchain rather than a synthetic module: the sentinel has to
// survive three layers that a hand-built module does not have. TeaVM compiles
// `throw` into its own unwinding scheme, the generated wrapper catches
// SuspendSignal in a `catch` clause TeaVM also compiles, and the drain the
// host then calls is a second export into a runtime that has just thrown. Any
// of the three could swallow the signal, and none of them is exercised by
// reading Java source with a regex.

// javaDeferSegment runs the named entry point on the real Java module, as a
// defer segment or not, and returns what reached the caller.
//
// Both directions matter here (see the control test below), so the flag is a
// parameter rather than two near-identical setups.
func javaDeferSegment(t *testing.T, wfID string, deferPhase bool) (*mockCaller, *SuspendResult, error) {
	t.Helper()

	wasmBytes, err := os.ReadFile(buildJavaWasm(t))
	if err != nil {
		t.Fatalf("read Java WASM: %v", err)
	}

	// The fence keys off what the guest declares, so a module that stopped
	// declaring `java` would take the whole test somewhere else -- past a fence
	// that never fired, into a segment that was never refused. Assert it rather
	// than assume it: 3.83's hazard is exactly that DetectLanguage returns the
	// guest's own metadata field verbatim.
	if lang := wasm.DetectLanguage(wasmBytes); lang != "java" {
		t.Fatalf("the built module declares language %q, not \"java\".\n\n"+
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
		"defer_order", json.RawMessage(`{}`))
	return caller, susp, err
}

// TestJavaDeferSegmentRunsOnlyTheDefers is the Java counterpart of
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
// **Those three are not equally strong, and the third is the weak one.**
// Measured 2026-09-03 by deleting `Memory.throwIfStopped` from `cleatCall` and
// rebuilding: `body` was still absent, because stopBeforeNewWork refuses the
// call HOST-side before the ServiceCaller is ever reached -- the sentinel is
// what tells the guest, not what does the refusing. So the `body` check cannot
// fail on a guest-side regression at all. It is kept because it can still fail
// on a host-side one, and the comment says which half it guards rather than
// leaving a reader to assume it covers both.
//
// What that same falsification DID produce is the signature worth knowing:
//
//	suspended: nil   result: {"status":"ok"}   operations: []
//
// The segment reported the terminated workflow as having completed normally,
// and performed none of its cleanup -- the wrapper drained the table on its
// ordinary success path and every body's call was refused in turn, which is
// 3.81's consumption. The first two assertions are the ones that catch it.
// Falsifying the other end agrees: making the generated wrapper drain in its
// `catch (SuspendSignal)` branch gives `defers_run=0` and the same empty list.
func TestJavaDeferSegmentRunsOnlyTheDefers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Java WASM integration test in short mode")
	}

	caller, susp, err := javaDeferSegment(t, "wf-java-defer-segment", true)
	if err != nil {
		t.Fatalf("the defer segment failed: %v", err)
	}
	if susp == nil {
		t.Fatalf("the Java defer segment did not suspend.\n\n"+
			"It ran to completion and reported an outcome for a workflow whose "+
			"outcome was already decided. Operations recorded: %v",
			operationsCalled(caller))
	}

	got := operationsCalled(caller)
	for _, op := range got {
		if op == "body" {
			t.Fatalf("the Java workflow body reached the ServiceCaller: %v.\n\n"+
				"The guest performed the workflow's own side effect during its "+
				"cleanup segment. This is the HOST half: stopBeforeNewWork "+
				"(engine/durablecalls.go) refuses a body call past the frontier "+
				"before the ServiceCaller is reached, so getting here means the "+
				"refusal itself is gone, not that the Java SDK stopped decoding "+
				"the sentinel. See this test's doc comment.", got)
		}
	}

	want := []string{"second", "first"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the Java defer segment recorded %v, want exactly %v.\n\n"+
			"An empty list is the 3.81 failure: the generated wrapper drained the "+
			"table itself on the way out, so the host's own drain found nothing "+
			"left. The wrapper's `catch (SuspendSignal)` branch must return the "+
			"sentinel WITHOUT calling Defer.runDeferred -- see CleatEntryProcessor.\n"+
			"Reversed order means the drain is running registration-order, and a "+
			"defer releases what the defer before it acquired.", got, want)
	}
}

// TestJavaOrdinarySegmentRunsTheBody is the other direction, and this test
// cannot be trusted without it.
//
// Every assertion above is satisfied by a Java SDK that refuses every call
// unconditionally -- `body` absent, and the defers recorded because the drain
// runs them through a path the refusal happens not to cover. Running the same
// entry point with no defer phase separates "the stop is conditional on the
// segment" from "this guest stopped working".
func TestJavaOrdinarySegmentRunsTheBody(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Java WASM integration test in short mode")
	}

	caller, susp, err := javaDeferSegment(t, "wf-java-ordinary", false)
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
		t.Fatalf("an ordinary Java segment recorded %v, want %v.\n\n"+
			"If `body` is missing here, the SDK is stopping calls that no defer "+
			"segment asked it to stop, and the test above passes for the wrong "+
			"reason.", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("an ordinary Java segment recorded %v, want %v", got, want)
		}
	}
}
