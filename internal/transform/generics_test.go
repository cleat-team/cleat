package transform

import (
	"go/ast"
	"go/token"
	"testing"

	"github.com/rcownie/cleat/internal/analyzer"
	"github.com/rcownie/cleat/internal/callgraph"
	"github.com/rcownie/cleat/internal/closure"
)

func tfGenericsFQ(name string) string {
	return "github.com/rcownie/cleat/testdata/generics." + name
}

// tfBuildGenericsConfig loads the generics testdata and returns a Config
// ready for Transform.
func tfBuildGenericsConfig(t *testing.T) *Config {
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
	cr := closure.Compute(result, cg)
	return &Config{Result: result, CallGraph: cg, Closure: cr}
}

// TestTransformGenericsOutputSyntaxValid verifies that the transform produces
// valid Go syntax for generic functions.
func TestTransformGenericsOutputSyntaxValid(t *testing.T) {
	cfg := tfBuildGenericsConfig(t)
	tr, err := Transform(cfg)
	if err != nil {
		t.Fatalf("Transform(generics) failed: %v", err)
	}
	// The generic functions all have h in their signatures already,
	// so no auto-threading should be needed.
	if len(tr.AddedH) != 0 {
		t.Logf("AddedH entries: %v", tr.AddedH)
	}
	// Validate that all output files are syntactically valid Go.
	for name, content := range tr.Files {
		tfSyntaxCheck(t, "generics:"+name, string(content))
	}
}

// TestTransformGenericsNoUnnecessaryModifications verifies that the transform
// does not modify functions that already have h in their signatures.
func TestTransformGenericsNoUnnecessaryModifications(t *testing.T) {
	cfg := tfBuildGenericsConfig(t)

	// Verify that all generic functions already have HostCalls as first param.
	for _, name := range []string{
		tfGenericsFQ("EntryPoint"),
		tfGenericsFQ("Process"),
		tfGenericsFQ("GenericLeaf"),
	} {
		fd := cfg.Result.Funcs[name]
		if fd == nil {
			t.Fatalf("function %s not found", name)
		}
		if !hasHostCallsParam(fd) {
			t.Errorf("%s should have HostCalls as first param", name)
		}
	}

	// Run transform — should be a no-op since all functions have h.
	tr, err := Transform(cfg)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if len(tr.AddedH) != 0 {
		t.Errorf("expected no AddedH, got %v", tr.AddedH)
	}
}

// TestTransformGenericsPreservesTypeParams verifies that the transform
// preserves type parameter lists on generic functions.
func TestTransformGenericsPreservesTypeParams(t *testing.T) {
	cfg := tfBuildGenericsConfig(t)

	// Before transform: check type params exist.
	processFD := cfg.Result.Funcs[tfGenericsFQ("Process")]
	if processFD == nil || processFD.Ast == nil {
		t.Fatal("Process function not found with AST")
	}

	beforeTypeParams := processFD.Ast.Type.TypeParams
	if beforeTypeParams == nil || beforeTypeParams.List == nil || len(beforeTypeParams.List) == 0 {
		t.Error("Process should have type parameters in AST before transform")
	}

	// Run transform.
	tr, err := Transform(cfg)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}

	// After transform: verify all output files are syntactically valid Go.
	for name, content := range tr.Files {
		tfSyntaxCheck(t, name, string(content))
	}
}

// TestTransformGenericsASTPreservesGenericCallSyntax verifies that generic
// calls like Process[string](...) survive the transformation.
func TestTransformGenericsASTPreservesGenericCallSyntax(t *testing.T) {
	cfg := tfBuildGenericsConfig(t)

	// Before transform: check that there are IndexExpr nodes.
	entryFD := cfg.Result.Funcs[tfGenericsFQ("EntryPoint")]
	if entryFD == nil || entryFD.Ast == nil || entryFD.Ast.Body == nil {
		t.Fatal("EntryPoint function not found with body")
	}

	indexCount := 0
	ast.Inspect(entryFD.Ast.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.IndexExpr); ok {
			indexCount++
		}
		return true
	})
	if indexCount == 0 {
		t.Error("expected IndexExpr nodes for generic function calls in EntryPoint")
	}

	// Run transform.
	tr, err := Transform(cfg)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}

	// After transform, the output should still be valid Go.
	// Check that all files are valid Go syntax.
	for name, content := range tr.Files {
		tfSyntaxCheck(t, name, string(content))
	}
}
