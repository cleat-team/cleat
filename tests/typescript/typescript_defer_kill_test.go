package typescript

import (
	"context"
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
// The cause of the trap is NOT established. The working hypothesis is QuickJS's interpreter state after a fuel trap in the
// middle of execution, which a second entry cannot recover from; the clean-suspension drain beside it works, which says
// the drain is sound and the instance is what is damaged. Do not cite the hypothesis as a finding.
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
	wt, err := engine.NewWasmtimeBackend(ctx, engine.WithWasmtimeInstructionLimit(200_000_000))
	if err != nil {
		t.Fatalf("engine.NewWasmtimeBackend: %v", err)
	}
	caller := &recordingCaller{}
	e := engine.NewEngine(rt, caller,
		engine.WithWorkflowID("wf-ts-defer-kill"),
		engine.WithDefName("defer_then_spin"),
		engine.WithDefVersion(1),
		engine.WithBackends(engine.WasmtimeLanguages, wt),
	)

	_, _, _, _, _, err = e.Execute(ctx, wasmBytes, "defer_then_spin", []byte(`{}`))
	if err == nil {
		t.Fatal("the spinning workflow returned without error: the instruction limit did not kill it, so this test measured nothing")
	}
	if !strings.Contains(err.Error(), "instruction limit") {
		t.Fatalf("the workflow was stopped, but not reported as an instruction-limit kill: %v\n\nThe post-kill drain must not replace the error it follows.", err)
	}
	if got := caller.snapshot(); len(got) != 0 {
		t.Fatalf("a killed workflow recorded %v. The defers ran after a kill, which this test says does not happen yet.\n\n"+
			"That is the limit being FIXED, not broken: see the comment above this test for what to change.", got)
	}
}
