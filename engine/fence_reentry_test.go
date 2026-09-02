//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v44"
	"github.com/cleat-team/cleat/wasm"
)

// Can a real Go guest be re-entered after the fence stops it?
//
// IMPROVEMENT-PLAN 3.35 phase 4 is the workflows whose defers nothing runs. The
// guest runs its own defers when its entry point finishes (3.70), so the open
// cases are the ones where it never finishes: a trap, a timeout, and the
// execution fence.
//
// engine/fence_survivability_test.go established the first half of the fence
// case -- wasmtime leaves an epoch-interrupted instance callable, with its
// linear memory intact -- and named the boundary it did NOT cross: that module
// is hand-written WAT with no language runtime in it. A Go guest additionally
// has a Go runtime interrupted at an arbitrary instruction, with the scheduler,
// the garbage collector and the stack in whatever state the interrupt found
// them. Whether THAT can be re-entered is a different question, and it is the
// one phase 4 actually turns on, because every real workflow is a guest with a
// runtime in it.
//
// This file answers it, with the control the question requires.

// reentryRig is Execute's setup, stopped one step short.
//
// Execute owns its store and instance and drops both on return, which makes the
// question unaskable through it: "call the instance again" needs the instance.
// So the sequence below is Execute's own, in Execute's order -- configureStore,
// WASI, registerAllImports, Instantiate -- kept open. Every step that differs
// from backend_wasmtime.go is a step this measurement would be lying about.
type reentryRig struct {
	backend  *wasmtimeBackend
	store    *wasmtime.Store
	instance *wasmtime.Instance
	mem      *wasmtime.Memory
	caller   *mockCaller

	// The two cleat_complete outcomes, kept apart exactly as Execute keeps
	// them: a guest that stopped cleanly and said it failed is not a trap.
	completeResult *string
	completeErr    *string
}

func newReentryRig(t *testing.T, wasmBytes []byte, timeout time.Duration) *reentryRig {
	t.Helper()
	ctx := context.Background()

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })

	b, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(timeout))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { b.Close(ctx) })

	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, b),
		WithWorkflowID("wf-fence-reentry"))

	session := &execSession{
		engine:     eng,
		deferrals:  make(map[string]string),
		workflowID: "wf-fence-reentry",
		execRunID:  "wf-fence-reentry",
		nowMs:      1756800000000,
	}
	// The wasmtime host functions read b.handler directly and build their own
	// context.Background(), so this assignment -- not withHandler -- is what
	// connects the guest to the session on this backend.
	b.handler = session

	store := wasmtime.NewStore(b.engine)
	t.Cleanup(func() { store.Close() })
	if _, err := b.configureStore(ctx, store); err != nil {
		t.Fatalf("configureStore: %v", err)
	}

	b.envNeeded = wasm.NeededEnvImports(wasmBytes)
	needsWasi := wasm.HasWasiImports(wasmBytes)
	if needsWasi {
		wasiConfig := wasmtime.NewWasiConfig()
		wasiConfig.InheritStderr()
		store.SetWasi(wasiConfig)
	}

	module, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	t.Cleanup(func() { module.Close() })

	var completeResult, completeErr string
	linker := wasmtime.NewLinker(b.engine)
	if err := b.registerAllImports(linker, &completeResult, &completeErr,
		needsWasi, abortImportType(module)); err != nil {
		t.Fatalf("registerAllImports: %v", err)
	}

	instance, err := linker.Instantiate(store, module)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	memExport := instance.GetExport(store, "memory")
	if memExport == nil || memExport.Memory() == nil {
		t.Fatal("the fixture has no exported memory")
	}

	return &reentryRig{
		backend: b, store: store, instance: instance, mem: memExport.Memory(),
		caller: caller, completeResult: &completeResult, completeErr: &completeErr,
	}
}

// runStart drives the guest the way Execute drives a Go module: work is written
// to the fixed offset main() reads, then _start runs the whole workflow.
func (r *reentryRig) runStart(t *testing.T, entryPoint string, input json.RawMessage) error {
	t.Helper()
	startFn := r.instance.GetFunc(r.store, "_start")
	if startFn == nil {
		t.Fatal("the fixture does not export _start, so it is not a Go guest and " +
			"this whole file is measuring something else")
	}
	r.backend.workEntryPoint = entryPoint
	escaped, _ := json.Marshal(string(input))
	r.backend.workInput = []byte(fmt.Sprintf(`{"inputJSON":%s}`, string(escaped)))
	r.backend.writeWorkToFixedMemory(r.mem, r.store, entryPoint, []byte(input))

	_, err := startFn.Call(r.store)
	return err
}

