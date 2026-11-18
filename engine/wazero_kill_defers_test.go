package engine

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// Does the wazero path run the defers of a workflow that trapped?
//
// IMPROVEMENT-PLAN §3.35 phase 4. The wasmtime backend does, for Go (#550) and
// for every other guest (#559). This is the other execution path -- reached
// when no backend is registered at all, which is `cleatctl replay|debug`,
// `cleat run`, `cleat-bench`, and the public testing packages
// `cleat/wasmtest`, `cleat/cleattest` and `cleat/embedded`.
//
// The testing packages are why this matters rather than why it does not.
// executor.go's own comment records a user who "saw a compensating defer fire
// twice under the harness and once in production, so the harness disagreed
// with the runtime in the direction that makes a real double-compensation look
// like a test artifact". A harness that runs no defers where production runs
// them is the same disagreement pointing the other way.
//
// The fixture traps rather than spinning because the fence is not available
// here: wazero cannot interrupt a compute-bound guest at all, measured three
// ways (CLAUDE.md). An explicit trap is the one abnormal exit both runtimes
// can produce.
func TestTheWazeroPathRunsDefersOfATrappedWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AssemblyScript WASM integration test in short mode")
	}

	wasmBytes, err := os.ReadFile(buildAssemblyScriptWasm(t))
	if err != nil {
		t.Fatalf("read AS WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// No backends registered on purpose: that is what selects executeCompiled,
	// the wazero path this test is about.
	caller := &mockCaller{}
	eng := NewEngine(rt, caller, WithWorkflowID("wf-wazero-trapped"))

	_, _, _, _, _, err = eng.Execute(ctx, wasmBytes,
		"trap_after_defer", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("a trapped workflow was reported as succeeding")
	}
	t.Logf("trap error: %v", err)

	if !ranTheDefer(caller) {
		t.Fatalf("the workflow trapped and its defer never ran (calls: %v).\n\n"+
			"invokeDefersOnTrap looks for an export named cleat_defer_<id>, which "+
			"no guest in any language has ever had -- `grep -rn cleat_defer_` "+
			"finds consumers and no producers. The export that does exist is "+
			"__cleat_run_deferred.", operationsCalled(caller))
	}
}

// trapAfterADeferWat is what an SDK-built guest looks like to the wazero path, reduced to the three facts the path depends on: an
// entry point that registers a defer and then TRAPS, no per-defer `cleat_defer_<id>` export (no SDK emits one), and the
// `__cleat_run_deferred` export every core-module SDK emits, which makes one durable call so the host's mock caller can see that the
// cleanup was run. It is the WAT arm of TestTheWazeroPathRunsDefersOfATrappedWorkflow, added beside the AssemblyScript one so the
// behaviour is asserted without an AssemblyScript toolchain (cleat#4023), and without any toolchain at all: wazero cannot interrupt
// a compute-bound guest, and a Go guest's panic is recovered by the generated dispatcher, so an explicit trap is only reachable here
// through a module written for it.
const trapAfterADeferWat = `(module
  (import "env" "cleat_defer" (func $defer (param i32 i32 i32 i32) (result i64)))
  (import "env" "cleat_call" (func $call (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)))
  (memory (export "memory") 1)
  (data (i32.const 1024) "release the lock")
  (data (i32.const 1088) "svc")
  (data (i32.const 1152) "the_wat_guests_defer")
  (data (i32.const 1216) "{}")
  (func (export "trap_after_defer") (param i32 i32 i32 i32) (result i64)
    (drop (call $defer (i32.const 1024) (i32.const 16) (i32.const 2048) (i32.const 64)))
    unreachable)
  (func (export "__cleat_run_deferred") (result i64)
    (drop (call $call
      (i32.const 1088) (i32.const 3)
      (i32.const 1152) (i32.const 20)
      (i32.const 1216) (i32.const 2)
      (i32.const 4096) (i32.const 256)))
    (i64.const 0))
)`

func TestTheWazeroPathRunsDefersOfATrappedGuestThatExportsTheRunner(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// No backends registered on purpose: that is what selects executeCompiled, the wazero path this test is about.
	caller := &mockCaller{}
	eng := NewEngine(rt, caller, WithWorkflowID("wf-wazero-trapped-wat"))

	_, _, _, _, _, err = eng.Execute(ctx, mustWat2Wasm(t, trapAfterADeferWat), "trap_after_defer", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("a trapped workflow was reported as succeeding")
	}
	t.Logf("trap error: %v", err)

	ran := false
	for _, op := range operationsCalled(caller) {
		if op == "the_wat_guests_defer" {
			ran = true
		}
	}
	if !ran {
		t.Fatalf("the workflow trapped and its defer never ran (calls: %v).\n\n"+
			"The guest exports __cleat_run_deferred and no cleat_defer_<id>, which is every SDK-built guest. The wazero path has "+
			"to reach the runner export, not only look for the legacy per-defer one.", operationsCalled(caller))
	}
}
