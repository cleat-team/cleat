package analyzer

import (
	"go/token"
	"testing"
)

// genericsFQ returns a fully-qualified name for a function in testdata/generics.
func genericsFQ(name string) string {
	return "github.com/cleat-team/cleat/testdata/generics." + name
}

// genericsFQMethod returns a fully-qualified name for a method on a generic type.
func genericsFQMethod(typeName, methodName string) string {
	return "*" + typeName + "[T]." + methodName
}

func genericsLoad(t *testing.T) *AnalysisResult {
	t.Helper()
	fset := token.NewFileSet()
	result, err := LoadPackages("github.com/cleat-team/cleat/testdata/generics", fset)
	if err != nil {
		t.Fatalf("LoadPackages(generics) failed: %v", err)
	}
	return result
}

// TestGenericsFunctionsFound verifies that generic functions are detected
// by the analyzer with their type parameters.
func TestGenericsFunctionsFound(t *testing.T) {
	result := genericsLoad(t)

	// Check that the generic function Process is found.
	fd := result.Funcs[genericsFQ("Process")]
	if fd == nil {
		t.Fatal("Process function not found in Funcs")
	}

	// Check that type parameters are resolved.
	if fd.Type == nil {
		t.Fatal("Process has no type signature")
	}
	tparams := fd.Type.TypeParams()
	if tparams == nil || tparams.Len() == 0 {
		t.Error("Process should have type parameters, got none")
	} else {
		t.Logf("Process has %d type parameter(s)", tparams.Len())
	}

	// Check that GenericLeaf is found.
	fd = result.Funcs[genericsFQ("GenericLeaf")]
	if fd == nil {
		t.Fatal("GenericLeaf function not found in Funcs")
	}
	if fd.Type == nil {
		t.Fatal("GenericLeaf has no type signature")
	}
	tparams = fd.Type.TypeParams()
	if tparams == nil || tparams.Len() == 0 {
		t.Error("GenericLeaf should have type parameters, got none")
	}

	// Check that the container method is found.
	methodFQ := genericsFQMethod("Container", "Process")
	fd = result.Funcs[methodFQ]
	if fd == nil {
		t.Fatal("*Container[T].Process method not found in Funcs")
	}

	// Check that EntryPoint is found and is an entry point.
	fd = result.Funcs[genericsFQ("EntryPoint")]
	if fd == nil {
		t.Fatal("EntryPoint function not found in Funcs")
	}
	if !fd.IsEntryPoint {
		t.Error("EntryPoint should be marked as entry point")
	}
	if !fd.IsExported {
		t.Error("EntryPoint should be exported")
	}
}
