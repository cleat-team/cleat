//go:build cgo

package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/wasm"
)

// A real C# component held through a durable sleep, and the execution fence on a C# component. THESE ROWS NEED NO TOOLCHAIN AND CANNOT SKIP: they read the checked-in
// engine/testdata/csharp_defer_order/defer_order.wasm (a prebuilt C# component) and stamp it as csharp in the test, so they run in Tier 1 on every machine. That is the
// condition scripts/check-csharp-tier.py's exemption for this file rests on, and it is why they sit in their own file: the Python rows in
// a_real_component_is_held_through_a_sleep_and_charged_its_language_test.go DO skip when the toolchain is absent, and an exemption for a file that mixed the two would be
// false. The shared helpers (executeAComponent, tryExecuteAComponent, heldRun, requireHeldInPlace) live in that file; see its header for what the rows show and do not.

func TestCSharpComponentIsHeldThroughADurableSleepAndChargedAsCSharp(t *testing.T) {
	// The checked-in C# fixture is UNSTAMPED (DetectLanguage reads it as python), so it is stamped here the way `cleat build --target csharp` stamps a real build
	// (cmd/cleat/build_csharp.go, which refuses to write a component that does not read back as csharp). Without this the row would charge it as Python.
	b, err := os.ReadFile(filepath.Join("testdata", "csharp_defer_order", "defer_order.wasm"))
	if err != nil {
		t.Fatalf("read the C# fixture: %v", err)
	}
	b, err = wasm.WriteMetadata(b, &wasm.Metadata{Language: "csharp"})
	if err != nil {
		t.Fatalf("stamp the fixture: %v", err)
	}
	if got := wasm.DetectLanguage(b); got != "csharp" {
		t.Fatalf("PRECONDITION FAILED: the stamped fixture is detected as %q, so the C# charge is not what is being tested", got)
	}
	langs := append(append([]string{}, WasmtimeLanguages...), "csharp")
	const input = `{"mode":"sleep"}` // the fixture's sleep is 400 ms

	// Off first: it also compiles and caches the component, so the run below is not paying for a cold compile inside its hold.
	off := executeAComponent(t, b, langs, input, false, "csharp-fixture@1")
	on := executeAComponent(t, b, langs, input, true, "csharp-fixture@1")
	requireHeldInPlace(t, "the C# component", off, on)

	want := defaultResidentInstanceBytes["csharp"] + defaultResidentFixedBytes["csharp"]
	if on.maxCharged != want {
		t.Errorf("while held the set was charged at most %d bytes, want the C# instance plus its fixed cost = %d. A figure of %d would be the Python charge: this run was charged as the wrong language.",
			on.maxCharged, want, defaultResidentInstanceBytes["python"]+defaultResidentFixedBytes["python"])
	}
	// A hold cannot be shorter than the sleep it waits out.
	if on.gap < 350*time.Millisecond {
		t.Errorf("the two calls were %s apart for a 400 ms sleep: the run did not wait", on.gap)
	}
}

// THE EXECUTION FENCE ON A C# COMPONENT (cleat#4278). The fixture's `spin` mode is a guest busy-loop whose own comment says it is "killed by the epoch fence", and no
// test ever selected it, so nothing in CI ran busy C# guest code under the fence. Measured when this was written: it is stopped at 5 ms, 300 ms and 2 s fences, native
// and on linux arm64. A first attempt at the held-versus-fence row below used the fixture's sleep mode as its liveness control and FAILED: that mode uses under one epoch
// tick of guest time before its first host call, so a 5 ms fence has nothing to interrupt in it. The control has to be a guest that actually runs.
func csharpFixtureStamped(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "csharp_defer_order", "defer_order.wasm"))
	if err != nil {
		t.Fatalf("read the C# fixture: %v", err)
	}
	b, err = wasm.WriteMetadata(b, &wasm.Metadata{Language: "csharp"})
	if err != nil {
		t.Fatalf("stamp the fixture: %v", err)
	}
	if got := wasm.DetectLanguage(b); got != "csharp" {
		t.Fatalf("PRECONDITION FAILED: the stamped fixture is detected as %q, not csharp", got)
	}
	return b
}

