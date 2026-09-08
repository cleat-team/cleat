package closure

import (
	"go/token"
	"sort"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/callgraph"
)

// cleat#965: Errors and Warnings are keyed by function name, and Go randomises
// map iteration, so every consumer that ranged over them directly produced a
// different order on every run -- human output, GitHub Actions annotations,
// JSON, and the summary.

func computeVetPkg(t *testing.T, pkg string) *Result {
	t.Helper()
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/"+pkg, fset)
	if err != nil {
		t.Fatalf("%s: LoadPackages: %v", pkg, err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("%s: callgraph.Build: %v", pkg, err)
	}
	return Compute(result, cg)
}

// TestTheUnderlyingMapOrderActuallyVaries is the vacuity control, and without
// it every assertion below is worthless.
//
// Sorting a collection that only ever arrives in one order proves nothing. This
// asserts the premise the fix exists for: ranging the raw map really does yield
// different sequences within a single process. If this ever fails, the ordering
// tests below have stopped being able to distinguish a sorted implementation
// from an unsorted one, and they will pass either way.
func TestTheUnderlyingMapOrderActuallyVaries(t *testing.T) {
	cr := computeVetPkg(t, "errors")
	if len(cr.Errors) < 2 {
		t.Fatalf("testdata/errors yields %d errored functions; need at least 2 for map "+
			"order to be observable at all", len(cr.Errors))
	}

	rawOrder := func() []string {
		var names []string
		for funcName := range cr.Errors {
			names = append(names, funcName)
		}
		return names
	}

	first := rawOrder()
	varied := false
	for i := 0; i < 200 && !varied; i++ {
		next := rawOrder()
		for j := range first {
			if first[j] != next[j] {
				varied = true
				break
			}
		}
	}
	if !varied {
		t.Errorf("ranging cr.Errors gave the same order 200 times over %d entries. "+
			"Go randomises map iteration, so this is unexpected -- and it means the "+
			"ordering tests below cannot fail for an unsorted implementation.",
			len(cr.Errors))
	}
}

func TestSortedErrorsAreStableAcrossFunctions(t *testing.T) {
	cr := computeVetPkg(t, "errors")
	first := cr.SortedErrors()

	// Across at least two FUNCTIONS, which is where map order lives. A package
	// whose diagnostics all sit in one function would be sorted by line alone
	// and could not distinguish this fix from its absence.
	seen := map[string]bool{}
	for _, d := range first {
		seen[d.FuncName] = true
	}
	if len(seen) < 2 {
		t.Fatalf("testdata/errors produced diagnostics in %d function(s); need at least 2", len(seen))
	}

	for i := 0; i < 200; i++ {
		got := cr.SortedErrors()
		if len(got) != len(first) {
			t.Fatalf("call %d returned %d errors, first returned %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("call %d differs at index %d:\n  first: %+v\n  got:   %+v",
					i, j, first[j], got[j])
			}
		}
	}
}

// TestDiagnosticsWithinOneFunctionAreOrderedByLine covers a DIFFERENT property
// from the tests above, and the distinction is worth stating because the naming
// invites confusion.
//
// helper_escape reports all eight of its errors in one function, so its map has
// a single key and iteration order cannot vary. This test therefore CANNOT fail
// for cleat#965 -- disabling sortDiagnostics entirely leaves it green, which was
// measured rather than assumed. What it does check is that the eight findings
// come back ascending by line, which is what makes a multi-violation function
// readable.
//
// TestSortedErrorsAreStableAcrossFunctions is the one that detects the map-order
// bug. Keeping both, clearly labelled, beats keeping one that looks like two.
func TestDiagnosticsWithinOneFunctionAreOrderedByLine(t *testing.T) {
	cr := computeVetPkg(t, "vet-checks/go/helper_escape")
	got := cr.SortedErrors()
	if len(got) < 2 {
		t.Fatalf("helper_escape produced %d errors; need at least 2 to have an order", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Line < got[i-1].Line {
			t.Errorf("index %d goes backwards: line %d after line %d\n  %+v",
				i, got[i].Line, got[i-1].Line, got[i])
		}
	}
}

// TestSortedDiagnosticsUseTheDocumentedKey pins the order itself, not merely
// that it is stable. A stable but arbitrary order would satisfy the tests above
// and still be unreviewable -- diagnostics for one function scattered through
// the output.
func TestSortedDiagnosticsUseTheDocumentedKey(t *testing.T) {
	cr := computeVetPkg(t, "errors")
	got := cr.SortedErrors()
	if len(got) < 2 {
		t.Fatalf("testdata/errors produced %d diagnostics; with fewer than 2 this "+
			"assertion holds for any implementation", len(got))
	}

	want := append([]Diagnostic(nil), got...)
	sort.SliceStable(want, func(i, j int) bool {
		a, b := want[i], want[j]
		switch {
		case a.FuncName != b.FuncName:
			return a.FuncName < b.FuncName
		case a.Line != b.Line:
			return a.Line < b.Line
		case a.Code != b.Code:
			return a.Code < b.Code
		default:
			return a.Message < b.Message
		}
	})
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("not ordered by (FuncName, Line, Code, Message) at index %d:\n"+
				"  got:  %+v\n  want: %+v", i, got[i], want[i])
		}
	}
}

// TestSortDiagnosticsIsATotalOrder guards the tie-break chain. Two findings
// equal on every key would be free to swap between runs, so the sort would be
// stable only by luck. One function reporting the same code twice on one line
// is real -- e013_sync_mutex does it for Lock and Unlock.
func TestSortDiagnosticsIsATotalOrder(t *testing.T) {
	in := []Diagnostic{
		{FuncName: "f", Line: 2, Code: "E013", Message: "b"},
		{FuncName: "f", Line: 2, Code: "E013", Message: "a"},
		{FuncName: "f", Line: 1, Code: "E002", Message: "z"},
		{FuncName: "a", Line: 9, Code: "E001", Message: "q"},
	}
	sortDiagnostics(in)
	want := []Diagnostic{
		{FuncName: "a", Line: 9, Code: "E001", Message: "q"},
		{FuncName: "f", Line: 1, Code: "E002", Message: "z"},
		{FuncName: "f", Line: 2, Code: "E013", Message: "a"},
		{FuncName: "f", Line: 2, Code: "E013", Message: "b"},
	}
	for i := range want {
		if in[i] != want[i] {
			t.Errorf("index %d: got %+v, want %+v", i, in[i], want[i])
		}
	}
}
