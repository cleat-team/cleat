//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Does the host run the defers of a killed NON-Go guest?
//
// IMPROVEMENT-PLAN §3.35 phase 4. #550 wired runGuestDefersAfterKill into the
// Go-on-wasmtime branch of backend_wasmtime.go. #553, #557 and #558 then gave
// Rust, AssemblyScript and Java a __cleat_run_deferred export for the host to
// call -- but every non-Go guest leaves through the *direct-export* path, and
// this test exists to establish whether that path calls it.
//
// The measurement matters more than the assertion. "The guest exports it" and
// "the host calls it" are two different facts, and shipping the first while
// believing it implies the second is how a killed workflow's lock stays held
// with an export sitting right there that would have released it.
func TestTheHostRunsDefersOfAKilledAssemblyScriptWorkflow(t *testing.T) {
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

	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer wt.Close(ctx)

	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-as-killed"))

	_, _, _, _, _, err = eng.Execute(ctx, wasmBytes,
		"spin_forever", json.RawMessage(`{}`))

	// The workflow still fails, and for the reason it failed. Cleanup running
	// must not turn a killed workflow into a successful one.
	if err == nil {
		t.Fatal("a fenced AssemblyScript workflow was reported as succeeding")
	}
	t.Logf("kill error: %v", err)

	if !ranTheDefer(caller) {
		t.Fatalf("the fence killed the AssemblyScript workflow and its defer never "+
			"ran (calls: %v).\n\n"+
			"The module DOES export __cleat_run_deferred -- "+
			"TestAssemblyScriptExportsTheDeferRunner asserts it. So this is the "+
			"host end: runGuestDefersAfterKill is wired into the Go-on-wasmtime "+
			"branch only, and every non-Go guest leaves through the direct-export "+
			"path, which returns its error without a defer pass.",
			operationsCalled(caller))
	}
}

// TestTheHostRunsDefersOfAKilledJavaWorkflow is the arm that must not be
// inferred from the AssemblyScript one, even though both leave through the
// same direct-export path.
//
// §3.35 says so explicitly: whether a second export can be called on the same
// instance after a kill is "a property of the guest's runtime, not of
// wasmtime". A TeaVM module initialises a whole runtime through _start --
// shadow stack, fiber system, thread-local globals -- and an epoch interrupt
// stops it wherever it happens to be. AssemblyScript under --runtime stub has
// almost none of that state, so it is the easy case, and passing there is not
// evidence about Java.
func TestTheHostRunsDefersOfAKilledJavaWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Java WASM integration test in short mode")
	}

	wasmBytes, err := os.ReadFile(buildJavaWasm(t))
	if err != nil {
		t.Fatalf("read Java WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer wt.Close(ctx)

	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-java-killed"))

	_, _, _, _, _, err = eng.Execute(ctx, wasmBytes,
		"spin_forever", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("a fenced Java workflow was reported as succeeding")
	}
	t.Logf("kill error: %v", err)

	if !ranTheDefer(caller) {
		t.Fatalf("the fence killed the Java workflow and its defer never ran "+
			"(calls: %v).\n\n"+
			"If the AssemblyScript arm passes and this one does not, the host "+
			"wiring is right and the TeaVM runtime cannot serve an export after "+
			"an epoch interrupt -- which is a finding about Java, not a bug in "+
			"the defer pass.", operationsCalled(caller))
	}
}
