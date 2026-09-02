//go:build cgo

package engine

import (
	"encoding/json"
	"testing"
	"time"
)

// The fence is not the only way a workflow dies with its defers outstanding.
//
// engine/fence_reentry_test.go measured one abnormal exit -- an epoch
// interrupt -- and found the instance re-enterable, with the fenced workflow's
// own defer still runnable. IMPROVEMENT-PLAN 3.35 phase 4 was careful not to
// generalise that, because the other ways a guest can be stopped unwind
// differently and nothing had been measured about them.
//
// This file measures the rest, using the same rig, so phase 4 can be designed
// against the whole set rather than the one case that happened to be easy.
//
// What "re-enterable" has to mean here is the same as there: guest code runs
// and reaches the host. An export that returns a plausible int64 having
// executed nothing is the failure this whole line of work keeps guarding
// against.

// reentryOutcome is what one abnormal exit leaves behind.
type reentryOutcome struct {
	stoppedBy    string // how the first call ended, in the host's own words
	reentered    bool   // did the second call return without a trap
	ranGuestCode bool   // did it reach the host
	ranDefer     bool   // did the dead workflow's outstanding defer run
}

func (o reentryOutcome) String() string {
	return o.stoppedBy + ": reentered=" + b2s(o.reentered) +
		" ranGuestCode=" + b2s(o.ranGuestCode) + " ranDefer=" + b2s(o.ranDefer)
}

func b2s(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// probeReentry runs one entry point to its abnormal end, then grants fresh
// budget and calls the other entry point, reporting what happened.
//
// grantBudget is the caller's job because each limit is refreshed differently
// -- SetEpochDeadline for time, SetFuel for instructions -- and getting it
// wrong looks identical to "the instance is dead". That distinction is the
// whole measurement, so it is not hidden inside a helper.
func probeReentry(t *testing.T, rig *reentryRig, entryPoint string,
	grantBudget func(*reentryRig)) reentryOutcome {
	t.Helper()

	err := rig.runStart(t, entryPoint, json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("%s returned instead of being stopped, so no abnormal exit "+
			"happened and this arm measures nothing", entryPoint)
	}
	out := reentryOutcome{stoppedBy: err.Error()}

	rig.caller.calls = nil
	grantBudget(rig)

	if _, callErr := rig.callExport(t, "after_the_fence", `{}`); callErr != nil {
		return out
	}
	out.reentered = true
	out.ranGuestCode = rig.reachedHost()
	out.ranDefer = rig.sawOp("the_fenced_workflows_defer")
	return out
}

// TestReentryAfterInstructionLimit is the fuel arm.
//
// Fuel and epoch are both "the host stopped this guest", and both surface as a
// wasmtime trap, but they are different trap codes reached through different
// machinery -- so whether they leave the same wreckage is a question, not a
// deduction. Fuel is refreshed with SetFuel rather than SetEpochDeadline; using
// the wrong one would make a live instance look dead.
func TestReentryAfterInstructionLimit(t *testing.T) {
	rig := newReentryRig(t, fenceReentryWasm(t), 30*time.Second,
		WithWasmtimeInstructionLimit(200_000_000))

	out := probeReentry(t, rig, "spin_forever", func(r *reentryRig) {
		if err := r.store.SetFuel(10_000_000_000); err != nil {
			t.Fatalf("granting fresh fuel: %v", err)
		}
	})
	t.Logf("FINDING -- %s", out)

	if !out.reentered {
		t.Fatalf("a guest stopped by the instruction limit could not be re-entered.\n\n"+
			"The fence arm (TestAGoGuestSurvivesTheFence) CAN be, so phase 4 cannot "+
			"treat the two the same way. Stopped by: %s", out.stoppedBy)
	}
	if !out.ranGuestCode {
		t.Fatal("the re-entry call returned but never reached the host, so no guest " +
			"code ran")
	}
	if !out.ranDefer {
		t.Fatal("guest code ran but the dead workflow's outstanding defer did not, " +
			"so its closure table did not survive fuel exhaustion the way it " +
			"survives the fence")
	}
}

// TestReentryAfterMemoryLimit is the OOM arm, and it is the one least likely to
// behave like the others.
//
// A fence or a fuel trap stops the guest between instructions with its heap
// intact. Refusing a memory growth does not: the Go runtime asked for memory,
// did not get it, and reacts on its own terms -- which may be a trap, a fatal
// error, or a proc_exit, and which happens with the allocator halfway through
// whatever it was doing.
//
// Measured 2026-09-02 on Go 1.27: it is NOT a trap. The guest stops with
// "Exited with i32 exit status 2" -- proc_exit, from the Go runtime's fatal
// out-of-memory path, after dumping every goroutine to stderr. That makes this
// the only arm whose stop mechanism is not a wasmtime trap, and it still
// re-enters and still runs the dead workflow's defer.
//
// The assertions below deliberately cover the OUTCOME and not the mechanism.
// "proc_exit with status 2" is the Go runtime's own choice and a future version
// may make a different one; whether the instance is still usable afterwards is
// what phase 4 rests on. Asserting the string would turn a Go upgrade into a
// failure here that says nothing about cleat.
//
// The stderr dump is left visible on purpose. It is noisy, it is also the only
// direct evidence that the Go runtime hit OOM rather than something else
// stopping the guest, and silencing it would leave this arm unable to tell
// those apart.
func TestReentryAfterMemoryLimit(t *testing.T) {
	rig := newReentryRig(t, fenceReentryWasm(t), 30*time.Second,
		WithWasmtimeMemoryLimits(64<<20, -1, -1))

	out := probeReentry(t, rig, "allocate_forever", func(r *reentryRig) {
		r.store.SetEpochDeadline(600)
		// Raise the ceiling as well, and note why: re-entry needs scratch space
		// for the call's input and output buffers, and a guest that died of OOM
		// has by construction used up everything it was allowed. Without this
		// the harness fails while setting the call up -- "failed to grow memory
		// by 33" -- which is not a finding about re-entry at all. It is the
		// same shape as forgetting SetEpochDeadline in the fence arm: budget
		// the host controls, exhausted, looking like a dead instance.
		r.store.Limiter(512<<20, -1, -1, -1, -1)
	})
	t.Logf("FINDING -- %s", out)

	if !out.reentered {
		t.Fatalf("a guest killed by the memory limit could not be re-entered.\n\n"+
			"The fence and fuel arms can be, so phase 4 would need a separate answer "+
			"for OOM. Stopped by: %s", out.stoppedBy)
	}
	if !out.ranGuestCode {
		t.Fatal("the re-entry call returned without reaching the host. That is the " +
			"worst of the outcomes, because it looks like a working instance and is " +
			"not one: a phase 4 that checked only the error would run no cleanup and " +
			"report success.")
	}
	if !out.ranDefer {
		t.Fatal("guest code ran but the dead workflow's outstanding defer did not. " +
			"The Go runtime reacts to a refused memory growth with the allocator " +
			"mid-flight, so the closure table surviving is the part that was least " +
			"safe to assume and is exactly what this asserts.")
	}
}
