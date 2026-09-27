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
