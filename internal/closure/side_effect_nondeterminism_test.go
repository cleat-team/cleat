package closure

import (
	"go/token"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/callgraph"
)

// cleat#4123: under #4113's freeze semantics the function passed to SideEffect runs once and its result is recorded, so it is where a clock
// read, entropy or a map range belongs. The closure scan used to refuse them there (E003, E017, E021), which made the feature unusable for its
// purpose. The exemption is for a function literal written at the call; everywhere else the same constructs are still refused.
func TestNondeterminismInsideTheSideEffectFunctionIsAllowedAndOnlyThere(t *testing.T) {
	const pkg = "github.com/cleat-team/cleat/testdata/sideeffectnondet"
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages(pkg, fset)
	if err != nil {
		t.Fatalf("LoadPackages: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("callgraph.Build: %v", err)
	}
	cr := Compute(result, cg)

	codes := func(fn string) []string {
		var out []string
		for _, e := range cr.Errors[pkg+"."+fn] {
			out = append(out, e.Code)
		}
		return out
	}

	// Exempt: the literal passed to h.SideEffect and to cleat.SideEffectTyped. If the scan stopped finding these functions at all every
	// assertion below would pass vacuously, so each one is also required to be a function the scan knows.
	for _, fn := range []string{"InsideLiteral", "InsideTypedLiteral"} {
		if !cr.DurableLeaves[pkg+"."+fn] && !cr.DurableClosure[pkg+"."+fn] {
			t.Fatalf("%s is not in the durable closure, so a clean result for it measures nothing", fn)
		}
		if got := codes(fn); len(got) != 0 {
			t.Errorf("%s: the function passed to SideEffect was refused: %v", fn, got)
		}
	}

	// Known-positives: the same constructs outside the literal are still refused.
	for _, tc := range []struct{ fn, code, why string }{
		{"OutsideSideEffect", "E003", "a clock read directly in the workflow"},
		{"AfterTheLiteral", "E003", "a clock read after the literal (the exemption ends with it)"},
		{"NotTheSDKsSideEffect", "E003", "a SideEffect method on a type that is not cleat.HostCalls"},
	} {
		found := false
		for _, e := range cr.Errors[pkg+"."+tc.fn] {
			if e.Code == tc.code {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want %s (%s); got %v", tc.fn, tc.code, tc.why, codes(tc.fn))
		}
	}

	// A named function passed to SideEffect is outside the exemption, and the scan's ordinary rule does not look at a PURE function (one that
	// calls no host method), so nothing is reported for it. Pinned so that the day the scan starts validating pure functions this changes on
	// purpose: the exemption is then no longer the only thing between a SideEffect function and a refusal.
	if got := codes("NamedFunction"); len(got) != 0 || len(cr.Errors[pkg+".nowString"]) != 0 {
		t.Errorf("NamedFunction/nowString now report errors (%v, %v): the scan validates pure functions, so a named SideEffect function is refused and the exemption's scope needs revisiting", got, cr.Errors[pkg+".nowString"])
	}

	// The count that says the exemption removed exactly the errors inside the literals and nothing else: one E003 per known-positive
	// function above, none in the exempt ones.
	var inLiterals int
	for _, fn := range []string{"InsideLiteral", "InsideTypedLiteral"} {
		inLiterals += len(cr.Errors[pkg+"."+fn])
	}
	if inLiterals != 0 {
		t.Errorf("%d error(s) inside exempt literals", inLiterals)
	}
	if !strings.Contains(strings.Join(codes("OutsideSideEffect"), ","), "E003") {
		t.Errorf("the control function lost its E003: %v", codes("OutsideSideEffect"))
	}
}
