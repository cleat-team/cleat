package closure

import (
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/callgraph"
)

// cleat#949: the determinism checks ran only on the durable closure, so a
// helper that makes no host call was counted by the analyzer without being
// checked by it -- while the workflow re-executes it on every replay.

const helperEscapePkg = "github.com/cleat-team/cleat/testdata/vet-checks/go/helper_escape"

func computeHelperEscape(t *testing.T) (*analyzer.AnalysisResult, *callgraph.Graph, *Result) {
	t.Helper()
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages(helperEscapePkg, fset)
	if err != nil {
		t.Fatalf("LoadPackages: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("callgraph.Build: %v", err)
	}
	return result, cg, Compute(result, cg)
}

func TestDeterminismChecksReachAHelperOutsideTheDurableClosure(t *testing.T) {
	_, _, cr := computeHelperEscape(t)

	const helper = helperEscapePkg + ".nonDeterministicHelper"
	errs := cr.Errors[helper]
	if len(errs) == 0 {
		t.Fatalf("nonDeterministicHelper produced no diagnostics.\n\n"+
			"It carries goroutines, channel send/receive/close, sync.Mutex and "+
			"sync.WaitGroup, and it is called by a durable entry point, so the "+
			"workflow re-executes it on every replay. Before cleat#949 this was "+
			"silent because the helper makes no host call and the determinism "+
			"checks ran only on the durable closure.\n"+
			"All errors seen: %v", cr.Errors)
	}

	// The specific codes, not just "some error" -- a single E001 would satisfy
	// a bare non-empty assertion while five other violations stayed invisible.
	want := []string{"E001", "E002", "E012", "E013"}
	got := map[string]bool{}
	for _, e := range errs {
		got[e.Code] = true
	}
	for _, code := range want {
		if !got[code] {
			t.Errorf("no %s reported for nonDeterministicHelper; got codes %v", code, got)
		}
	}
}

// TestTheEscapingHelperIsStillTaggedPure is the control, and it is what keeps
// this fix narrow.
//
// Pure means "reaches no host call". That is the right answer to the question
// closure analysis exists to ask -- which host functions to import -- and it is
// what codegen consumes. cleat#949 was not that the tag was wrong; it was that
// determinism was reading the tag to answer a different question. If this test
// ever fails, the fix has started changing the closure rather than widening the
// validation set, and codegen is downstream of that.
func TestTheEscapingHelperIsStillTaggedPure(t *testing.T) {
	_, _, cr := computeHelperEscape(t)

	const helper = helperEscapePkg + ".nonDeterministicHelper"
	if !cr.Pure[helper] {
		t.Errorf("nonDeterministicHelper is no longer tagged Pure.\n\n"+
			"It makes no host call, so Pure is correct and codegen depends on it. "+
			"cleat#949 widened which functions are VALIDATED, not which are in "+
			"the closure.\nleaves=%v closure=%v pure=%v",
			cr.DurableLeaves, cr.DurableClosure, cr.Pure)
	}
	if cr.DurableClosure[helper] {
		t.Errorf("nonDeterministicHelper was pulled into the durable closure")
	}
}

// TestDurableReachableIsStrictlyLargerThanTheDurableClosure proves the
// traversal does real work on ordinary packages, not only on the one package
// written to trip it.
//
// Without this, "no existing package's diagnostics changed" is equally
// consistent with the traversal being inert everywhere else -- which is the
// reading that flatters the change.
func TestDurableReachableIsStrictlyLargerThanTheDurableClosure(t *testing.T) {
	for _, pkg := range []string{"basic", "allhostcalls", "deferfunc"} {
		fset := token.NewFileSet()
		result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/"+pkg, fset)
		if err != nil {
			t.Fatalf("%s: LoadPackages: %v", pkg, err)
		}
		cg, err := callgraph.Build(result)
		if err != nil {
			t.Fatalf("%s: callgraph.Build: %v", pkg, err)
		}
		cr := Compute(result, cg)

		durable := len(cr.DurableLeaves) + len(cr.DurableClosure)
		reachable := len(durableReachable(result, cg, cr))
		if reachable <= durable {
			t.Errorf("%s: durableReachable returned %d names, durable closure has %d. "+
				"The determinism scope is not wider than the closure here, so the "+
				"traversal is checking nothing extra in this package.", pkg, reachable, durable)
		}
	}
}

// TestDurableReachableTerminatesOnRecursion pins the worklist against mutual
// recursion between helpers, which is ordinary in workflow code and would hang
// a naive recursive walk.
func TestDurableReachableTerminatesOnRecursion(t *testing.T) {
	result := &analyzer.AnalysisResult{Funcs: map[string]*analyzer.FuncDecl{}}
	cg := &callgraph.Graph{Calls: map[string]map[string]bool{
		"a": {"b": true},
		"b": {"c": true},
		"c": {"a": true, "b": true}, // cycle, and a self-referencing pair
	}}
	cr := &Result{
		DurableLeaves:  map[string]bool{"a": true},
		DurableClosure: map[string]bool{},
	}
	got := durableReachable(result, cg, cr)
	for _, name := range []string{"a", "b", "c"} {
		if !got[name] {
			t.Errorf("%q not reached; got %v", name, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("reached %d names, want exactly 3: %v", len(got), got)
	}
}

// TestTheHelperEscapeFixtureStillHasViolations guards the fixture itself.
//
// Every assertion above is about diagnostics from one testdata package. If
// someone "tidies" that file, the tests keep passing while checking nothing --
// the vacuous-pass shape this repo keeps meeting.
func TestTheHelperEscapeFixtureStillHasViolations(t *testing.T) {
	result, _, _ := computeHelperEscape(t)
	if _, ok := result.Funcs[helperEscapePkg+".nonDeterministicHelper"]; !ok {
		t.Fatal("nonDeterministicHelper is gone from the fixture; the tests above now assert nothing")
	}

	// Relative to this package's directory, which is where `go test` runs.
	src, err := os.ReadFile(filepath.Join(
		"..", "..", "testdata", "vet-checks", "go", "helper_escape", "main.go"))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	for _, needle := range []string{"sync.Mutex", "sync.WaitGroup", "go func", "close("} {
		if !strings.Contains(string(src), needle) {
			t.Errorf("fixture no longer contains %q, so a code asserted above is unreachable", needle)
		}
	}
}

// TestAPureEntryPointIsChecked is the half of cleat#949 that #964 left.
//
// #964 seeded the downward walk from the durable sets, which reaches a helper
// below a durable workflow. It does not reach a workflow that makes NO host
// call: that function is not durable, and it is not the callee of anything
// durable, so nothing enqueues it.
//
// That is the issue's first example, and the asymmetry it names:
//
//	no host call        -> "0 in cleat closure" -> built, no diagnostic
//	+ h.SetQueryState() -> "1 in cleat closure" -> two E013s, refused
//
// Here the unchecked body is not a helper somewhere below the workflow -- it is
// the workflow.
func TestAPureEntryPointIsChecked(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages(
		"github.com/cleat-team/cleat/testdata/vet-checks/go/e949_no_host_call", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph: %v", err)
	}
	cr := Compute(result, cg)

	if !hasErrorCode(cr, "E013") {
		t.Errorf("a workflow entry point whose only construct is a sync.Mutex produced %v, "+
			"want E013.\n\n"+
			"It makes no host call, so it is neither durable nor the callee of anything "+
			"durable -- seeding the walk from the durable sets alone never reaches it. An "+
			"entry point is by definition executed.", listErrorCodes(cr))
	}
}
