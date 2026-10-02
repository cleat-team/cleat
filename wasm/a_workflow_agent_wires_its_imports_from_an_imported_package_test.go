package wasm

import (
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/callgraph"
	"github.com/cleat-team/cleat/internal/closure"
)

// The agent workflow's five host calls must be wired by cleat/agentworkflow's
// //cleat:require directive. cleat#1983.
//
// WHY THIS TEST EXISTS AT ALL, given that the loop has its own tests through
// cleattest. Because cleattest CANNOT see this. It constructs a fully populated
// HostCalls, so h.PluginCall works in every host-side test whether or not the
// compiled module imports plugin_call -- and cleat#2951 shipped exactly that
// bug one file over, green in `cleat`, `examples/order-lifecycle` and
// `internal/analyzer` alike, and caught only by this package's helper-import
// test. The failure is silent and late: the module builds, `cleat build` exits
// 0, and the workflow dies on its first LLM turn with "the HostCalls runtime
// was not initialized".
//
// WHY THE SDK HELPER TABLE CANNOT COVER IT (mirroring the dagrun test's note):
// SDKDurableHelper accepts only receivers whose package is named "cleat", and
// the helper package here is named agentworkflow. A row in sdkHelperImports
// would be written and never consulted.
func TestAWorkflowAgentWiresItsImportsFromAnImportedPackage(t *testing.T) {
	const pkg = "github.com/cleat-team/cleat/testdata/agentguest"

	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages(pkg, fset)
	if err != nil {
		t.Fatalf("LoadPackages(%s): %v", pkg, err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("callgraph.Build: %v", err)
	}
	usage := AnalyzeUsage(result, closure.Compute(result, cg))

	var got []string
	for imp := range usage.Used {
		got = append(got, imp)
	}
	sort.Strings(got)

	// THE CONTROL, and it is load-bearing for the same reason dagguest's is.
	// The fixture calls h.NowMs() itself, so cleat_now is wired by the
	// ordinary scan of the workflow's own source. If it is absent, the build
	// is broken for a cause that has nothing to do with the directive, and the
	// assertions below would fail for THAT reason -- reporting a directive bug
	// that is not there.
	if !usage.Used["cleat_now"] {
		t.Fatalf("the fixture's own h.NowMs() did not wire cleat_now, so this run says "+
			"nothing about the agentworkflow directive.\n  imports wired: %v", got)
	}

	// Exactly the five the directive names. Every one of them is a call the
	// WORKFLOW never writes -- it writes only agentworkflow.Run(h, input) --
	// so if any is missing, the directive is being read from the wrong place
	// and the deployed module will fail at run time on the step that needs it.
	for _, imp := range []string{
		"cleat_await_child", // AwaitChild, for a workflow tool
		"cleat_child_workflow",
		"cleat_call",      // DurableCall, for a service tool
		"plugin_call",     // PluginCall, for the LLM turn and a plugin tool
		"set_query_state", // SetQueryState, for progress
	} {
		if usage.Used[imp] {
			continue
		}
		t.Errorf("a workflow whose only route is cleat/agentworkflow does not wire %s.\n"+
			"  imports wired: %v\n\n"+
			"cleat/agentworkflow declares these with //cleat:require. If the directive is "+
			"read only from the workflow's own package, this is cleat#1617's defect again: "+
			"the module builds, exits 0 with no warning, and the agent fails on its first "+
			"LLM turn with \"the HostCalls runtime was not initialized\".", imp, got)
	}
}

// Every name in the agent directive must be a real HostCallsOptions field,
// because an unrecognised one is SILENTLY SKIPPED: collectRequirements is
// `if importName, ok := fieldToImport[field]; ok`, with no else. A typo wires
// nothing and nothing complains.
//
// READ FROM THE SOURCE, not from a list copied into this file. The first
// version of this test carried its own []string{"AwaitChild", ...} and checked
// THAT against hostFunctions -- so it verified the file agreed with itself and
// could not fail when the directive was misspelled. Measured 2026-10-02:
// changing the directive to `AwaitChildren,ChildWorkflow,...` left it green.
// A test of the artifact has to read the artifact.
func TestEveryNameInTheAgentDirectiveIsARealHostFunction(t *testing.T) {
	const src = "../cleat/agentworkflow/agentworkflow.go"

	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading the directive's source: %v", err)
	}
	names := directiveNames(string(body), "cleat:require ")
	if len(names) == 0 {
		t.Fatalf("no //cleat:require directive found in %s, so this test has lost its "+
			"subject -- and the fixture in the sibling test would be checking nothing", src)
	}

	byField := map[string]bool{}
	for _, hf := range hostFunctions {
		byField[hf.FieldName] = true
	}
	for _, field := range names {
		if !byField[field] {
			t.Errorf("the directive in %s names %q, which is not a HostCallsOptions field in "+
				"hostFunctions -- collectRequirements skips an unrecognised name in silence, so "+
				"this wires nothing and no other check would notice", src, field)
		}
	}
}

// directiveNames returns the comma-separated names on the first directive line
// carrying the given prefix.
func directiveNames(src, prefix string) []string {
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "//"+prefix) {
			continue
		}
		var out []string
		for _, n := range strings.Split(strings.TrimPrefix(line, "//"+prefix), ",") {
			if n = strings.TrimSpace(n); n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	return nil
}