// callExport invokes a generated //go:wasmexport entry point directly, with the
// same four arguments and the same scratch layout Execute uses for non-Go
// guests.
func (r *reentryRig) callExport(t *testing.T, name string, input string) (int64, error) {
	t.Helper()
	fn := r.instance.GetFunc(r.store, name)
	if fn == nil {
		t.Fatalf("no export %q -- codegen emits one //go:wasmexport per entry "+
			"point, so this means the fixture changed", name)
	}

	outBufSz := uint32(DefaultOutBufSize)
	currentSize := r.mem.DataSize(r.store)
	scratchBase, err := scratchBaseFor(uint64(currentSize), outBufSz)
	if err != nil {
		t.Fatalf("scratchBaseFor: %v", err)
	}
	inputOffset, outputOffset := scratchBase, scratchBase+outBufSz
	needed := uint64(outputOffset + outBufSz)
	if uint64(currentSize) < needed {
		pages := (needed - uint64(currentSize) + wasmPageSize - 1) / wasmPageSize
		if _, err := r.mem.Grow(r.store, pages); err != nil {
			t.Fatalf("growing guest memory: %v", err)
		}
	}
	copy(r.mem.UnsafeData(r.store)[inputOffset:], []byte(input))

	var res any
	var callErr error
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				callErr = fmt.Errorf("wasmtime panic calling %q: %v", name, rec)
			}
		}()
		res, callErr = fn.Call(r.store,
			int32(inputOffset), int32(len(input)), int32(outputOffset), int32(outBufSz))
	}()
	if callErr != nil {
		return 0, callErr
	}
	got, _ := res.(int64)
	return got, nil
}

// reachedHost reports whether the guest got all the way to the host. The
// fixture's second entry point makes exactly one DurableCall, and that call
// landing in the mock is the only evidence that guest code ran: an export can
// return a plausible int64 without executing a line of its body.
func (r *reentryRig) reachedHost() bool { return r.sawOp("after_the_fence") }

func (r *reentryRig) sawOp(op string) bool {
	for _, rec := range r.caller.calls {
		if rec.Op == op {
			return true
		}
	}
	return false
}

func fenceReentryWasm(t *testing.T) []byte {
	t.Helper()
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "fencereentry"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return wasmBytes
}

// TestTheHarnessCanCallAnExportDirectly is the control, and without it the
// measurement below is worthless.
//
// If a generated //go:wasmexport cannot be called directly at all -- wrong
// argument encoding, a Go runtime that refuses re-entry after main() has
// exited, a scratch layout the guest does not agree with -- then the fenced
// case failing says nothing about the fence. It says the harness does not work.
//
// So this arm runs a guest to completion the normal way and THEN calls the
// second entry point directly on the same instance. It is the same two-phase
// shape as the real measurement with the fence removed, which is the only way
// to attribute a difference to the fence.
func TestTheHarnessCanCallAnExportDirectly(t *testing.T) {
	rig := newReentryRig(t, fenceReentryWasm(t), 30*time.Second)

	// Phase 1: a normal, complete execution. Go's wasip1 runtime traps via
	// proc_exit when main() returns, so a non-nil error here is expected and
	// carries no information -- what matters is that the guest reported a
	// result through cleat_complete.
	_ = rig.runStart(t, "after_the_fence", json.RawMessage(`{}`))
	if *rig.completeErr != "" {
		t.Fatalf("the fixture failed on a normal run: %s", *rig.completeErr)
	}
	if !rig.reachedHost() {
		t.Fatal("the fixture did not reach the host even on a normal run, so the " +
			"rig is not wired to the session and nothing below is a measurement")
	}

	// Phase 2: call the export directly on the instance that just finished.
	rig.caller.calls = nil
	if _, err := rig.callExport(t, "after_the_fence", `{}`); err != nil {
		t.Fatalf("calling a generated export directly failed on a cleanly exited "+
			"instance: %v\n\n"+
			"This is the control. If the harness cannot do this WITHOUT the fence "+
			"involved, then TestAGoGuestSurvivesTheFence is not measuring the fence.", err)
	}
	if !rig.reachedHost() {
		t.Fatal("the direct export call returned without reaching the host, so the " +
			"guest body did not run. An export can return a plausible int64 having " +
			"executed nothing, which is exactly why this asserts on the host call.")
	}
}

