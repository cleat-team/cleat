package engine

import (
	"context"
	"os"
	"sort"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// TestAssemblyScriptWorkflowDefersRun is IMPROVEMENT-PLAN §3.73 for
// AssemblyScript, the fourth and last SDK.
//
// The AS SDK's HostCalls.defer registers a DESCRIPTION. The host recorded it
// and nothing anywhere ran it -- while the SDK's own doc comment said it
// registers "cleanup to run on workflow exit". That is §3.70's defect in a
// fourth language, and this is the regression test for the fix: deferFunc
// takes a top-level function reference plus a payload, and the transformer's
// generated wrapper drains the table when the entry point returns.
//
// It asserts the full sequence rather than mere presence. "The defers ran" is
// satisfied by running them in registration order, which is wrong in the way
// that matters: a defer releases what the defer before it acquired, so FIFO
// unwinds the workflow inside-out.
func TestAssemblyScriptWorkflowDefersRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AssemblyScript WASM integration test in short mode")
	}

	wasmPath := buildAssemblyScriptWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read AS WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	eng := NewEngine(rt, caller, WithWorkflowID("wf-as-defer"))

	input := []byte(`{"userID":"u1"}`)
	if _, _, suspended, _, _, err := eng.Execute(ctx, wasmBytes, "defer_order", input); err != nil {
		t.Fatalf("defer_order: %v", err)
	} else if suspended != nil {
		t.Fatalf("defer_order suspended unexpectedly: %v", suspended.Reason)
	}

	got := operationsCalled(caller)
	want := []string{"body", "second", "first"}
	if len(got) != len(want) {
		t.Fatalf("recorded %d calls %v, want %d %v.\n\n"+
			"Two of these come from defer bodies. If only \"body\" is present the "+
			"defers did not run at all -- the state every AssemblyScript workflow "+
			"was in before §3.73, while the SDK documented cleanup that runs.",
			len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d is %q, want %q (full sequence %v, want %v).\n\n"+
				"Order is not cosmetic: defers unwind, so the last one registered "+
				"must run first.", i, got[i], want[i], got, want)
		}
	}
}

// TestAssemblyScriptDefersDoNotRunOnSuspension is the other half of §3.73, and
// in this SDK it is the half that can actually break.
//
// A suspended workflow has not exited. Its defers are still pending, and
// firing them at the first sleep would release a lock the workflow is about to
// come back and use.
//
// The other three SDKs get that for free: they suspend by unwinding -- a
// panic, a raise, a thrown SuspendSignal -- so the drain sits on a path the
// unwind skips, and there is no ordering to get wrong. AssemblyScript builds
// with --runtime stub and has no exceptions, so suspension is a global flag,
// the entry point returns normally either way, and the generated wrapper's
// `if (isWorkflowSuspended()) return SUSPEND_SENTINEL;` is the only thing
// standing between a sleeping workflow and its cleanup. Emitting the drain one
// line earlier compiles and passes every other test in this file.
func TestAssemblyScriptDefersDoNotRunOnSuspension(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AssemblyScript WASM integration test in short mode")
	}

	wasmPath := buildAssemblyScriptWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read AS WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	eng := NewEngine(rt, caller, WithWorkflowID("wf-as-defer-suspend"))

	input := []byte(`{"userID":"u1"}`)
	_, _, suspended, _, _, err := eng.Execute(ctx, wasmBytes, "defer_suspend", input)
	if err != nil {
		t.Fatalf("defer_suspend: %v", err)
	}

	// Guard the assertion below on the workflow having actually suspended. If
	// the sleep ever stops suspending -- a replay path, a changed host
	// function -- the "no calls were made" check underneath would still pass,
	// and would be measuring nothing at all.
	if suspended == nil {
		t.Fatalf("defer_suspend did not suspend, so this test proves nothing " +
			"about the suspend path; it sleeps 60s on a fresh execution and the " +
			"host is expected to report a suspension")
	}

	if got := operationsCalled(caller); len(got) != 0 {
		t.Fatalf("the workflow suspended but %d call(s) were made: %v.\n\n"+
			"A suspended workflow has not exited. Its defer bodies are still "+
			"pending, and running them here releases what the workflow is about "+
			"to come back and use. Check that the generated wrapper drains AFTER "+
			"the isWorkflowSuspended() check, not before.", len(got), got)
	}
}

// TestAssemblyScriptExportsTheDeferRunner is §3.35 phase 4 for AssemblyScript,
// the piece the guest-side drain does not cover.
//
// The generated wrapper drains the defer table when a workflow RETURNS. A
// workflow the host killed -- execution fence, instruction limit, memory
// ceiling -- never reaches it, so its cleanup would simply never happen: the
// lock stays held, the charge stays uncompensated.
//
// `runGuestDefersAfterKill` (engine/backend_wasmtime.go) looks the export up by
// name on the killed instance. Before this, that lookup returned nil for every
// non-Go guest and the host's kill-path cleanup silently did nothing.
//
// The name is asserted against the host's own constant rather than a literal.
// A test with the string written out twice passes while the two halves drift
// apart, which is exactly the failure it exists to catch.
func TestAssemblyScriptExportsTheDeferRunner(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AssemblyScript WASM integration test in short mode")
	}

	wasmBytes, err := os.ReadFile(buildAssemblyScriptWasm(t))
	if err != nil {
		t.Fatalf("read AS WASM: %v", err)
	}

	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)

	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	fn, ok := compiled.ExportedFunctions()[deferRunnerExport]
	if !ok {
		var names []string
		for n := range compiled.ExportedFunctions() {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("the AssemblyScript module exports no %q; it exports %v.\n\n"+
			"The transformer emits it once per module that has an entry point. "+
			"Without it the host has no way to run a killed workflow's defers, "+
			"and runGuestDefersAfterKill returns without doing anything.",
			deferRunnerExport, names)
	}

	// Signature, not just presence. The host calls this with no arguments and
	// reads an i64 count; a wrapper emitted with the entry-point signature
	// would be found by name and then fail at the call.
	if got := len(fn.ParamTypes()); got != 0 {
		t.Errorf("%s takes %d parameter(s), want 0 -- the host calls it with none",
			deferRunnerExport, got)
	}
	if got := fn.ResultTypes(); len(got) != 1 || got[0] != api.ValueTypeI64 {
		t.Errorf("%s returns %v, want one i64 (how many bodies ran)",
			deferRunnerExport, got)
	}
}
