package wasm

import (
	"go/token"
	"sort"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/callgraph"
	"github.com/cleat-team/cleat/internal/closure"
)

// TestEachRewiredMethodWiresItsImport pins the fixes for IMPROVEMENT-PLAN
// 3.224: public Go SDK methods that compiled to no host import at all -- no
// error, no warning, a successful build and a workflow that could not do the
// thing. Rust, Java and AssemblyScript bound these; Go alone did not.
//
// The assertion is on AnalyzeUsage rather than on a compiled binary because
// that is the step that was wrong: cleat build wires imports by scanning the
// user's AST against hostFunctions, and the rows were missing. Compiling is
// covered by TestEveryAdapterDefCompiles, and the two together are the pair
// that matters -- a row without an importDefs entry passes this test and fails
// to compile, which is exactly what happened wiring the first of these.
//
// One fixture per method, each calling exactly one host call, so a failure
// names the method rather than whatever else a shared fixture happens to use.
func TestEachRewiredMethodWiresItsImport(t *testing.T) {
	cases := []struct {
		method string
		pkg    string
		imp    string
		why    string
	}{
		{
			method: "SignalWorkflow",
			pkg:    "github.com/cleat-team/cleat/testdata/signalworkflow",
			imp:    "cleat_signal_workflow",
			why: "the one signalling path the engine implements fully -- it really calls " +
				"DeliverSignal (engine/signaller.go) -- so this was the engine able to deliver a " +
				"signal between workflows and no Go guest able to ask for it",
		},
		{
			method: "ScheduleInvoke",
			pkg:    "github.com/cleat-team/cleat/testdata/scheduleinvoke",
			imp:    "cleat_schedule_invoke",
			why: "a one-way message scheduled after a delay; unwired, the SDK returned an error " +
				"blaming the caller for not being inside a workflow function, which is the one " +
				"cause that was not the cause",
		},
		{
			method: "DurableSend",
			pkg:    "github.com/cleat-team/cleat/testdata/durablesend",
			imp:    "cleat_send",
			why: "fire-and-forget delivery to a service; Python, Rust, Java and AssemblyScript " +
				"all expose it, and Go alone could not reach it -- which is what settled that it " +
				"was meant to be callable from a workflow rather than reserved for the host",
		},
		{
			method: "ResolvePromise",
			pkg:    "github.com/cleat-team/cleat/testdata/resolvepromise",
			imp:    "cleat_resolve_promise",
			why: "completing a promise another workflow is waiting on; unwired, a Go workflow " +
				"could create the wait and never satisfy it",
		},
		{
			method: "RejectPromise",
			pkg:    "github.com/cleat-team/cleat/testdata/rejectpromise",
			imp:    "cleat_reject_promise",
			why: "the failure half of ResolvePromise, and the half that matters more: unwired, " +
				"the waiter has no way to be told the thing it waits for will never arrive",
		},
		{
			method: "NowMs",
			pkg:    "github.com/cleat-team/cleat/testdata/nowms",
			imp:    "cleat_now",
			why: "NowMs invokes the `now` CLOSURE FIELD directly rather than calling h.Now(), so " +
				"TestEveryCompositeHostCallHasAnImportRow -- which scans for h.<Uppercase>( -- cannot see " +
				"it. Unwired it returned 0, an epoch timestamp, from a clock",
		},
	}

	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			fset := token.NewFileSet()
			result, err := analyzer.LoadPackages(tc.pkg, fset)
			if err != nil {
				t.Fatalf("LoadPackages(%s): %v", tc.pkg, err)
			}
			cg, err := callgraph.Build(result)
			if err != nil {
				t.Fatalf("callgraph.Build: %v", err)
			}
			usage := AnalyzeUsage(result, closure.Compute(result, cg))

			if !usage.Used[tc.imp] {
				var got []string
				for imp := range usage.Used {
					got = append(got, imp)
				}
				sort.Strings(got)
				t.Errorf("a workflow whose only host call is h.%s(...) wires no %s import; "+
					"it would compile and be unable to act.\n"+
					"  imports wired: %v\n"+
					"  why it matters: %s\n"+
					"Needs all four: an importDefs entry (wasm/generator.go), a hostFunctions row "+
					"(wasm/usage.go), an adapterDefs entry (wasm/adapter_metadata.go), and the SDK "+
					"HostCallsOptions field. See IMPROVEMENT-PLAN 3.224.",
					tc.method, tc.imp, got, tc.why)
			}
		})
	}
}
