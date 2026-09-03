//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The Rust SDK's suspend mechanism does not work, and the host has been hiding
// it. IMPROVEMENT-PLAN 3.86.
//
// crates/cleat-sdk suspends by std::panic::panic_any(SuspendSentinel), and
// #[cleat_entry] wraps the workflow body in std::panic::catch_unwind to
// intercept it and return memory::SUSPEND_SENTINEL to the host. Its README
// documents exactly that: "catch_unwind so SuspendSentinel panics are safely
// intercepted".
//
// wasm32-wasip1 builds with panic=abort. There is no unwinding, so catch_unwind
// cannot catch anything: the panic aborts, which in WASM is the `unreachable`
// instruction, which is a trap. Every Rust suspension is a trapped guest.
//
// It has never looked broken because the paths that suspend also set
// session.suspendErr on the HOST side -- cleat_sleep, cleat_await_child,
// cleat_await_signals all record their own suspension before returning to the
// guest. executor.go then deliberately lets a suspension win over the error
// that accompanied it ("callErr != nil && session.suspendErr == nil"), so the
// trap is discarded and the run is reported as a clean suspension. The guest
// died; the workflow suspended; nothing anywhere says so.
//
// That mask is why this matters beyond tidiness. It holds only where the host
// sets suspendErr itself. IMPROVEMENT-PLAN 3.84's stop sentinel does not -- the
// host refuses the call and leaves the guest to unwind -- so a Rust guest in a
// defer segment traps outright, which is how this was found.

// buildRustProbeWasm is buildRustWasm's contract, restated here because this
// test needs the release build that carries suspend_probe.
func buildRustProbeWasm(t *testing.T) []byte {
	t.Helper()
	path := buildRustWasm(t) // skips only when cargo or wasm32-wasip1 is absent
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

// TestTheRustTargetCompilesWithPanicAbort is the mechanism, asked of the
// compiler rather than asserted in prose.
//
// Pinning it here means the day the toolchain gains unwinding for this target
// -- or the day someone builds against wasm32-unknown-unknown with exception
// handling enabled -- this test fails and points at the two below, which can
// then be rewritten to assert the working behaviour instead of the broken one.
func TestTheRustTargetCompilesWithPanicAbort(t *testing.T) {
	if _, err := exec.LookPath("rustc"); err != nil {
		if toolchainRequired("rust") {
			t.Fatalf("rustc not installed, but %s declares rust: %v", requireToolchainEnv, err)
		}
		t.Skip("rustc not installed")
	}
	out, err := exec.Command("rustc", "--print", "cfg", "--target", "wasm32-wasip1").Output()
	if err != nil {
		t.Fatalf("rustc --print cfg: %v", err)
	}
	if !strings.Contains(string(out), `panic="abort"`) {
		t.Fatalf("wasm32-wasip1 no longer reports panic=\"abort\".\n\n"+
			"That is the whole reason catch_unwind cannot intercept "+
			"SuspendSentinel in crates/cleat-macro's #[cleat_entry] wrapper. If "+
			"unwinding is now available, the SDK's documented mechanism may "+
			"actually work -- recheck the two tests below and "+
			"IMPROVEMENT-PLAN 3.86.\n\nrustc --print cfg said:\n%s", out)
	}
}

// TestARustGuestCannotSuspendByPanicking pins the actual boundary.
//
// suspend_probe raises the SDK's own suspend panic with no host call in the
// way, so nothing can set session.suspendErr and nothing can mask the result.
// A working catch_unwind would return memory::SUSPEND_SENTINEL and this would
// be a clean suspension with no error. It traps.
//
// Measured 2026-09-02, wasm32-wasip1 release build on wasmtime.
func TestARustGuestCannotSuspendByPanicking(t *testing.T) {
	ctx := context.Background()
	wasmBytes := buildRustProbeWasm(t)

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

	eng := NewEngine(rt, &mockCaller{},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-rust-suspend-probe"))

	_, _, susp, _, _, err := eng.Execute(ctx, wasmBytes, "suspend_probe",
		json.RawMessage(`{"user_id":"u","cart":[]}`))

	if susp != nil && err == nil {
		t.Fatalf("the Rust guest suspended cleanly.\n\n" +
			"That is the BEHAVIOUR WE WANT, and this test asserts the broken one " +
			"because it was broken when written. If catch_unwind now intercepts " +
			"SuspendSentinel, delete this test, revisit " +
			"TestThePanicTrapIsMaskedWhereverTheHostSetsSuspendErr, and add " +
			"\"rust\" to deferSegmentLanguages with a defer-segment test to back " +
			"it. See IMPROVEMENT-PLAN 3.86.")
	}
	if err == nil {
		t.Fatalf("expected a trap, got no error at all (susp=%v)", susp != nil)
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("expected a wasm `unreachable` trap -- panic=abort compiles a "+
			"panic to exactly that -- got: %v", err)
	}
}

// TestThePanicTrapIsMaskedWhereverTheHostSetsSuspendErr is the half that
// explains why nobody noticed for as long as the Rust SDK has existed.
//
// Same guest, same trap, but through cleat_sleep, which records its own
// suspension on the host before returning. The run comes back as a clean
// suspension with err == nil: identical, from the outside, to a guest that
// unwound correctly.
//
// This is the observable that cannot tell two states apart -- the recurring
// shape in this file and in CLAUDE.md's "Is this result real?". The pair of
// tests is the point; either alone is misleading. Delete neither without the
// other.
func TestThePanicTrapIsMaskedWhereverTheHostSetsSuspendErr(t *testing.T) {
	ctx := context.Background()
	wasmBytes := buildRustProbeWasm(t)

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

	// Pin both clocks. seedNowMs falls back to the package-level nowMs atomic
	// when there is no history and no start time, and other tests in this
	// package move it -- with a small seed the deadline is already in the past,
	// DurableSleep returns "completed" instead of suspending, and this test
	// fails only when run alongside them. It did, once, and passing in
	// isolation is exactly the reading this file is about.
	const t0 int64 = 1_700_000_000_000
	eng := NewEngine(rt, &mockCaller{},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-rust-sleep-mask"),
		WithWorkflowStartTime(t0),
		WithClock(func() int64 { return t0 }))

	// sleep_probe raises the same panic as suspend_probe, but through
	// cleat_sleep, which records the suspension on the host first.
	_, _, susp, _, _, err := eng.Execute(ctx, wasmBytes, "sleep_probe",
		json.RawMessage(`{"user_id":"u","cart":[]}`))

	if err != nil {
		t.Fatalf("sleep_probe returned an error: %v\n\n"+
			"The mask is the finding: this run must come back CLEAN even though "+
			"the guest trapped, because the host set session.suspendErr before "+
			"the guest panicked. An error here means the mask is gone -- which "+
			"would be an improvement, and means IMPROVEMENT-PLAN 3.86 and the "+
			"test above both need rewriting.", err)
	}
	if susp == nil {
		t.Fatalf("sleep_probe neither suspended nor failed; there is no third " +
			"state worth having here.")
	}

	t.Log("pinned: the same guest that traps in " +
		"TestARustGuestCannotSuspendByPanicking reports a clean suspension here, " +
		"because executor.go lets session.suspendErr win over the trap that came " +
		"with it. The trap is invisible wherever the host records the suspension " +
		"itself.")
}