// requireFenced runs the busy-loop mode under a fence and asserts it was the FENCE that stopped it: the message names the fence's own budget, which a workflow
// deadline (set here far above the fence as a backstop, so a missing fence fails in seconds and not never) would not.
func requireFenced(t *testing.T, b []byte, langs []string, fence time.Duration) {
	t.Helper()
	start := time.Now()
	_, err := tryExecuteAComponent(t, b, langs, `{"mode":"spin"}`, false, "csharp-fixture@1",
		[]WasmtimeOption{WithWasmtimeExecutionTimeout(fence)}, WithDefaultWorkflowTimeout(20*time.Second))
	if err == nil {
		t.Fatalf("a busy C# guest ran to completion under a %s fence", fence)
	}
	want := fmt.Sprintf("execution time limit exceeded (%s wall-clock budget", fence)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("a busy C# guest under a %s fence stopped for another reason after %s (want the message to contain %q): %v", fence, time.Since(start).Round(time.Millisecond), want, err)
	}
}

func TestABusyCSharpGuestIsStoppedByTheExecutionFence(t *testing.T) {
	b := csharpFixtureStamped(t)
	langs := append(append([]string{}, WasmtimeLanguages...), "csharp")
	executeAComponent(t, b, langs, `{"mode":"hello"}`, false, "csharp-fixture@1") // compile and cache first

	for _, fence := range []time.Duration{300 * time.Millisecond, 2 * time.Second} {
		t.Run(fence.String(), func(t *testing.T) { requireFenced(t, b, langs, fence) })
	}

	// The control: the same fixture and the same short fence, a mode that does NOT spin, completes. Without it "the fence stops the guest" is also what a fence that
	// stops EVERY guest, spinning or not, would show.
	if _, err := tryExecuteAComponent(t, b, langs, `{"mode":"hello"}`, false, "csharp-fixture@1",
		[]WasmtimeOption{WithWasmtimeExecutionTimeout(300 * time.Millisecond)}, WithDefaultWorkflowTimeout(20*time.Second)); err != nil {
		t.Fatalf("PRECONDITION FAILED: a guest that does not spin was stopped by the same 300 ms fence: %v", err)
	}
}

// A run held for longer than the fence is not charged for the hold: a held wait is host time (cleat#4174), so a 400 ms hold completes under a 250 ms fence. Its
// liveness control is the one above, the fence STOPS a busy guest at that same 250 ms. What this does not show is that the fence is restarted at the wake on the
// component path, which needs a guest that computes before and after the hold (the core path's timing row); it is not measured on a component.
func TestAHeldCSharpRunLongerThanTheFenceIsNotChargedForTheHold(t *testing.T) {
	b := csharpFixtureStamped(t)
	langs := append(append([]string{}, WasmtimeLanguages...), "csharp")
	const fence = 250 * time.Millisecond
	fenceOpt := []WasmtimeOption{WithWasmtimeExecutionTimeout(fence)}
	bound := WithDefaultWorkflowTimeout(20 * time.Second)
	const input = `{"mode":"sleep"}` // the fixture sleeps 400 ms

	executeAComponent(t, b, langs, `{"mode":"hello"}`, false, "csharp-fixture@1") // compile and cache first
	requireFenced(t, b, langs, fence)                                             // the fence is live on this path at this value

	off, err := tryExecuteAComponent(t, b, langs, input, false, "csharp-fixture@1", fenceOpt, bound)
	if err != nil || !off.suspended {
		t.Fatalf("PRECONDITION FAILED: with residency off and a %s fence the guest did not simply suspend (err=%v, suspended=%v), so its own execution does not fit the fence", fence, err, off.suspended)
	}
	on, err := tryExecuteAComponent(t, b, langs, input, true, "csharp-fixture@1", fenceOpt, bound)
	if err != nil {
		t.Fatalf("a C# run held for 400 ms was stopped by a %s fence: the hold was charged to the guest's execution budget: %v", fence, err)
	}
	if on.suspended || len(on.ops) != 2 {
		t.Fatalf("the held run did not finish in place under a %s fence: suspended=%v ops=%v", fence, on.suspended, on.ops)
	}
	if on.gap < 350*time.Millisecond {
		t.Errorf("the two calls were %s apart: the run did not hold through the 400 ms sleep, so the fence was never tested against a hold longer than itself", on.gap)
	}
}
