package typescript

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// A KILLED TypeScript workflow RUNS the defers it registered (cleat#3922).
//
// This is the second path to __cleat_run_deferred. The defer segment's drain is one caller; the host's cleanup after it
// kills a guest is the other. Until cleat#3922 the second path ran nothing: the kill was a wasmtime trap at whatever
// instruction the guest happened to be on, rquickjs's RefCell guards never ran their Drop, and the drain panicked
// ("RefCell already borrowed") and trapped, so no defer body ran. That was DIVERGENCES.md D1.
//
// The fix is a SOFT kill. cleat_interrupt_requested is a host function the guest's QuickJS interrupt handler polls; the
// host answers 1 when the budget is nearly spent, QuickJS raises an exception user code cannot catch, and the Rust call
// frames RETURN rather than being abandoned -- so nothing is left borrowed when the host drains.
//
// What is asserted here, all of it measured rather than assumed:
//   - the workflow still ends on the kill it always ended on, and the error still names it;
//   - the defer's body runs, so the ServiceCaller records its call;
//   - the log says "ran the defers of a killed workflow" rather than "could not be run".
//
// The inverse is pinned by TestTypeScriptAKillTheGuestCannotObserveStillLosesItsDefers, which is the residue: a guest
// whose work never returns to the QuickJS bytecode loop is still reached by the hard trap.
func TestTypeScriptKilledWorkflowRunsItsDefers(t *testing.T) {
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/defer_then_spin.ts"
	res := build(t, root, src, "defer_then_spin")
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasmBytes, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("engine.NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(ctx) })
	// The log line is the only place the trap is visible: the workflow's own error is the kill, by design, so nothing
	// else distinguishes "the drain trapped" from "the drain found nothing to call". Asserted rather than quoted in
	// the comment above, because a comment cannot fail when the behaviour changes (cleat#3826).
	//
	// The backend needs its OWN logger. Engine.WithLogger does not reach these records -- the warning is emitted by
	// runGuestDefersAfterKill's b.log(), which falls back to slog.Default() when WithWasmtimeLogger is absent, and that
	// is how the first version of this test failed while the warning was printing to stderr beside it.
	var logs strings.Builder
	backendLogs := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	wt, err := engine.NewWasmtimeBackend(ctx,
		engine.WithWasmtimeInstructionLimit(200_000_000),
		engine.WithWasmtimeLogger(backendLogs))
	if err != nil {
		t.Fatalf("engine.NewWasmtimeBackend: %v", err)
	}
	caller := &recordingCaller{}
	e := engine.NewEngine(rt, caller,
		engine.WithWorkflowID("wf-ts-defer-kill"),
		engine.WithDefName("defer_then_spin"),
		engine.WithDefVersion(1),
		engine.WithBackends(engine.WasmtimeLanguages, wt),
		engine.WithLogger(backendLogs),
	)

	_, _, _, _, _, err = e.Execute(ctx, wasmBytes, "defer_then_spin", []byte(`{}`))
	if err == nil {
		t.Fatal("the spinning workflow returned without error: the instruction limit did not kill it, so this test measured nothing")
	}
	if !strings.Contains(err.Error(), "instruction limit") {
		t.Fatalf("the workflow was stopped, but not reported as an instruction-limit kill: %v\n\nThe post-kill drain must not replace the error it follows.", err)
	}
	// THE ASSERTION THE FIX EXISTS FOR. Before cleat#3922 this was empty and the drain logged a failure.
	if got := caller.snapshot(); len(got) != 1 || got[0] != "inventory/cleanup" {
		t.Fatalf("a killed workflow recorded %v, want exactly one inventory/cleanup from its defer.\n\n"+
			"Empty means the soft kill did not reach the guest (no instruction limit, or the interrupt margin is too\n"+
			"small) and the hard trap left the instance borrowed.", got)
	}
	if strings.Contains(logs.String(), "a killed workflow's defers could not be run") {
		t.Fatalf("the drain reported a failure although the defer ran. Logs:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "ran the defers of a killed workflow") {
		t.Fatalf("the drain did not report running the defers. Logs:\n%s", logs.String())
	}

	// The damaged instance must not outlive the execution that damaged it. "No second entry repairs it" is only a
	// limitation rather than a hazard if the second entry is never THIS instance -- so run another workflow on the same
	// engine and require it to work.
	//
	// The code says it cannot be reused: Execute creates a store per call (`store := wasmtime.NewStore(b.engine)` with
	// `defer store.Close()`) and instantiates into that store (`linker.Instantiate`), and nothing pools or caches an
	// instance -- only compiled MODULES are cached, and a compiled module carries no guest state. That is the citation;
	// this is the check, because a citation cannot fail when the code changes.
	second := build(t, root, "tests/typescript/testdata/workflows/answer.ts", "answer")
	if second.exit != 0 {
		t.Fatalf("building answer.ts failed (exit %d):\n%s", second.exit, second.output)
	}
	secondBytes, err := os.ReadFile(second.module)
	if err != nil {
		t.Fatal(err)
	}
	before := len(caller.snapshot())
	_, _, susp, _, _, err := e.Execute(ctx, secondBytes, "answer", []byte(`{}`))
	if err != nil {
		t.Fatalf("a second workflow on the same engine failed after a killed one: %v\n\n"+
			"If this is a runtime-state error, a poisoned instance is being REUSED and the kill limit is a hazard, not "+
			"a limitation. Execute is supposed to instantiate into a fresh store per call.", err)
	}
	if susp != nil {
		t.Fatalf("the second workflow suspended (%s); it should have run to an outcome", susp.Reason)
	}
	after := caller.snapshot()
	if len(after) != before+1 || after[before] != "payments/charge" {
		t.Fatalf("the workflow after the kill recorded %v, want exactly one more entry, \"payments/charge\".\n\n"+
			"An empty tail means the second workflow did not run its body; anything else means it ran on state left "+
			"by the killed one.", after[before:])
	}
}
