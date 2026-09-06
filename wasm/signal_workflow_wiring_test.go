package wasm

import (
	"go/token"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/callgraph"
	"github.com/cleat-team/cleat/internal/closure"
)

// TestSignalWorkflowWiresItsImport pins the fix for IMPROVEMENT-PLAN 3.224.
//
// A Go workflow calling h.SignalWorkflow(...) used to compile with no
// cleat_signal_workflow import at all -- no error, no warning, a successful
// build and a workflow that could not signal anything. Rust, Java and
// AssemblyScript all bound the import; Go alone did not. The engine half has
// always worked: SignalWorkflow is the one signalling path that really calls
// DeliverSignal (engine/signaller.go), so this was the engine able to deliver a
// signal between workflows and no Go guest able to ask for it.
//
// The assertion is on AnalyzeUsage rather than on a compiled binary because
// that is the step that was wrong -- cleat build wires imports by scanning the
// user's AST against hostFunctions, and the row was missing. Compiling is
// covered separately by TestEveryAdapterDefCompiles.
func TestSignalWorkflowWiresItsImport(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/signalworkflow", fset)
	if err != nil {
		t.Fatalf("LoadPackages: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("callgraph.Build: %v", err)
	}
	usage := AnalyzeUsage(result, closure.Compute(result, cg))

	if !usage.Used["cleat_signal_workflow"] {
		var got []string
		for imp := range usage.Used {
			got = append(got, imp)
		}
		t.Errorf("a workflow whose only host call is h.SignalWorkflow(...) wires no "+
			"cleat_signal_workflow import; it would compile and be unable to signal anything.\n"+
			"  imports wired: %v\n"+
			"Needs all four: an importDefs entry (wasm/generator.go), a hostFunctions row "+
			"(wasm/usage.go), an adapterDefs entry (wasm/adapter_metadata.go), and the SDK "+
			"HostCallsOptions field. See IMPROVEMENT-PLAN 3.224.", got)
	}
}
