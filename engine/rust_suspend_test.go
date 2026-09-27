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

// The Rust SDK suspends without unwinding. IMPROVEMENT-PLAN 3.87.
//
// It used to suspend by std::panic::panic_any(SuspendSentinel), with
// #[cleat_entry] wrapping the workflow body in std::panic::catch_unwind to
// intercept it and return memory::SUSPEND_SENTINEL to the host. Its README
// documented exactly that: "catch_unwind so SuspendSentinel panics are safely
// intercepted".
//
// wasm32-wasip1 builds with panic=abort. There is no unwinding, so catch_unwind
// could not catch anything: the panic aborted, which in WASM is the
// `unreachable` instruction, which is a trap. Every Rust suspension was a
// trapped guest.
//
// It never looked broken because the paths that suspend also set
// session.suspendErr on the HOST side -- cleat_sleep, cleat_await_child,
// cleat_await_signals all record their own suspension before returning to the
// guest. executor.go then deliberately lets a suspension win over the error
// that accompanied it ("callErr != nil && session.suspendErr == nil"), so the
// trap was discarded and the run reported as a clean suspension.
//
// THAT MASK IS WHY THESE TESTS ARE SHAPED THE WAY THEY ARE. A probe that
// suspends through a host call reports a clean suspension whether the guest
// works or traps, so it cannot tell the fix from the bug. The mask-free probe
// below sets the suspension flag with no host call in the way, leaving the
// guest's own wrapper as the only thing that can produce a suspension.
//
// Suspension is now a return value -- Result<T, CallError> and `?` -- with a
// thread-local flag as the backstop for a body that discards the Err. Both are
// tested here: the mechanism and the backstop fail independently.

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
			"That is the whole reason the Rust SDK cannot use unwinding to "+
			"suspend, and therefore the reason suspension is a Result rather "+
			"than a panic (IMPROVEMENT-PLAN 3.87). If unwinding is now "+
			"available, the Result design is still correct -- it is explicit "+
			"and target-independent -- but this test no longer pins anything "+
			"and the SDK could offer an unwinding path as well. Nothing here "+
			"is unsafe if it changes; it just stops being load-bearing."+
			"\n\nrustc --print cfg said:\n%s", out)
	}
}

// TestARustGuestSuspendsCleanly is the regression test for 3.87, and it is
// deliberately the MASK-FREE one.
//
// suspend_probe sets the suspension flag with no host call in the way, so
// nothing sets session.suspendErr and nothing can mask the outcome. The only
// thing that can make this a suspension is #[cleat_entry] reading the flag and
// returning memory::SUSPEND_SENTINEL, which the engine decodes into
// res.Suspended (executor.go:320).
//
// Before the fix the equivalent probe raised a panic here and trapped with
// "wasm trap: unreachable". A broken version of the fix -- a wrapper that never
// checks the flag -- would instead return a normal completed result. Neither is
// this, and the two failure modes are distinguishable in the message below.
func TestARustGuestSuspendsCleanly(t *testing.T) {
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

	result, _, susp, _, _, err := eng.Execute(ctx, wasmBytes, "suspend_probe",
		json.RawMessage(`{"user_id":"u","cart":[]}`))

	if err != nil {
		if strings.Contains(err.Error(), "unreachable") {
			t.Fatalf("the Rust guest TRAPPED instead of suspending: %v\n\n"+
				"That is the pre-3.87 behaviour returning: something in the SDK "+
				"is suspending by panicking again, and panic=abort compiles a "+
				"panic to exactly this trap. Check crates/cleat-sdk for "+
				"panic_any and crates/cleat-macro/src/entry.rs for catch_unwind.", err)
		}
		t.Fatalf("suspend_probe returned an error: %v", err)
	}
	if susp == nil {
		t.Fatalf("the guest neither suspended nor failed; it returned %q.\n\n"+
			"#[cleat_entry] did not act on cleat_sdk::is_suspended(). The body "+
			"set the flag and returned Ok, and the wrapper formatted that Ok as "+
			"a completed workflow -- so a workflow that asked to suspend was "+
			"reported as finished. That is the same class of defect 3.87 fixed, "+
			"reintroduced in the fix.", result)
	}
}

// The backstop -- a body that DISCARDS Err(CallError::Suspended) -- is not
// tested from here, deliberately, and the reason is worth recording because a
// test for it was written and then deleted.
//
// Its only distinguishing consequence is guest-side: whether the body's own
// value reaches the host. But every suspending host call sets
// session.suspendErr, and executor.go then returns a SuspendResult with an
// empty result string whatever the guest returned -- so a probe that discards
// the Err reports EXACTLY the same thing through Execute whether the backstop
// works or not. Measured: with both flag checks deleted from
// #[cleat_entry], the Go-side assertion still passed.
//
// That is a test that cannot fail, which is the thing this file exists to warn
// about. The property is real and is tested where it is observable, in
// crates/cleat-sdk/tests/suspend_backstop.rs, which calls the generated export
// directly and reads its return value.