// TestAGoGuestSurvivesTheFence is IMPROVEMENT-PLAN 3.35 phase 4's deciding
// measurement, and it comes out positive.
//
// Phase 1 registers a defer and then spins until the fence stops it, so the
// workflow dies with a cleanup outstanding -- phase 4's exact subject. Phase 2
// grants a fresh epoch budget (SetEpochDeadline is relative to the current
// epoch, so without it the next call is interrupted immediately) and calls the
// other entry point on the SAME instance.
//
// Three things hold, and the third is the one that decides phase 4:
//
//  1. the call succeeds -- a fenced Go guest is re-enterable, not just the
//     runtime-free WAT module of fence_survivability_test.go;
//  2. the guest reaches the host, so guest code genuinely ran;
//  3. the defer registered by the FENCED entry point runs, and reaches the
//     host -- so the closure table survived the interrupt, in the instance
//     that owns it.
//
// Point 3 needs no production change to observe, which is the surprise:
// codegen already emits _cleatRunDeferred at the end of every export (3.70), so
// any subsequent call into the instance drains the table. The mechanism phase 4
// needs already works. What is missing is the host deciding to make that call
// -- and a defer runner it can name, rather than borrowing an unrelated entry
// point the way this test does.
//
// This is a capability test, not a design. It says the fence case is buildable
// and pins the properties a build would rest on.
func TestAGoGuestSurvivesTheFence(t *testing.T) {
	rig := newReentryRig(t, fenceReentryWasm(t), 2*time.Second)

	began := time.Now()
	err := rig.runStart(t, "spin_forever", json.RawMessage(`{}`))
	fencedAfter := time.Since(began)

	if err == nil {
		t.Fatal("the spinning entry point returned instead of being interrupted, so " +
			"the fence did not fire and nothing below measures anything")
	}
	if limitErr := rig.backend.resourceLimitError(err, 2*time.Second); limitErr == nil {
		t.Fatalf("the guest stopped after %v, but not on a resource limit: %v\n\n"+
			"Something other than the fence ended this run -- most likely the guest "+
			"faulted before it reached the loop -- so the state the instance is in "+
			"is not the state phase 4 is about.", fencedAfter.Round(time.Millisecond), err)
	}
	t.Logf("the fence stopped the guest after %v", fencedAfter.Round(time.Millisecond))

	// The defer's body must not have run yet. It is registered through
	// cleat_defer, a host import, which does not go through the ServiceCaller
	// -- so a clean phase 1 records nothing at all here. If the body HAD run,
	// every assertion below would pass for the wrong reason.
	if got := operationsCalled(rig.caller); len(got) != 0 {
		t.Fatalf("the fenced entry point reached the host %v before it was stopped.\n\n"+
			"It registers a defer and then spins without entering the host, so this "+
			"means the defer body ran during phase 1 -- and the re-entry assertions "+
			"below would then be measuring phase 1's leftovers.", got)
	}
	rig.caller.calls = nil
	rig.store.SetEpochDeadline(600) // 30s at the 50ms tick; ample, and still bounded.

	if _, err := rig.callExport(t, "after_the_fence", `{}`); err != nil {
		t.Fatalf("a fenced Go guest could not be re-entered: %v\n\n"+
			"TestTheHarnessCanCallAnExportDirectly is the control: if that one passes "+
			"and this fails, the fence -- not the harness -- is what made the instance "+
			"unusable, and IMPROVEMENT-PLAN 3.35 phase 4's fence case needs a different "+
			"design from the one this test says is available.", err)
	}
	if !rig.reachedHost() {
		t.Fatal("the re-entry call returned but never reached the host, so no guest " +
			"code ran. A returned int64 is not evidence; the host call is.")
	}

	if !rig.sawOp("the_fenced_workflows_defer") {
		t.Fatalf("the instance was re-entered and guest code ran, but the defer the "+
			"FENCED entry point registered did not (host calls seen: %v).\n\n"+
			"This is the half phase 4 actually needs. Re-entry alone is not enough: "+
			"the closure table lives in the guest heap, and if the interrupt left it "+
			"unreadable then the defers of a fenced workflow are unreachable even "+
			"though the instance answers.", operationsCalled(rig.caller))
	}
}
