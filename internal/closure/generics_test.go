package closure

import (
	"go/token"
	"testing"

	"github.com/rcownie/cleat/internal/analyzer"
	"github.com/rcownie/cleat/internal/callgraph"
)

func closureGenericsFQ(name string) string {
	return "github.com/rcownie/cleat/testdata/generics." + name
}

func closureLoadGenerics(t *testing.T) (*analyzer.AnalysisResult, *Result) {
	t.Helper()
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/cleat/testdata/generics", fset)
	if err != nil {
		t.Fatalf("LoadPackages(generics) failed: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}
	cr := Compute(result, cg)
	return result, cr
}

// TestComputeGenericsDurableLeaves verifies that generic functions calling
// HostCalls methods are identified as durable leaves.
func TestComputeGenericsDurableLeaves(t *testing.T) {
	_, cr := closureLoadGenerics(t)

	expectedLeaves := []string{
		closureGenericsFQ("GenericLeaf"),
		closureGenericsFQ("Process"),
		"*Container[T].Process",
	}

	for _, name := range expectedLeaves {
		if !cr.DurableLeaves[name] {
			t.Errorf("expected %s to be a durable leaf", analyzer.ShortName(name))
		}
	}
}

// TestComputeGenericsClosure verifies that the entry point is identified
// as part of the durable closure (transitively reaches durable leaves).
func TestComputeGenericsClosure(t *testing.T) {
	_, cr := closureLoadGenerics(t)

	if !cr.DurableClosure[closureGenericsFQ("EntryPoint")] {
		t.Error("expected EntryPoint to be in durable closure")
	}
}

// TestComputeGenericsNoErrors verifies that generic functions don't produce
// false positive validation errors.
func TestComputeGenericsNoErrors(t *testing.T) {
	_, cr := closureLoadGenerics(t)

	// Generic functions should not produce validation errors or warnings.
	for name, errs := range cr.Errors {
		if len(errs) > 0 {
			t.Errorf("unexpected errors for %s: %v", analyzer.ShortName(name), errs)
		}
	}
}

// TestComputeGenericsTagsConsistent verifies that FuncDecl tags match the
// closure computation results.
func TestComputeGenericsTagsConsistent(t *testing.T) {
	result, cr := closureLoadGenerics(t)

	for name := range cr.DurableLeaves {
		fd := result.Funcs[name]
		if fd == nil {
			t.Errorf("leaf %s not found in Funcs", analyzer.ShortName(name))
			continue
		}
		if !fd.IsDurableLeaf {
			t.Errorf("leaf %s has IsDurableLeaf=false", analyzer.ShortName(name))
		}
		if fd.DurabilityTag != "DurableLeaf" {
			t.Errorf("leaf %s has DurabilityTag=%q", analyzer.ShortName(name), fd.DurabilityTag)
		}
	}

	for name := range cr.DurableClosure {
		fd := result.Funcs[name]
		if fd == nil {
			t.Errorf("closure %s not found in Funcs", analyzer.ShortName(name))
			continue
		}
		if !fd.InDurableClosure {
			t.Errorf("closure %s has InDurableClosure=false", analyzer.ShortName(name))
		}
		if fd.DurabilityTag != "DurableClosure" {
			t.Errorf("closure %s has DurabilityTag=%q", analyzer.ShortName(name), fd.DurabilityTag)
		}
	}
}

// TestComputeGenericsPartitionComplete verifies that every function is
// categorized (leaf + closure + pure = total).
func TestComputeGenericsPartitionComplete(t *testing.T) {
	result, cr := closureLoadGenerics(t)

	totalFuncs := len(result.Funcs)
	durableCount := len(cr.DurableLeaves) + len(cr.DurableClosure)
	pureCount := len(cr.Pure)

	if durableCount+pureCount != totalFuncs {
		t.Errorf(
			"durable (%d) + pure (%d) = %d, expected %d",
			durableCount, pureCount, durableCount+pureCount, totalFuncs,
		)
	}
}
