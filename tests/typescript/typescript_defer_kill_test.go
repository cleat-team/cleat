package typescript

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// A KILLED TypeScript workflow does NOT run the defers it registered, and that is a limit, not a design (cleat#3826).
//
// This is the second path to __cleat_run_deferred. The defer segment's drain is one caller; the host's cleanup after it
// kills a guest (instruction limit, fence, out of memory) is the other. Both look the export up by name, and both did
// nothing for a JavaScript module until the module exported one -- so giving the JavaScript guest the export changed this
// path too, without anyone asking for it by name. What it changed to is measured here, not assumed:
//
//   - the host finds the export and calls it (the log reads "a killed workflow's defers could not be run", where before
//     it was silent because there was nothing to call);
//   - the call TRAPS (`unreachable`), so no defer body runs and the ServiceCaller records nothing;
//   - the kill is still reported as the kill: the drain's failure does not replace or mask the instruction-limit error.
//
// THE CAUSE IS MEASURED (2026-11-16, local run with the bootstrapped javy 9.1.0 toolchain), and the trap is a Rust panic,
// not a silent refusal. The guest prints, immediately before the warning below:
//
//	thread '<unnamed>' (1) panicked at rquickjs-core-0.12.2/src/safe_ref.rs:35:20:
//	RefCell already borrowed
//
// So rquickjs's own borrow bookkeeping is still outstanding when the drain enters. That is what a fuel trap leaves: it
// aborts the guest wherever it happens to be, so a RefCell guard live at that instant never runs its Drop. It is not
// budget starvation -- runGuestDefersAfterKill refunds the fuel (store.SetFuel) before calling -- and it is not a broken
// drain, because the defer segment's drain works on this same toolchain: there the guest returned normally through the
// suspend sentinel and nothing is left borrowed. The damage is to the killed instance, and a second entry cannot repair it.
//
// What that means for a reader: "no defers run after a kill" is a limit of the instance, not of the export, and it is
// documented as such in packages/cleat-ts/DIVERGENCES.md.
//
// WHEN THIS FAILS because the defers now run, the limit has been fixed: change the assertion to the cleanup call, delete
// the limit from packages/cleat-ts/DIVERGENCES.md, and say so in the PR.
func TestTypeScriptKilledWorkflowDoesNotRunItsDefersYet(t *testing.T) {
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
	backendLogs := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
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
	// The drain ran and could not complete. This is what the export's arrival CHANGED for the kill path: before it, the
	// host found nothing to call and said nothing at all. If this assertion starts failing because the message is gone,
	// the drain is no longer being attempted on a killed guest -- which is a silent regression in the direction this test
	// exists to catch.
	if !strings.Contains(logs.String(), "a killed workflow's defers could not be run") {
		t.Fatalf("the post-kill drain did not report a failure, so either it was not attempted or it succeeded.\n\n"+
			"Want the warning \"a killed workflow's defers could not be run\". Logs:\n%s", logs.String())
	}
	if got := caller.snapshot(); len(got) != 0 {
		t.Fatalf("a killed workflow recorded %v. The defers ran after a kill, which this test says does not happen yet.\n\n"+
			"That is the limit being FIXED, not broken: see the comment above this test for what to change.", got)
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
