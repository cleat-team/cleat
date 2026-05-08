package callgraph

import (
	"go/token"
	"testing"

	"github.com/rcownie/cleat/internal/analyzer"
)

func cgGenericsFQ(name string) string {
	return "github.com/rcownie/cleat/testdata/generics." + name
}

func cgLoadGenerics(t *testing.T) (*analyzer.AnalysisResult, *Graph) {
	t.Helper()
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/cleat/testdata/generics", fset)
	if err != nil {
		t.Fatalf("LoadPackages(generics) failed: %v", err)
	}
	g, err := Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}
	return result, g
}

// TestBuildGenericsHasEdges verifies that generic function calls produce
// edges in the call graph, including:
//   - Direct generic function instantiation: Func[T](args)
//   - Generic method calls: obj.Method(args)
//   - Generic durable leaf calls
func TestBuildGenericsHasEdges(t *testing.T) {
	_, g := cgLoadGenerics(t)

	// Edge from entry point to generic function (via Func[T](args) syntax).
	if !g.Calls[cgGenericsFQ("EntryPoint")][cgGenericsFQ("Process")] {
		t.Error("expected edge EntryPoint -> Process (generic function instantiation)")
	}

	// Edge from entry point to generic leaf.
	if !g.Calls[cgGenericsFQ("EntryPoint")][cgGenericsFQ("GenericLeaf")] {
		t.Error("expected edge EntryPoint -> GenericLeaf (generic leaf)")
	}

	// Edge from entry point to generic method on Container.
	if !g.Calls[cgGenericsFQ("EntryPoint")]["*Container[T].Process"] {
		t.Error("expected edge EntryPoint -> *Container[T].Process (generic method)")
	}
}

// TestBuildGenericsDurableLeaves verifies that generic functions that call
// HostCalls methods are correctly identified as durable leaves.
func TestBuildGenericsDurableLeaves(t *testing.T) {
	_, g := cgLoadGenerics(t)

	// GenericLeaf calls h.DurableLog directly, so it should be a leaf.
	if !g.DurableLeaves[cgGenericsFQ("GenericLeaf")] {
		t.Error("GenericLeaf should be a durable leaf")
	}

	// Process calls h.DurableLog directly, so it should be a leaf.
	if !g.DurableLeaves[cgGenericsFQ("Process")] {
		t.Error("Process should be a durable leaf")
	}

	// *Container[T].Process calls h.DurableLog directly, so it should be a leaf.
	if !g.DurableLeaves["*Container[T].Process"] {
		t.Error("*Container[T].Process should be a durable leaf")
	}

	// EntryPoint does NOT directly call HostCalls, so it should NOT be a leaf.
	if g.DurableLeaves[cgGenericsFQ("EntryPoint")] {
		t.Error("EntryPoint should NOT be a durable leaf (it delegates)")
	}
}

// TestBuildGenericsAllFunctions verifies that all functions in the generics
// testdata have entries in the call graph and that functions called by
// others have CalledBy entries.
func TestBuildGenericsAllFunctions(t *testing.T) {
	result, g := cgLoadGenerics(t)

	// Every function should have a Calls entry (initialized by Build).
	for _, name := range result.Funcs {
		fq := name.FullyQualifiedName()
		if _, ok := g.Calls[fq]; !ok {
			t.Errorf("no Calls entry for %s", analyzer.ShortName(fq))
		}
	}

	// Functions that are called by others should have CalledBy entries.
	for _, name := range []string{
		cgGenericsFQ("Process"),
		cgGenericsFQ("GenericLeaf"),
		"*Container[T].Process",
	} {
		if _, ok := g.CalledBy[name]; !ok {
			t.Errorf("no CalledBy entry for %s", analyzer.ShortName(name))
		}
	}

	if len(g.Calls) != len(result.Funcs) {
		t.Errorf("Calls entries=%d, Funcs=%d", len(g.Calls), len(result.Funcs))
	}
}

// TestBuildGenericsCalledBy verifies consistency of Calls and CalledBy.
func TestBuildGenericsCalledByConsistency(t *testing.T) {
	_, g := cgLoadGenerics(t)
	for caller, callees := range g.Calls {
		for callee := range callees {
			if !g.CalledBy[callee][caller] {
				t.Errorf("CalledBy missing edge %s <- %s",
					analyzer.ShortName(callee), analyzer.ShortName(caller))
			}
		}
	}
}
