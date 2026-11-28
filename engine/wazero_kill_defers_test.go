package engine

import (
	"context"
	"encoding/json"
	"testing"
)

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
